# ADR 0026: Rate limiting

- **Status:** Accepted
- **Date:** 2026-10-19

## Context

Rails 8 added `rate_limit to: 10, within: 3.minutes, only: :create`: one line
that protects a route, backed by the application cache. Forge already throttles
password login (ADR 0011) with a purpose-built, two-key limiter, but
applications need the same protection for every other route (comment forms,
password-reset requests, API endpoints, uploads) without writing a limiter
each time, and with a store that works on every database Forge supports and
across replicas.

The design must also be honest about the two ways rate limiting fails in
practice: the store grows without bound under a flood of distinct keys, and the
client "identity" is whatever the application believed, which behind a reverse
proxy is usually spoofable. Forge does not interpret `X-Forwarded-For` (see the
roadmap), so the default must not pretend to.

## Decision

### Package layout

| Package | Role | Imports |
|---------|------|---------|
| `ratelimit` | `Limit`, `Limiter`, `Decision`, bounded `Memory` limiter, `Key`, HTTP `Middleware`, `ByRemoteAddr`, `Join` | stdlib, `config`, `fault`, `web` |
| `ratelimit/limitertest` | conformance suite | stdlib, `ratelimit` |
| `ratelimit/postgres`, `/mysql`, `/sqlite` | shared limiters and their embedded migrations | the adapter, `ratelimit` |

`ratelimit` and `limitertest` are core packages (ADR 0006); the three database
packages are adapters, one per database like the job queue (ADR 0023).

### Fixed windows

A `Limiter` has one method, `Allow(ctx, key, Limit{N, Per})`. The window of a
key starts with its first hit and lasts `Per`; at most `N` hits are allowed in
it; the next hit after the window starts a new one. Every call counts as a hit,
denied or not, so a client cannot probe for free. A fixed window is one atomic
statement on every database (`INSERT ... ON CONFLICT` / `ON DUPLICATE KEY
UPDATE` with the reset decided on the database clock), has no read-modify-write
race, and is what Rails does. Its known cost is the boundary burst: a client
aligning its requests to a window edge gets up to `2N` in `Per`. A sliding
window counter would smooth that at the price of two rows or a log per key, a
per-dialect SQL that is hard to make atomic on MySQL, and a harder-to-explain
contract. We accept the burst and document it; applications that need a hard
ceiling lower `N`.

`Limit` is validated (`N` in `[1, 2^30]`, `Per` in `[1ms, 30d]`). The same
`CheckRequest` runs in every implementation, so all stores reject the same
inputs, and the conformance suite checks it.

### Keys never contain identifiers

`ratelimit.Key(scope, parts...)` derives the store key: SHA-256 over
length-prefixed parts, in hex (64 characters). The middleware always passes
`scope` (the route pattern by default), the window length and the application's
`KeyFunc` result through it. The store therefore sees fixed-length, opaque,
ASCII keys: no IP addresses or e-mail addresses at rest (the same property as
ADR 0011), no unbounded key length, no delimiter confusion between scopes, and
the same key under two different limits does not share a counter. Database
columns are `ASCII` and at most 128 characters.

### Memory limiter: bounded, no goroutines

`Memory` holds at most `MaxKeys` buckets (default 100,000; `FORGE_RATE_LIMIT_MAX_KEYS`).
Buckets are also in a min-heap by window end; expired buckets are popped during
later calls, so expiry costs `O(log n)` amortized and there is **no sweeper
goroutine** to own, stop or leak (the decision rule of the project: no hidden
background tasks). When the limiter is full of live buckets:

- `EvictSoonest` (default) removes the bucket closest to expiring. New clients
  are never refused, and the counters that reset are the ones about to reset
  anyway; an attacker needs more than `MaxKeys` distinct keys inside one window
  to push a victim's counter out.
- `Deny` refuses new keys until space frees. No counter is ever lost, but a
  flood of distinct keys locks out new clients. Choose it where a bypass is
  worse than an outage.

Counters saturate at `N+1`, so a flood cannot overflow them.

