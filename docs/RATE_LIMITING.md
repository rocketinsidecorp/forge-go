# Rate limiting

Forge's `ratelimit` package is Rails' `rate_limit` with safe defaults: a
fixed-window limiter, HTTP middleware that answers `429` with `Retry-After`, a
bounded in-memory store, and shared stores for PostgreSQL, MySQL/MariaDB and
SQLite. The design is in [ADR 0031](adr/0031-rate-limiting.md).

(Password login has its own two-key throttle, [ADR 0011](adr/0011-login-throttling.md);
use `ratelimit` for everything else.)

## Protect a route

Rails:

```ruby
rate_limit to: 10, within: 3.minutes, only: :create
```

Forge:

```go
limiter, err := ratelimit.NewMemory(ratelimit.MemoryOptionsFromConfig(app.Config().RateLimit))
if err != nil { return err }

create, err := ratelimit.Middleware(limiter, ratelimit.ByClientIP,
    ratelimit.Limit{N: 10, Per: 3 * time.Minute})
if err != nil { return err }

err = r.Handle("POST /comments", create(http.HandlerFunc(h.Create))) // only this route
```

For the whole application, use `app.Use(create)`; the default scope is then
shared by all routes. `Middleware` returns `func(http.Handler) http.Handler`,
the `httpserver.Middleware` type, so it also works with
`forge.WithHTTPMiddleware`.

Over the limit the client gets `429` and `Retry-After: <seconds>`:

```json
{"error":{"code":"rate_limited","message":"too many requests","request_id":"..."}}
```

Browsers (`Accept: text/html`) get a minimal static HTML page instead; both
carry `Cache-Control: no-store`. Place the middleware **after** authentication
when it keys by principal, and before expensive work (password hashing, mail,
uploads): requests are counted before the handler runs, whatever its outcome.

### Scopes

The counter of a request is identified by (scope, window length, client key).
The scope defaults to the matched route pattern (`POST /comments`), so the same
`create` middleware value applied to two routes counts them separately. Use
`ratelimit.WithScope("writes")` to share one counter across routes on purpose.

### When the limiter fails

If the limiter returns an error (the database is down), the middleware fails
**closed** with `503 rate_limit_unavailable`; an attacker who can make the
limiter fail must not become unlimited. `ratelimit.FailOpen()` lets requests
through (and logs a warning) for routes where availability matters more.

## Who is the client? Proxy caveats

`ratelimit.KeyFunc` is `func(*http.Request) string`; the result is hashed
before it reaches a store (no addresses or identifiers at rest).

`ratelimit.ByClientIP` keys by the client address the HTTP server resolved
(`web.ClientIP`, [ADR 0037](adr/0037-trusted-proxies-and-client-ip.md)). That
is the **TCP peer address**, unless the peer is a trusted proxy listed in
`FORGE_HTTP_TRUSTED_PROXIES`: then it is the first address of
`X-Forwarded-For` that is not a trusted proxy, read from the right. IPv4
addresses are used whole; IPv6 addresses are reduced to their `/64`, because
one subscriber controls the whole prefix and could otherwise rotate addresses
at will. `X-Real-IP` and `Forwarded` are never read, and `X-Forwarded-For`
from a peer that is not listed is ignored, so a client cannot pick a fresh
bucket per request. `ratelimit.ByRemoteAddr` always keys by the peer.

Consequences by deployment:

| Deployment | `ByClientIP` | What to do |
| ---------- | ------------ | ---------- |
| Direct (TLS in the application) | the client | nothing |
| `forge deploy` behind forge-proxy | the client | nothing: the proxied role trusts the `forge` network |
| Behind another proxy / load balancer | **the proxy** until configured: every client shares one bucket | `FORGE_HTTP_TRUSTED_PROXIES=<proxy CIDRs>` |

```sh
export FORGE_HTTP_TRUSTED_PROXIES=10.0.0.0/24   # your load balancer's subnet
```

