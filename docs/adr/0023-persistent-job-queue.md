# ADR 0023: Persistent database-backed job queue

Status: accepted; amended by [ADR 0047](0047-batch-claims-notify-and-recorded-ticks.md) (batch claims, LISTEN/NOTIFY wake-up, recovery off the claim path)

## Context

ADR 0020 gave Forge an in-process scheduler with no persistence: a job that is
enqueued and then lost in a crash, or enqueued by a transaction that later
rolls back, is simply gone or, worse, runs for state that never existed. ADR
0019 solved delivery for outbox events on PostgreSQL only. Applications need a
general queue (send the email, build the report, call the partner API) that
works on every database Forge supports, is atomic with the business write
that causes it, and is as easy to operate as Rails 8's Active Job with Solid
Queue. The bar is parity with that, and better where safety is concerned.

## Decision

### Package layout

- `queue` (core, standard library plus core packages only): `Registry` and
  typed `Register[T]`, `Prepare`/`Enqueue`, `Worker`, the `Store` port, retry
  policy, error sanitizing, hooks.
- `queue/queuecmd` (core): the `jobs` application subcommand.
- `queue/storetest` (test support): the conformance suite every adapter passes
  against a real database.
- `queue/postgres` (pgx), `queue/mysql` (MySQL 8.4 and MariaDB 11.8) and
  `queue/sqlite`: a `Store`, embedded migrations and `Commands()`.

### Typed payloads

`Register[T](registry, kind, func(ctx, T) error, opts...)` fixes the payload
type of a kind. `Prepare[T]` rejects a payload whose Go type differs from the
registered one, encodes it as JSON, and verifies that the strict decoder
accepts it, so a job that could never run is refused where the programmer can
fix it. The worker decodes with `DisallowUnknownFields` and rejects trailing
data. A payload that no longer decodes (schema drift between deploys) fails
permanently on its first attempt instead of burning every retry. Kind and
queue names match `^[a-z][a-z0-9_.-]{0,63}$` (enforced in Go and by database
constraints); payloads are at most 1 MiB.

### Transactional enqueue

`queue.Enqueue(ctx, inserter, registry, kind, payload, opts...)` takes an
`Inserter`. A store is one (an independent single-statement write); each
adapter's `Store.In(tx)` returns an `Inserter` bound to the caller's
transaction (`pgx.Tx`, or `*sql.Tx` through `sqldb.DBTX`). The adapter opens
no second transaction and commits nothing, so the job exists if and only if
the business transaction commits, and workers cannot see it before. Duplicate
idempotency keys are handled without raising a database error (`ON CONFLICT DO
NOTHING` / `ON DUPLICATE KEY UPDATE id = id`), so a duplicate never aborts the
caller's PostgreSQL transaction.

MySQL and MariaDB look for the holder of a key with a plain, non-locking read
before inserting, so a duplicate enqueue inside a long business transaction
takes no lock on the running job (a locking read or `ON DUPLICATE KEY UPDATE`
would block the worker's heartbeat and acknowledgement until the business
transaction ended). Only when two enqueues of one key race does the losing
INSERT leave a shared lock on the duplicate unique-index entry until its
transaction ends; it can delay the holder's Complete or Fail, never its
heartbeat.

PostgreSQL under REPEATABLE READ or SERIALIZABLE reports a concurrent enqueue
of the same key as a serialization failure (SQLSTATE 40001), as it does for any
conflicting write; run such transactions with `postgres.TxOptions.Attempts` so
the retry sees the committed holder and reports a duplicate. At READ COMMITTED
one caller inserts and the others get duplicates, with no retry.

### Claiming, leases and fencing

- Claim = one job, one short statement or transaction, ended before the
  handler runs: PostgreSQL `UPDATE ... FROM (SELECT ... FOR UPDATE SKIP
  LOCKED LIMIT 1)` in a single autocommit statement; MySQL and MariaDB a
  READ COMMITTED transaction with `SELECT ... FOR UPDATE SKIP LOCKED` then
  `UPDATE` (READ COMMITTED avoids gap locks that would block enqueues);
  SQLite one `UPDATE ... RETURNING` under its single-writer lock.
- Lease timestamps come from the database clock on PostgreSQL and MySQL/
  MariaDB, so host clock skew cannot expire a lease early. SQLite uses the
  application clock, which is the only clock when all workers share a host.
- Each claim gets a UUIDv7 lease token. Renew, Complete, Fail and Release all
  require `id AND lease_token AND state = 'running'`; zero affected rows is
  `ErrLeaseLost`. A stale worker cannot overwrite a newer claim.
- Expired leases are recovered by `Store.Recover` (a job whose lease expired
  becomes pending again, or dead when it already used its last attempt). Two
  things call it: `Claim` (throttled to one sweep per 250 ms per store) and
  each worker's own recovery loop, which runs every `Lease/4` (between 100 ms
  and 5 s) whether or not the worker has a free slot, so a saturated worker
  still recovers a dead peer's jobs. A crashed worker's jobs become pending
  at most `Lease` plus that interval (at most 5 s) after its last renewal,
  provided at least one worker is alive; with no live worker nothing recovers
  them until one starts.
- A running handler's lease is renewed every `Lease/3`. The worker keeps a
  deadline of `Lease - Lease/10` after the start of the last successful claim
  or renewal; every renewal is capped to the time left and, when the deadline
  passes without a successful renewal, the handler's context is canceled with
  `ErrLeaseLost` while the store's lease has not yet expired. If the store
  says the lease is gone the context is canceled and the result discarded. In
  every case the outcome is still written fenced.
- Workers claim only kinds in their registry, so a job enqueued by a newer
  deploy waits for a worker that knows it instead of failing on an old one.

