# ADR 0019: PostgreSQL outbox dispatch

Status: accepted; amended by [ADR 0047](0047-batch-claims-notify-and-recorded-ticks.md)
(events are claimed in batches, each still acknowledged on its own)

## Context

The transactional outbox foundation made event persistence atomic with domain
writes but did not define delivery. Dispatch must work across process replicas
without holding database locks during external I/O, and must make duplicate
delivery and poison events explicit.

## Decision

- Claim one eligible row at a time using PostgreSQL `FOR UPDATE SKIP LOCKED`
  and an atomic update. The database transaction ends before the handler runs.
- Bound in-process worker concurrency, use database-clock lease timestamps,
  renew active leases, and fence acknowledgement/retry by a per-claim UUID
  token. A stale worker cannot mutate a row after it has been reclaimed.
- Deliver with at-least-once semantics. A process can fail after a handler's
  external side effect but before acknowledgement; consumers must be
  idempotent. Exactly-once external effects are not promised.
- Shutdown (canceling `Run`'s context) interrupts handlers and idle waits,
  never the dispatcher's own statements (claim, acknowledgement, retry,
  release, renewal). They run detached from the cancellation, each bounded to
  5 seconds, so an event whose handler returned is recorded (acknowledged,
  retried, or dead-lettered on its last attempt) unless that statement itself
  fails (a database error, the 5-second bound, or a lease lost because a slow
  handler outlived renewal, which stops at shutdown); only then is it
  delivered again when its lease expires. A statement canceled while it runs
  can also be applied without the worker learning it, and pgx can return its
  connection to the pool with a stale read-deadline error.
- Retry handler failures using capped exponential full jitter. After the
  configured maximum attempts, retain the event in a terminal dead-letter
  state; do not delete it or persist arbitrary handler error text, which may
  contain sensitive data.
- Treat this as a trusted global worker: the outbox is not tenant-scoped.
  Applications handling tenant data must include the necessary tenant
  identity in their application-owned payload and enforce authorization in
  the handler. Payloads must not contain secrets or unnecessary personal data.
- Keep the migrations irreversible to protect durable event records. Manual
  dead-letter replay and queue observability are follow-up work.

## Consequences

- Multiple workers and replicas can claim different events without a global
  advisory lock.
- A handler slower than the initial lease can continue safely while its lease
  renews, provided it honors the application context. If the database becomes
  unavailable long enough to lose the lease, delivery can repeat.
- Terminal rows require operator visibility and an explicit replay policy;
  consumers must not assume the framework automatically retries them.
- The global-worker trust boundary is unsuitable for applications whose
  database policy requires tenant-isolated worker access without additional
  application-owned enforcement.