Key by something the application has authenticated whenever possible:

```go
byUser := func(r *http.Request) string {
    if p, ok := auth.FromContext(r.Context()); ok {
        return p.SubjectID().String()
    }
    return "" // anonymous clients share one bucket: limited, never exempt
}
// several values: ratelimit.Join(p.Tenant().ID.String(), p.SubjectID().String())
```

Do not write a `KeyFunc` that reads a forwarding header directly: a header
the client can set lets one client use unlimited keys and bypass the limit
entirely. Configure the trusted proxies and use `ByClientIP` instead.

An empty key is not "unlimited": unidentifiable clients share one bucket.

## Stores

| Store | Package | Shared across replicas | Notes |
| ----- | ------- | ---------------------- | ----- |
| Memory | `ratelimit` | no | bounded by `MaxKeys`, no goroutines; the limit is multiplied by the number of replicas |
| PostgreSQL | `ratelimit/postgres` | yes | one atomic upsert per hit, database clock |
| MySQL, MariaDB | `ratelimit/mysql` | yes | one short transaction per hit, database clock |
| SQLite | `ratelimit/sqlite` | processes of one host | application clock |

Shared limiter setup (PostgreSQL shown; use `mysql.CommandsFromSets` and
`sqlite.CommandsFromSets` with `ratelimitmysql` / `ratelimitsqlite` otherwise):

```go
forge.Main(bootstrap.Configure,
    postgres.CommandsFromSets(db.Migrations(), ratelimitpostgres.Migrations())...)

limiter, err := ratelimitpostgres.NewLimiter(db, ratelimit.StoreOptions{})
```

`app rollback` never reverts this framework migration, and removing the set
from `CommandsFromSets` later keeps `migrate` working
([framework migrations](DATABASE.md#framework-migrations)).

A new store should pass `limitertest.Run`.

### Memory limits

`FORGE_RATE_LIMIT_MAX_KEYS` (default `100000`) bounds the in-memory buckets.
When it is full of live buckets, `EvictSoonest` (default) drops the bucket
closest to expiring so new clients are never refused; `Deny` refuses new keys
until space frees (no counter is lost, but a flood of distinct keys locks out
new clients). Pick `MemoryOptions{WhenFull: ratelimit.Deny}` where a bypass is
worse than an outage.

**Never key an `EvictSoonest` limiter on a value the attacker can choose per
target** (the username of a login or reset attempt, an e-mail address, a
resource ID in the URL). Flooding the limiter with more than `MaxKeys` distinct
junk keys evicts the victim's counter and resets it, which defeats a
per-target limit. Use `Deny`, or better a database limiter (no key cap, expired
rows pruned), for such keys, and combine them with a per-client key.

### Pruning the database tables

Expired rows are removed in bounded batches (`StoreOptions.PruneBatch`, default
500; one `Prune` call runs at most `ratelimit.MaxPruneRounds` = 20 batches):
inline after every `PruneBatch` hits or every `PruneInterval` (default one
minute), whichever comes first, once the hit has been decided, so a stream of
new keys cannot pile up expired rows; and by `limiter.Prune(ctx)`, which you can
run from `jobs.Scheduler`. `PruneInterval: -1` disables the inline prune. No
goroutine is ever started. Live rows are bounded only by the distinct keys seen
within one window.

## Fixed windows: what to expect

The window of a key starts with its first hit and lasts `Per`. A client that
spends its allowance at the end of one window and again at the start of the next
gets up to `2N` requests in `Per`. For a hard ceiling, lower `N`. Denied
requests count too, so hammering a limited route does not shorten the wait.

## Testing

`limitertest.Run(t, func(t testing.TB) ratelimit.Limiter { ... })` is the
conformance suite every store passes. The memory limiter takes no clock in its
public API; use short windows (`Per: 50 * time.Millisecond`) in tests.
