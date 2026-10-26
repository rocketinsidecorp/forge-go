# Account Recovery

Forge provides a provider-neutral password-recovery flow in the `auth`
package and PostgreSQL persistence in `auth/postgres`.

## Requesting recovery

Create a `RecoveryService` with a recovery store, a `RecoverySender` and a
dispatcher that takes the work off the request path. The sender receives a
redacted `RecoveryToken` value whose plaintext is available only through
`Reveal()`.

```go
queue, err := auth.NewRecoveryQueue(auth.RecoveryQueueConfig{Logger: app.Logger()})
if err != nil {
    return err
}
recovery, err := auth.NewRecoveryService(repository, sender,
    auth.WithRecoveryDispatcher(queue),
    auth.WithRecoveryLogger(app.Logger()),
)
if err != nil {
    return err
}
// Register the hook after the database's, so the queue stops first.
if err := app.OnShutdown(queue.Shutdown); err != nil {
    return err
}
if err := queue.Start(recovery); err != nil {
    return err
}

handler, err := auth.NewPasswordRecoveryRequestHandler(recovery)
if err != nil {
    return err
}

app.Handle("POST /auth/recovery", handler)
```

The request body is:

```json
{
  "email": "alice@example.com",
  "tenant": "acme"
}
```

Known and unknown identities both receive HTTP `202` with
`{"status":"accepted"}`. Invalid email/tenant syntax is also treated as an
accepted no-op. This avoids exposing account or membership existence through
the response body or status.

### Why the answer comes first

`RecoveryService.Request` canonicalizes the login (`auth.CanonicalLogin`, ASCII
only: a non-ASCII address names no account), counts the request against the
throttle, hands the request to the dispatcher and answers. It never answers
sooner than **100 ms** (`WithRecoveryResponseTime`), whatever happened. The
account lookup, token issuance and delivery run afterwards, in
`RecoveryService.Process`, so neither the response nor its timing (a database
write and an SMTP conversation take far longer than an unknown account's
nothing) tells whether the account exists. Without a dispatcher `Request` does
that work inline and the timing difference returns; always set one where the
request is answered to the public.

- **`auth.RecoveryQueue`** (generated applications): a bounded in-process
  queue (256 requests, 2 workers, 30 s per request by default) whose workers
  start with `Start` and are stopped, draining first and then cancelling, by
  `Shutdown` from `App.OnShutdown`. There are no detached goroutines. A full
  queue never waits: the request is dropped and logged (no login, source or
  token in the log), so a flood can delay legitimate resets but cannot slow the
  page or exhaust memory. It is not durable; a person asks again.
- **`auth/recoveryqueue`** (applications with a persistent `queue` store): the
  dispatcher enqueues a job per request and `Register` adds the handler, so a
  failed delivery is retried (5 attempts, with backoff; each retry issues a
  new token and invalidates the previous one) and survives restarts:

  ```go
  registry := queue.NewRegistry()
  dispatcher, _ := recoveryqueue.NewDispatcher(registry, store)  // store: queue/postgres.Store
  recovery, _ := auth.NewRecoveryService(repository, sender, auth.WithRecoveryDispatcher(dispatcher))
  _ = recoveryqueue.Register(registry, recovery)
  worker, _ := queue.NewWorker(store, registry, queue.WorkerOptions{})
  _ = worker.Start(ctx) // and register worker.Shutdown with App.OnShutdown
  ```

  While a job for one login and tenant is pending or running, repeated requests
  store nothing. The payload is the canonical login and tenant slug the visitor
  typed (whether or not an account has them) and two digests for the audit
  event; it never holds a token. The login is personal data kept until the job
  is pruned (`RetainSucceeded`, 24 hours by default), so keep the retention
  short.

### Delivery goes only to the stored address

The reset link is sent to `RecoveryIdentity.Email`, the address the store holds
for the account, never to what the visitor typed. Combined with the canonical
login rule, a look-alike spelling (`user@İcloud.com`) is not looked up at all,
and one a database collation would fold onto the account still cannot receive
the account's token.

### Throttle

The default recovery throttle allows, per one-hour window, three requests per
login and source, ten per login across sources (so a victim's mailbox cannot
be flooded from many sources, and one source cannot use up the victim's own
requests) and 30 per source; IPv6 sources count per `/64`. The keys are SHA-256
domain-separated hashes; plaintext emails and network sources are not stored by
the limiter. A blocked request returns `429 recovery_throttled` with
`Retry-After`. Reset submissions are counted per token and per source.

For multi-replica PostgreSQL applications, use
`auth/postgres.NewRecoveryThrottler` through
`auth.WithRecoveryThrottler`. It uses the shared PostgreSQL throttle table,
while recovery keys use a separate hash domain and do not collide with login
keys. Run `migrate` first: the table gained the pair counter scope.

`RecoverySender` should itself return promptly. If delivery fails,
`RecoveryService.Process` invalidates the just-issued token and returns a
typed `RecoveryDeliveryError` (logged by the queue, retried by the job queue).
The public HTTP handler still returns the same `202 accepted` response, so an
email-provider outage does not become an account-enumeration oracle. Sender
implementations should log/alert their own delivery failures.

## Resetting the password

The reset endpoint accepts:

```json
{
  "token": "<opaque recovery token>",
  "new_password": "<new password>"
}
```

Register it as a POST route:

```go
reset, err := auth.NewPasswordResetHandler(recovery)
if err != nil {
    return err
}

app.Handle("POST /auth/reset-password", reset)
```

Malformed, expired, unknown, consumed, or superseded tokens all return the same
`400 recovery_invalid` response. Reset attempts are throttled before Argon2id
password hashing to bound CPU and memory abuse, and a store that implements
`auth.RecoveryTokenChecker` (`auth/postgres.Repository` does) is asked whether
the token exists, is unused and has not expired before the new password is
hashed, so a request with a made-up token costs one lookup, not a hash. A store
without it still works and hashes first.

On PostgreSQL, a successful reset atomically replaces the password hash,
increments `session_version`, revokes all sessions for the user, and consumes
all outstanding recovery tokens. The same token cannot be used twice, and two
different valid links racing for one user cannot both win.

Successful reset responses are `204`. All responses from the recovery request
and reset handlers, including errors and throttling responses, are marked
`Cache-Control: no-store` / `Pragma: no-cache`.

## Storage and cleanup

`auth/postgres.Migrations()` adds `forge_password_recovery` with a unique
32-byte token digest, user ID, expiry, consumption time, and creation time.

`Repository.PruneRecovery(ctx, limit)` deletes at most `limit` consumed or
expired rows using `SKIP LOCKED`. Run it periodically; Phase 4 scheduling can
own that periodic work later.

See [ADR 0013](adr/0013-password-recovery.md).