### Shared limiters

Each database adapter stores one row per live bucket (`forge_rate_limits`:
bucket, hits, expires_at) and counts a hit atomically:

- PostgreSQL: one `INSERT ... ON CONFLICT DO UPDATE ... RETURNING` on the
  server clock.
- MySQL and MariaDB: `INSERT ... ON DUPLICATE KEY UPDATE`, then a read of the
  same row **in the same transaction** (the upsert holds the row lock until
  commit, so the count read is the caller's own, never a later hit's), with
  `UTC_TIMESTAMP(6)` as the clock and deadlock retries.
- SQLite: one upsert with `RETURNING` under the single-writer lock; the
  application clock is the only clock (SQLite serves one host).

Expired rows are removed by `Prune(ctx)`, which deletes at most `PruneBatch`
(default 500) rows per call, `SKIP LOCKED` where available so replicas do not
block each other. Allow also prunes opportunistically, **inline, at most once per
`PruneInterval` (default one minute)**, after the hit has been decided, ignoring
errors: tables stay small with no scheduler and no goroutine, and
`jobs.Scheduler` can call `Prune` for an explicit cadence.

A database limiter costs one round trip (PostgreSQL, SQLite) or one short
transaction (MySQL) per request. Put a `Memory` limiter with a generous limit
in front for floods, or accept the cost: the routes worth limiting (logins,
registrations, form posts) are not the hot read path.

### HTTP middleware

`ratelimit.Middleware(limiter, key, Limit{N, Per}, opts...)` returns a
`httpserver.Middleware`. Per route (Rails' `only: :create`) it wraps one
handler; with `App.Use` it covers the application. Over the limit it answers
`429` with `Retry-After` (whole seconds, rounded up) and `Cache-Control:
no-store`, in the JSON error format (`code: rate_limited`) or, when the client
prefers HTML (`web.PrefersHTML`), a minimal static HTML page.

- **Fail closed.** If the limiter returns an error, the middleware answers
  `503 rate_limit_unavailable` and the error is logged by `web.Error`; the
  cause is never sent to the client. `FailOpen()` opts out, for routes where
  availability matters more than protection; it logs a warning.
- **Scope.** The default scope is the matched route pattern
  (`http.Request.Pattern`), so one middleware value used on two routes counts
  them separately, and `WithScope` shares a counter on purpose.
- **Empty key.** A `KeyFunc` that returns `""` (for example "principal" for an
  anonymous request) is not exempt: all such requests share one bucket.

### Client identity is the application's decision

`KeyFunc func(*http.Request) string`. The only built-in is `ByRemoteAddr`: the
TCP peer address (IPv4 whole; IPv6 reduced to its `/64`, because one
subscriber controls a whole `/64` and could otherwise rotate addresses),
**never** `X-Forwarded-For`, `X-Real-IP` or `Forwarded`. Behind a reverse proxy
(`FORGE_HTTP_TRANSPORT=trusted-proxy`) the peer is the proxy, so
`ByRemoteAddr` puts every client in one bucket: loud and safe, but useless as a
per-client limit. Two honest options are documented: key by something the
application authenticated (session or user ID, `ratelimit.Join` for several
values), or write a `KeyFunc` that reads the one header the deployment's proxy
overwrites, only when the proxy is the sole network path to the application. A
first-party trusted-proxy policy remains a separate roadmap item; limiter keys
must not be derived from client-controlled input (the type documents this).

## Consequences

- One line protects a route, on any database, with bounded memory and no
  background goroutines.
- The login throttler (ADR 0011) is unchanged: its two-key, success-clears
  semantics are not expressible in `Limiter`, and replacing it would change a
  reviewed security contract. New code uses `ratelimit`.
- The fixed-window boundary burst and the shared-store round trip are accepted
  costs, documented in `docs/RATE_LIMITING.md`.
- Behind a proxy the default key is useless until the application chooses a
  better one; the framework does not guess.
- MySQL and MariaDB limiters were verified on MariaDB; CI also covers MySQL 8.4.