### Retries and dead jobs

- A job runs at most `MaxAttempts` times: default 10 (per kind through
  `HandlerMaxAttempts`, per job through `WithMaxAttempts`, at most 1000).
  Backoff doubles from 5 s to a 1 h ceiling with equal jitter (a uniform draw
  from the upper half of the ceiling); `HandlerBackoff` overrides per kind.
  `queue.Permanent(err)` dead-letters at once; `queue.RetryAfter(d, err)`
  overrides the delay.
- Panics are recovered and count as failures. Only the panic's type is
  stored; the stack goes to the log.
- Exhausted jobs enter the `dead` state and stay until an operator retries
  (attempts reset, refused with `ErrKeyInUse` if a newer active job holds the
  idempotency key) or discards (deleted with its payload) them.
- Succeeded jobs are kept for `RetainSucceeded` (default 24 h) and pruned by
  the worker's maintenance loop and by `jobs prune`.

### Error text and privacy

Payloads are never logged, never passed to hooks and never written to an
error column. The stored `last_error` is `"<root error type>: <message>"`
where the message has payload string values, URL credentials, `key=value`
secret assignments and bearer tokens replaced by `[redacted]`, control
characters (C0 and C1, including the 8-bit CSI), bidirectional overrides and
isolates, and line separators removed, and the result cut to 1024 bytes on a
rune boundary. Payload strings of any length are redacted whole, and any run
of 32 bytes or more copied from a payload string over 48 bytes is redacted too;
when a payload holds more than 8192 distinct strings the shortest are not
matched. An error whose `Error` method panics is stored as
`<type>: error text unavailable`. The `jobs` command escapes the same
characters when printing, so stored text cannot drive the operator's terminal.
This is defense in depth, not a proof: handlers should not echo their input
in errors. Payloads must not contain secrets; they are stored in clear text.

### Idempotency keys

`WithKey(k)` makes enqueueing idempotent per kind while a job with that key
is pending or running: PostgreSQL and SQLite use a partial unique index; MySQL
and MariaDB, which lack partial indexes, use a stored generated column
`active_key` (the key while pending/running, else NULL) under a unique index.
The key is free again once the job succeeds or dies.

### Lifecycle

`Worker.Start(ctx)` launches one poller per queue, each bounded by its queue's
concurrency (a slot semaphore, so there is one claim query per free slot and
one idle poll per interval, with jittered exponential idle backoff drawn
from [interval/2, interval] and doubling from 1 s up to 10 s). Canceling the
start context (SIGTERM) stops claiming without interrupting running jobs;
no claim starts once shutdown began. `Worker.Shutdown(ctx)`, whose signature
matches `forge.ShutdownHook`, waits for in-flight jobs. At the deadline it
cancels the handlers and waits up to `ReleaseTimeout` (5 s) for them to return;
each handler that returns after cancellation gives its job back to the queue
**without counting the attempt** (or acknowledges it, if the work finished). It
then waits up to another `ReleaseTimeout` for acknowledgements already being
written, so the caller can close the pool.

The guarantee has a limit: a handler that ignores its canceled context and is
still running after that is **abandoned, not released**. Releasing it would let
another worker run the job concurrently with the original handler. Its lease
is no longer renewed, so the job becomes available after the lease expires
(at most `Lease` later), Shutdown returns an error naming how many handlers
are still running, and those handlers may still touch the database; handlers
must honor their context. Register Shutdown with `App.OnShutdown` after the
database hook (hooks run in reverse), within the HTTP shutdown budget.

### Operations

`jobs status|dead|show|retry|discard|prune` (package `queuecmd`) ships in the
application binary. Listings never show payloads. `Hooks` (Started, Finished
with outcome, duration and the sanitized error) let applications bridge to
logs, metrics or tracing; the core imports no telemetry library.

### Storage

UUIDv7 ids: `uuid` (PostgreSQL), `CHAR(36)` ascii (MySQL), `TEXT` (SQLite
STRICT). Payloads: `jsonb` on PostgreSQL (its size CHECK is twice the application limit, because
jsonb's text form adds spaces); `MEDIUMTEXT` with `JSON_VALID`
on MySQL/MariaDB (the JSON type normalizes numbers and key order, and is a
`LONGTEXT` alias on MariaDB); `TEXT` with `json_valid` on SQLite. Claiming
indexes cover `(queue, priority, run_at, id)` for pending jobs. Migrations
are irreversible by design (dropping the table would destroy pending work),
like the outbox's.

## Consequences

- Delivery is at-least-once. Handlers must be idempotent. A lease can expire
  under a live handler (pause, partition); the stale result is discarded by
  the fence but its side effects have happened.
- No exactly-once promise, no cross-queue fairness, no per-job rate limits
  and no cron (the in-process `jobs` scheduler remains; it can enqueue into
  this queue with a key per tick for cluster-wide once-per-tick).
- Polling, not LISTEN/NOTIFY: a job waits up to `MaxPollInterval` (10 s by
  default) on an idle queue, 1 s while the queue is busy. Push wake-ups are
  future work.
- SQLite is for single-host deployments; MySQL/MariaDB and PostgreSQL scale
  to many worker hosts. Workers are trusted and global: the queue is not
  tenant-scoped; payloads that concern a tenant carry the tenant ID and the
  handler enforces authorization.
- The `kind IN (...)` claim filter walks past pending jobs of kinds a worker
  cannot run; deployments that run specialized workers should give those kinds
  their own queue.
- Tables grow with retained jobs; `RetainSucceeded` and `jobs prune` bound
  succeeded rows, and dead rows are kept until an operator acts.
