# Background jobs

Forge ships a persistent, database-backed job queue in the package `queue`
([ADR 0023](adr/0023-persistent-job-queue.md)): typed payloads, enqueue inside
your database transaction, leases, retries with backoff, dead jobs you can
retry or discard, idempotency keys and graceful shutdown. It runs on
PostgreSQL, MySQL, MariaDB and SQLite.

(`jobs` is the older in-process scheduler for recurring work, ADR 0020. The
queue is for work you hand off: send this email, build this report.)

| Database | Package | Notes |
| -------- | ------- | ----- |
| PostgreSQL | `queue/postgres` | `FOR UPDATE SKIP LOCKED`, many worker hosts |
| MySQL 8.4, MariaDB 11.8 | `queue/mysql` | `SKIP LOCKED`, many worker hosts |
| SQLite | `queue/sqlite` | one host; development and small deployments |

## 1. Add the tables

Each adapter embeds its migrations. Compose them with the application's own:

```go
forge.Main(bootstrap.Configure,
    append(postgres.CommandsFromSets(db.Migrations(), queuepostgres.Migrations()),
        queuepostgres.Commands()...)...)
```

`queuepostgres.Commands()` adds the `jobs` operator command (below). For MySQL
and SQLite use `mysql.CommandsFromSets` / `sqlite.CommandsFromSets` with
`queuemysql` / `queuesqlite`. Run `app migrate` as usual.

