# ADR 0047: Batch claims, LISTEN/NOTIFY wake-up and per-tick scheduler locks

- **Status:** Accepted
- **Date:** 2026-10-26
- **Amends:** [ADR 0019](0019-outbox-dispatch.md) (one event per claim),
  [ADR 0020](0020-background-jobs-scheduling.md) (every replica runs every
  tick), [ADR 0023](0023-persistent-job-queue.md) (one job per claim, lease
  recovery during claims)
- **Parity with:** Forge-Rust `forge-jobs` (claims up to the free slots of a
  queue) and `forge-events` (`DispatcherSettings::batch`); see Consequences

## Context

The 2026-10-24 audit (PF-1, PF-2, PF-5, PF-7) measured the job queue and the
outbox paying one round trip, and for the outbox one whole transaction, per
claimed row; idle queue pollers backing off to 10 seconds, so a new job could
wait that long; lease recovery running inside claims; and every replica
running every scheduled tick.

## Decision

- **Queue batches.** `queue.BatchClaimer.ClaimBatch` leases up to `n` jobs in
  one statement (`LIMIT $n ... FOR UPDATE SKIP LOCKED` on PostgreSQL, MySQL and
  MariaDB, one `UPDATE ... RETURNING` on SQLite) under one lease token; fencing
  stays per job (job ID and token). `WorkerOptions.BatchSize` defaults to 10,
  and a claim never asks for more than the queue's free slots, so each claimed
  job starts at once and leases, attempts and fairness are unchanged.
- **Recovery off the claim path.** `Store.Claim` no longer recovers expired
  leases; the worker's own loop (every `Lease/4`, 100 jobs per statement) is
  the only sweep.
- **Wake-up.** `queuepostgres.WithNotify` adds `pg_notify('forge_jobs', queue)`
  to the enqueue statement for due jobs (delivered at commit) and makes the
  store a `queue.Listener`. A worker on a listener holds one `LISTEN`
  connection in its own lifecycle (started by `Start`, canceled and awaited
  by `Shutdown`, reconnected with 1s to 30s backoff), and a notification ends
  the idle wait of that queue's poller (coalesced, at least 10ms apart). On
  every (re)subscription each queue claims once. Polling remains, so
  notifications are hints, never needed for correctness. It is opt-in because
  of its costs: a connection per worker, no PgBouncer transaction pooling, and
  serialized commits of notifying transactions.
- **Outbox batches.** A dispatcher worker claims up to
  `DispatcherOptions.BatchSize` (default 10) events in one transaction and
  delivers them one after another in claim order, acknowledging each on its
  own. One renewal per batch extends the leases of every event still held, so
  waiting events are never reclaimed; an event reclaimed anyway is skipped;
  unstarted events are released without using an attempt on shutdown or
  failure. A failing publish affects only its own event.
- **Scheduler locks.** `jobs.SchedulerOptions.Lock` (`jobs.Lock`): a replica
  runs a tick only after taking that tick's lock, keyed on the job name and the
  tick time, and holds it `LockWindow` (default 5s, at most half the interval)
  after the tick so replicas with lagging clocks find it taken. Ticks reached
  later than the window, or whose lock cannot be checked, are skipped and
  reported (at most once per tick). PostgreSQL uses
  `pg_try_advisory_xact_lock` in a transaction rolled back on unlock; MySQL and
  MariaDB use `GET_LOCK(name, 0)` on a dedicated connection. Generated
  applications pass the database's lock.

## Consequences

- Fewer round trips per job and event; idle-to-claim latency drops from up to
  `MaxPollInterval` to milliseconds with `WithNotify`.
- Custom `queue.Store` implementations keep working: batching and listening
  are optional interfaces. Callers that drove `Store.Claim` directly must call
  `Store.Recover` themselves.
- A scheduler with a lock trades "every replica runs" for "at most one replica
  runs", so a tick can be skipped when the database is unreachable or a replica
  lags more than the window; jobs should tolerate a missed tick.
- Forge-Rust differs: it records ticks in a table (exactly once, with a
  lateness bound), has no `LISTEN/NOTIFY` wake-up and no batch size knob for
  jobs, and its outbox default batch is 50 with a lease covering the batch.
  Follow-ups are tracked in the parity matrix.
