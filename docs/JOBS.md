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

    store, err := queuepostgres.NewStore(db)
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
`MaxPollInterval` (1s/10s, jittered), `Backoff`, `RetainSucceeded` (24h),
`WorkerID`, `Hooks`. Run several processes and workers freely; a job is leased
to one worker at a time, a worker that lost its lease cannot overwrite the new
owner's result (token fencing), and a handler is canceled before its lease can
expire when renewals fail. Delivery stays at-least-once: see above.

Lease recovery runs in every worker, independently of free slots, every
`Lease/4` (100ms to 5s): a crashed worker's jobs are pending again at most one
lease plus that interval after its last renewal, as long as one worker is alive.

Run job processing in dedicated replicas by starting the worker only in a
second command or `Configure` branch selected by configuration; the queue
itself does not care which process claims.

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

Delivery is at-least-once, not exactly-once. Polling, not push: a job can wait
up to the idle poll interval. The queue is global, not tenant-scoped: put the
tenant ID in the payload and authorize in the handler. SQLite is single-host.
Workers only claim kinds they have a handler for; give specialized kinds their
own queue.
