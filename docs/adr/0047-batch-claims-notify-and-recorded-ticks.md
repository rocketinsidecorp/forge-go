# ADR 0047: Batch claims, LISTEN/NOTIFY wake-up and recorded scheduler ticks

- **Status:** Accepted
- **Date:** 2026-10-26
- **Amends:** [ADR 0019](0019-outbox-dispatch.md) (one event per claim),
  [ADR 0020](0020-background-jobs-scheduling.md) (every replica runs every
  tick), [ADR 0023](0023-persistent-job-queue.md) (one job per claim, lease
  recovery during claims)
- **Parity with:** Forge-Rust `forge-jobs` (claims up to the free slots of a
  queue; `forge_job_schedules` and `forge_jobs_fire_tick`) and `forge-events`
  (`DispatcherSettings::batch`); follow-ups in
  [forge-rust#80](https://github.com/rocketinsidecorp/forge-rust/issues/80)

## Context

The 2026-10-24 audit (PF-1, PF-2, PF-5, PF-7) measured the job queue and the
outbox paying one round trip, and for the outbox one whole transaction, per
claimed row; idle queue pollers backing off to 10 seconds, so a new job could
wait that long; lease recovery running inside claims; and every replica
running every scheduled tick.

A first version of the scheduler change took a database lock per tick (an
advisory lock in an open transaction on PostgreSQL, `GET_LOCK` on a dedicated
session on MySQL) and held it while the job ran and for a window after the
tick. Review found that it held one pooled connection per running tick: jobs
aligned on the same ticks then starved the pool they and the HTTP handlers
needed. It was replaced before release by recorded ticks, below.

## Decision

- **Queue batches.** `queue.BatchClaimer.ClaimBatch` leases up to `n` jobs in
  one statement (`LIMIT $n ... FOR UPDATE SKIP LOCKED` on PostgreSQL, MySQL and
  MariaDB, one `UPDATE ... RETURNING` on SQLite) under one lease token, in no
  particular order; fencing stays per job (job ID and token).
  `WorkerOptions.BatchSize` defaults to 10, and a claim never asks for more
  than the queue's free slots, so each claimed job starts at once and leases,
  attempts and fairness are unchanged.
- **Recovery off the claim path.** `Store.Claim` no longer recovers expired
  leases; the worker's own loop (every `Lease/4`, 100 jobs per statement) is
  the only sweep. Code driving a store without a worker calls `Recover`.
- **Wake-up.** `queuepostgres.WithNotify` adds `pg_notify('forge_jobs', queue)`
  to the enqueue statement for due jobs (delivered at commit) and makes the
  store a `queue.Listener`. A worker on a listener holds one `LISTEN`
  connection in its own lifecycle (started by `Start`, canceled and awaited
  by `Shutdown`, reconnected after a jittered 0.5s to 1s, doubling to 15s to
  30s), and a notification ends the idle wait of that queue's pollers: every
  idle worker of the queue claims once, at most once per 10ms per poller. On
  every (re)subscription each queue claims once. Polling remains, so
  notifications are hints, never needed for correctness. It is opt-in because
  of its costs: a connection per worker, no PgBouncer transaction pooling, and
  serialized commits of notifying transactions.
- **Outbox batches.** A dispatcher worker claims up to
  `DispatcherOptions.BatchSize` (default 10) events in one transaction and
  delivers them one after another in claim order, acknowledging each on its
  own. One renewal per batch extends the leases of every event still held, so
  waiting events are never reclaimed; an event reclaimed anyway is skipped,
  and checking an event's lease and registering its handler happen in one
  step; unstarted events are released without using an attempt on shutdown
  or failure. A failing publish affects only its own event. A crash spends
  the attempt of every claimed but unstarted event (up to `BatchSize - 1`).
- **Recorded scheduler ticks.** `jobs.SchedulerOptions.Ticks`
  (`jobs.TickClaimer`): after taking a concurrency slot, a replica runs a tick
  only if `ClaimTick` records it. The table `forge_job_schedules` (framework
  migration `20261026120000_job_schedules`, owner `jobs`) keeps one row per
  job with its latest claimed tick, and one autocommit upsert advances it only
  when the tick is later (`ON CONFLICT ... WHERE last_tick_at < excluded` on
  PostgreSQL, `ON DUPLICATE KEY UPDATE ... IF(last_tick_at < tick, ...)` on
  MySQL and MariaDB; the row count says who won). So:
  - each tick runs on exactly one replica, whatever the replicas' clocks;
  - ticks only move forward: a lagging replica can never claim a past tick,
    and a tick that no replica claimed before a later one was is not run;
  - no connection is held after the statement, so jobs and requests keep the
    pool;
  - a tick ahead of the database clock is refused (`*jobs.EarlyTickError`)
    and retried when the database catches up (at most a minute), so a fast
    clock can neither run ticks early nor block the others;
  - a tick claimed elsewhere is skipped silently (debug log); a tick that
    cannot be recorded is skipped and reported to `OnError` (fail closed);
  - one row per job needs no pruning;
  - like `forge_jobs`, the table is a framework table without row-level
    security.

  `jobs/postgres` and `jobs/mysql` ship the migration and `NewTickClaimer`;
  generated applications compose the migration and pass the claimer.

## Consequences

- Fewer round trips per job and event; idle-to-claim latency drops from up to
  `MaxPollInterval` to milliseconds with `WithNotify`.
- Custom `queue.Store` implementations keep working: batching and listening
  are optional interfaces.
- With recorded ticks, two replicas can run consecutive ticks of one job at the
  same time (a run longer than the interval); jobs that must not overlap need
  their own guard. A tick is skipped, not retried, while the database is
  unreachable.
- Applications generated before this change keep running every tick on every
  replica until they compose `jobs<adapter>.Migrations()` and pass
  `jobs<adapter>.NewTickClaimer(database)` to the scheduler.
- Forge-Rust differs: it enqueues a scheduled tick as a queue job (recorded in
  the same transaction) and skips ticks later than `max_lateness`; it has no
  `LISTEN/NOTIFY` wake-up and no batch size knob for jobs, and its outbox
  default batch is 50 with a lease covering the batch.
  [forge-rust#80](https://github.com/rocketinsidecorp/forge-rust/issues/80)
  tracks the alignment.