`app rollback` never reverts this framework migration, and removing the set
from `CommandsFromSets` later keeps `migrate` working
([framework migrations](DATABASE.md#framework-migrations)).

## 2. Define a job

```go
type SendWelcome struct {
    UserID string `json:"user_id"`
}

func registerJobs(r *queue.Registry, mailer Mailer) error {
    return queue.Register(r, "mail.welcome",
        func(ctx context.Context, p SendWelcome) error {
            job, _ := queue.JobFromContext(ctx) // ID, Attempt, MaxAttempts...
            return mailer.SendWelcome(ctx, p.UserID, job.ID.String()) // job ID = idempotency key for the mail provider
        },
        queue.HandlerQueue("mail"),
        queue.HandlerMaxAttempts(8),
        queue.HandlerTimeout(30*time.Second),
    )
}
```

The payload type `T` is the job's schema. Enqueueing any other type for the
kind is an error; the worker decodes strictly (unknown fields are rejected).
Put identifiers in payloads, never secrets: payloads are stored in clear text.

Handlers run **at least once**. A crash after the side effect but before the
acknowledgement, or a lease that expires under a slow handler, runs the job
again. Make handlers idempotent.

Handler errors:

- any error: retry with exponential backoff and jitter, until `MaxAttempts`,
  then the job is dead;
- `queue.Permanent(err)`: dead immediately (invalid input, a rejected request);
- `queue.RetryAfter(d, err)`: retry after `d` (a downstream `Retry-After`);
- a panic: recorded as a failure and retried.

Defaults: 10 attempts; backoff 5s doubling to 1h with equal jitter; queue
`default`; priority 0 (lower runs first).

Generate the handler, its payload type, an enqueue function and tests:

```sh
forge generate job send_welcome user_id:uuid --queue
```

It writes `app/jobs/send_welcome.go` (`SendWelcomePayload`,
`RegisterSendWelcome(registry)`, `EnqueueSendWelcome(...)`, a placeholder
`PerformSendWelcome` that fails permanently until you implement it) and its
test, and prints the registration line ([Generators](GENERATORS.md#job)).

## 3. Enqueue, atomically with your data

```go
err := db.InTx(ctx, func(tx pgx.Tx) error {
    if err := createUser(ctx, tx, user); err != nil {
        return err
    }
    _, err := queue.Enqueue(ctx, store.In(tx), registry, "mail.welcome",
        SendWelcome{UserID: user.ID.String()},
        queue.WithKey("welcome:"+user.ID.String()), // idempotent while pending/running
    )
    return err
})
```

`store.In(tx)` writes through your transaction: if it rolls back there is no
job, and workers cannot see the job before it commits. On MySQL and SQLite,
`tx` is the `*sql.Tx` of `db.InTx`. Without a transaction, pass the store:
`queue.Enqueue(ctx, store, registry, ...)`.

On PostgreSQL, transactions at REPEATABLE READ or SERIALIZABLE can fail with
SQLSTATE 40001 when two of them enqueue the same key concurrently, as with any
conflicting write; run them with `postgres.TxOptions{Attempts: n}` and the retry
reports a duplicate. READ COMMITTED needs no retry. On MySQL and MariaDB a
duplicate enqueue takes no lock on the running job.

Options: `WithQueue`, `WithMaxAttempts`, `WithPriority`, `WithKey`, `RunAfter(d)`,
`RunAt(t)` (years 1000 to 9999). `InsertResult.Duplicate` tells you a key was already held; its
`ID` is the existing job.

## 4. Run the worker and stop it with the app

```go
func Configure(ctx context.Context, app *forge.App) error {
    db, err := postgres.Open(ctx, app.Config().Database)
    if err != nil { return err }
    if err := app.OnShutdown(func(context.Context) error { db.Close(); return nil }); err != nil { return err }

    // WithNotify: enqueues announce themselves (NOTIFY forge_jobs) and idle
    // workers wake at once instead of at their next poll (optional).
    store, err := queuepostgres.NewStore(db, queuepostgres.WithNotify())
    if err != nil { return err }
    registry := queue.NewRegistry()
    if err := registerJobs(registry, mailer); err != nil { return err }

    worker, err := queue.NewWorker(store, registry, queue.WorkerOptions{
        Queues: []queue.QueueConfig{{Name: "default", Concurrency: 5}, {Name: "mail", Concurrency: 2}},
        Logger: app.Logger(),
    })
    if err != nil { return err }
    if err := worker.Start(ctx); err != nil { return err }
    // Registered after the database hook, so it runs before it (reverse order).
    return app.OnShutdown(worker.Shutdown)
}
```

On SIGTERM the worker stops claiming at once and running jobs finish.
`Shutdown` (bounded by `FORGE_HTTP_SHUTDOWN_TIMEOUT`) cancels what is left;
every handler that then returns gives its job back to the queue without
counting an attempt, and `Shutdown` waits for those writes before returning.
A handler that ignores its canceled context is **not released** (another worker
would run the job concurrently with it): its lease stops being renewed, the job
becomes available when the lease expires, and `Shutdown` returns an error
saying how many handlers were still running. Keep handlers responsive to `ctx`.

Worker options (all optional): `Lease` (60s; renewed every third of it, so a
crashed worker's jobs resume after at most a lease), `PollInterval`/
`MaxPollInterval` (1s/10s, jittered), `BatchSize` (10, see below), `Backoff`,
`RetainSucceeded` (24h), `WorkerID`, `Hooks`. Run several processes and workers freely; a job is leased
to one worker at a time, a worker that lost its lease cannot overwrite the new
owner's result (token fencing), and a handler is canceled before its lease can
expire when renewals fail. Delivery stays at-least-once: see above.

Lease recovery runs in every worker, independently of free slots and off the
claim path, every `Lease/4` (100ms to 5s), up to 100 jobs per statement: a
crashed worker's jobs are pending again at most one lease plus that interval
after its last renewal, as long as one worker is alive. `Store.Claim` does not
recover leases; call `Store.Recover` if you drive a store without a worker.

**Batches.** A poller claims up to `BatchSize` jobs in one round trip
(`FOR UPDATE SKIP LOCKED` with a `LIMIT` on PostgreSQL, MySQL and MariaDB; one
`UPDATE ... RETURNING` on SQLite), but never more than its queue has free
slots, so every claimed job starts at once: leases, attempts and fairness
between queues and workers are those of claiming one job at a time.
`BatchSize: 1` restores one job per round trip.

**Wake-up (PostgreSQL).** With `queuepostgres.WithNotify()`, every due job
enqueued through the store (or `store.In(tx)`, when the transaction commits)
sends `NOTIFY forge_jobs` with its queue name, and every worker on the store
holds one `LISTEN` connection: an idle worker claims a new job within
milliseconds instead of after up to `MaxPollInterval`. Each notification
wakes every idle worker polling that queue, and each claims once (at most one
woken claim per 10ms per poller). Polling stays on as the fallback, so a lost
notification only delays a job to the next poll. The listener reconnects after
a jittered 0.5s to 1s, doubling to 15s to 30s while it keeps failing, and
`worker.Listening()` reports it. Costs: one extra connection per worker, outside the pool's limit; `LISTEN`
needs a direct connection or session pooling (not PgBouncer transaction
pooling); PostgreSQL serializes the commits of notifying transactions, which
matters only at thousands of enqueues per second. Anyone who can `NOTIFY` on
the database can wake workers, which then claim once.

Run job processing in dedicated replicas by starting the worker only in a
second command or `Configure` branch selected by configuration; the queue
itself does not care which process claims.

**Outbox batches.** The outbox dispatcher (`outbox/postgres`) claims up to
`DispatcherOptions.BatchSize` events (default 10) in one transaction and
delivers them one after another, acknowledging each on its own; their leases
are renewed together, and on shutdown the events not yet started go back
without using an attempt. The claim counts an attempt for every event of the
batch, so a crash spends one attempt on each claimed event that never
started (up to `BatchSize - 1`), and an event already on its last attempt is
then dead-lettered without having been delivered. Lower `BatchSize` or raise
`MaxAttempts` if that matters.

**Recurring jobs on several replicas.** The `jobs.Scheduler` (the generated
`JobsConfigure`) records each tick in `forge_job_schedules`
(`jobspostgres`/`jobsmysql` `.Migrations()` and `.NewTickClaimer`): each tick
of a job runs on exactly one replica, the first with a free slot to record
it, whatever the replicas' clocks, and no connection is held while the job
runs. Two replicas can run consecutive ticks of one job at the same time.

## 5. Operate

```
app jobs status                       jobs per queue: ready, scheduled, running, dead, succeeded, oldest ready
app jobs dead [-queue Q] [-kind K] [-limit N]   dead jobs with sanitized errors (never payloads)
app jobs show ID                      one job's metadata
app jobs retry ID                     dead -> pending, attempts reset
app jobs discard ID                   delete a dead job and its payload
app jobs prune [-older 24h]           delete finished jobs
```

Stored errors are `"<error type>: <message>"` with payload values, URL
credentials, secret assignments and bearer tokens redacted, cut to 1 KiB. That
is defense in depth; do not echo input in handler errors.

Programmatic access: the `queue.Admin` methods on the store (`Stats`, `Get`,
`ListDead`, `Retry`, `Discard`, `Prune`). `queue.Hooks` (`Started`,
`Finished` with outcome, duration and sanitized error) bridge to your own
metrics and tracing. `WorkerOptions.Telemetry` traces attempts and records
`forge.queue.job.duration` directly ([OBSERVABILITY.md](OBSERVABILITY.md)).

## Limits

Delivery is at-least-once, not exactly-once. Without `WithNotify` (and always
on MySQL, MariaDB and SQLite) workers poll: a job can wait up to the idle poll
interval. The queue is global, not tenant-scoped: put the
tenant ID in the payload and authorize in the handler. SQLite is single-host.
Workers only claim kinds they have a handler for; give specialized kinds their
own queue.
