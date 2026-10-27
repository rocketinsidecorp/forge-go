# Authentication and Authorization

Phase 3 now includes an API-oriented security core plus an opinionated
PostgreSQL identity/session adapter. The core remains database-independent.

Two packages make it up ([ADR 0052](adr/0052-import-boundaries.md)):
`github.com/rocketinsidecorp/forge-go/auth` is the domain (tokens,
principals, permissions, passwords, sign-in, MFA, recovery, throttling,
audit and the store ports) and imports no HTTP package;
`github.com/rocketinsidecorp/forge-go/auth/authhttp` is its HTTP adapter (the
middleware, `Require*`, the JSON handlers, `Browser`, cookies and OpenAPI).
Code written before the split replaces `auth.NewMiddleware`, `auth.Require`,
`auth.NewBrowser`, `auth.New*Handler` and the other HTTP names with the same
names in `authhttp`:

```go
import (
	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/auth/authhttp"
)
```

## Opaque bearer sessions

`auth.NewToken` creates 32 random bytes with `crypto/rand` and encodes them as
unpadded base64url. Applications deliver `Token.Reveal()` once and persist
only `Token.Digest()` (SHA-256). A database disclosure therefore does not
directly disclose the active bearer credential. UUIDv7 values are identifiers,
not session tokens.

`authhttp.Middleware` accepts exactly one `Authorization: Bearer <token>` header.
It rejects missing, duplicate, combined, malformed, unknown, and expired
credentials with a stable JSON `401` and `WWW-Authenticate`. Cookies, query
parameters, `X-User-*`, and `X-Tenant-*` headers are never authentication
fallbacks.

The application supplies an `auth.Resolver`. Its contract is deliberately
strict: for every request it must resolve the current session, active user,
active tenant membership, and current role permissions. Do not copy roles or
permissions into a long-lived token. A resolver infrastructure failure is a
generic `500`; an unknown credential is a `401`.

```go
middleware, err := authhttp.NewMiddleware(sessionRepository)
if err != nil {
    return err
}

permission, _ := auth.NewPermission("tasks:read")
handler, err := authhttp.Require(permission, listTasks)
if err != nil {
    return err
}

app.Handle("GET /v1/tasks", middleware.Authenticate(handler))
```

Applications may configure `authhttp.WithAuthenticationAuditor(auditor)` to record
syntactically valid bearer or cookie tokens rejected as unknown, revoked, or
expired. The event stores only the token digest; missing or malformed
credentials are not persisted. Audit failures are logged without changing the
existing `401` response. Because each rejected valid token can produce a
database write, deployments should apply request limits and plan audit
retention/capacity before enabling this option.

Use `authhttp.RequireAudited(permission, handler, auditor)` when permission
denials should be recorded. The event contains only the authenticated actor
ID; the permission and tenant are deliberately omitted from the audit payload.
`authhttp.Require` remains unchanged and does not persist denial events.

Permissions use explicit lowercase `resource:action` names and have no
wildcards. `authhttp.Require` denies by default: no principal is `401`; a current
principal without the permission is `403`. Authentication installs both the
principal and its validated tenant in the request context.

## Password hashing

`auth.HashPassword` (`auth.DefaultPasswordHasher`) writes Argon2id v19
(RFC 9106, `golang.org/x/crypto/argon2`) with Forge-Rust's parameters: 19,456
KiB of memory, 2 passes, 1 lane, a random 16-byte salt from `crypto/rand` and a
32-byte key, as a PHC string in unpadded standard base64:

```text
$argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>
```

**Where Argon2id comes from.** The core packages import no third-party
module ([ADR 0053](adr/0053-dependency-free-core.md)), so `auth` has no Argon2
of its own: it hashes with the function given to `auth.RegisterArgon2id`. The
identity stores `auth/postgres`, `auth/mysql` and `auth/sqlite` register
`golang.org/x/crypto/argon2` in their `init`, so an application that links one
(every `forge new --auth` application does) writes `$argon2id$` hashes with no
code of its own. An application with another credential store registers it
itself, once, in `main` or an `init`:

```go
import (
	"golang.org/x/crypto/argon2"

	"github.com/rocketinsidecorp/forge-go/auth"
)

func init() { auth.RegisterArgon2id(argon2.IDKey) }
```

The first registration wins and later ones are ignored; hashers look the
function up at each hash, so the order of initialization does not matter. A
function that is not Argon2id v19 with the parameters it is given
(`argon2.Key` is Argon2i, with the same signature; a wrapper could clamp the
cost) fails two known-answer checks and panics at registration.
`auth.DefaultPasswordAlgorithm()` reports what new hashes use. With nothing
registered:

- new hashes are PBKDF2-HMAC-SHA256 with 600,000 iterations
  (`$pbkdf2-sha256$v=1$i=600000$…`), the format below, which every build
  verifies;
- verifying an existing `$argon2id$` hash fails closed with
  `auth.ErrPasswordAlgorithmUnavailable`: the sign-in (or step-up) answers
  `503` and the request log names the fix; it is never counted as a wrong
  password and the hash is never rewritten. A hasher built with
  `auth.WithPasswordAlgorithm(auth.PasswordAlgorithmArgon2id)` refuses to hash
  the same way. Register Argon2id (or link an identity store) and restart.

Either framework verifies the other's hashes (a test verifies a hash written
by Forge-Rust). Verification uses the cost the stored hash names, within
bounds that cap what one stored hash can demand: only version 19; `m`, `t`
and `p` in that order as canonical decimals; `m` from `8p` to 65,536 KiB, `t`
from 1 to 10, `p` from 1 to 8; a salt of 8 to 64 bytes; a 32-byte key; no
padding, other alphabet or extra parameter; at most 1,024 characters.
Anything else is `ErrPasswordHashInvalid`, decided before any hashing.

**Upgrading from PBKDF2.** Hashes written before ([ADR
0048](adr/0048-argon2id-password-hashing.md)), `$pbkdf2-sha256$v=1$i=…`
(PBKDF2-HMAC-SHA256, 100,000 to 5,000,000 iterations), still verify. When a
sign-in succeeds with a hash that is not Argon2id with the current parameters
(PBKDF2, or Argon2id with any other `m`, `t` or `p`, or a salt that is not
16 bytes), `LoginService` hashes the password again and stores it through
`PasswordStore.ReplacePasswordHash`, which replaces it only if the stored hash
is still the one verified; if it changed concurrently, no session is issued.
Nothing needs to be migrated by hand: each account moves to Argon2id at its
next sign-in. That first sign-in costs a PBKDF2 verification plus an Argon2id
hash (about 120 + 50 ms). If hashing is saturated at that moment the rehash is
skipped, the sign-in still succeeds, and the next one tries again.

**Legacy timing floor.** A wrong password against a PBKDF2 hash costs about
120 ms, against an Argon2id hash or an unknown account about 30 ms: without
more, the difference would tell accounts that still have a PBKDF2 hash (that
exist and have not signed in since the upgrade) from unknown ones. So while a
hasher writes Argon2id, every failed check of `LoginService` and of the
`authhttp.Browser` step-up (unknown account or tenant, malformed login, wrong
password against any hash) is padded, from the credential lookup on, up to the
cost of one PBKDF2-600k verification plus 25% (one verification varies from
call to call by some 10-20%). The floor is calibrated once per hasher, on the
first failure, as the slowest of two PBKDF2 verifications, and rises with the
slowest legacy verification seen since, at most to twice the calibration:
observations only raise it, so a slow outlier must not slow every later
failure for the life of the process. The calibration itself is not capped: a
CPU flood during the first failure after start makes every failed check
slower until the process restarts, which costs latency only. Measured on a
4-CPU container: unknown account, wrong password on an Argon2id account and
wrong password on a PBKDF2 account all answered in 185 ms (medians within
0.5 ms; 33, 31 and 130 ms without the floor). The failure is audited first
(`auth.login_failed`, or the step-up's denied event), then padded, so a client
that hangs up during the padding, having learned the guess was wrong, still
leaves the record. The padding is a timer: it holds no `PasswordWorkLimiter`
slot, burns no CPU and ends with the request's context. Successful sign-ins
are not padded.

An application created with this release or later never stored a PBKDF2 hash,
and one whose `$pbkdf2-sha256$` hashes are all gone (below) no longer needs the
floor. Turn it off where `identityRoutes` (`app/bootstrap/bootstrap.go`)
builds the login service:

```go
hasher, err := auth.NewPasswordHasher(auth.WithLegacyHashTimingFloor(false))
login, err := auth.NewLoginService(repository, repository, auth.WithLoginPasswordHasher(hasher))
```

A `forge_pbkdf2` build, or one with no Argon2id registered, needs no floor:
every check already costs PBKDF2, and the default hasher turns it on only
while it writes Argon2id.

**Dormant PBKDF2 accounts.** To stop waiting for accounts that never sign in,
count them and make them choose a new password through "Forgot your
password?". This sets a hash no password matches (all-zero key) and ends their
sessions:

```sql
SELECT count(*) FROM forge_users WHERE password_hash LIKE '$pbkdf2-sha256$%';

UPDATE forge_users
SET password_hash = '$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA',
    session_version = session_version + 1,
    updated_at = now()
WHERE password_hash LIKE '$pbkdf2-sha256$%';
```

Do this only where reset emails are configured (`FORGE_AUTH_PASSWORD_RESET_URL`
and `FORGE_MAIL_*`); then turn the floor off.

**Bounded work.** Each hash holds about 19 MiB while it runs, so every hash and
verification (unknown accounts included) runs through one process-wide
`auth.PasswordWorkLimiter`, as Forge-Rust's `PasswordWorkLimiter`: one slot per
CPU (`GOMAXPROCS`), at most `auth.MaxDefaultPasswordWork` (16), and an attempt
that waits `auth.DefaultPasswordWorkWait` (2 s) for a slot fails with
`auth.ErrPasswordHashingUnavailable`: `503 authentication_unavailable` with
`Retry-After: 5` from the JSON handlers and `authhttp.Browser` (the generated
pages answer it through `web.Error`). The slot is held by the hashing
itself. Applications that need another bound build their own:

```go
limiter, err := auth.NewPasswordWorkLimiter(4, time.Second)
hasher, err := auth.NewPasswordHasher(auth.WithPasswordWorkLimiter(limiter))
login, err := auth.NewLoginService(repository, repository, auth.WithLoginPasswordHasher(hasher))
recovery, err := auth.NewRecoveryService(repository, delivery, auth.WithRecoveryPasswordHasher(hasher))
```

`authhttp.Browser` checks step-up passwords with its login service's hasher.

**FIPS 140.** Argon2id is not a FIPS-approved algorithm. Build the
application with the `forge_pbkdf2` tag to write PBKDF2-HMAC-SHA256 (600,000
iterations, the Go standard library's FIPS module) instead:

```sh
go build -tags forge_pbkdf2 -o bin/myapp ./cmd/myapp
```

Such a build still verifies Argon2id hashes through the registered
implementation (an identity store registers it even then), and rewrites them
as PBKDF2 at the next sign-in. `auth.WithPasswordAlgorithm(auth.PasswordAlgorithmPBKDF2SHA256)`
chooses PBKDF2 for one `PasswordHasher` only; the tag also covers `forge setup`
and every service left on the default hasher.

Forge never trims or normalizes passwords before hashing. Comparisons use
constant-time equality (`crypto/subtle`).

## PostgreSQL identity schema

`auth/postgres.Migrations()` provides the Phase 3 identity schema:

- users with normalized unique email, password hash, active/disabled status,
  and a credential/session version;
- organizations and tenants;
- memberships, roles, permissions, and role assignments;
- sessions containing only token digests, membership, credential version,
  expiry, revocation time and last use (`last_seen_at`, for the idle
  timeout).

All entity identifiers are UUIDv7 values the repositories generate in Go
(package `uuid`); the id columns have no database default, so the identity
migrations apply on PostgreSQL 16, 17 and 18. A row you insert by hand needs
its id: `uuid.MustNew()` in Go, or `forge uuid` in a shell:

```sh
psql "$FORGE_MIGRATION_DATABASE_URL" -c "INSERT INTO forge_roles (id, tenant_id, name) VALUES ('$(forge uuid)', '<tenant id>', 'auditor')"
```

**Behaviour change (forge-go QG9).** The identity migrations used to declare
`DEFAULT uuidv7()`, a PostgreSQL 18 function. They were edited in place
because no release included them yet; a database that applied the earlier
text keeps its (unused) defaults, and `migrate` accepts the checksum Forge
recorded for it, so no action is needed.

`auth/postgres.Repository` implements `auth.Resolver`. Every resolution
joins the live session, user, organization, tenant and membership state and
recomputes permissions from current role assignments. `RevokeSession` is
idempotent, while `RevokeUserSessions` increments the user's credential
version and revokes existing sessions atomically.

### Session idle timeout

A session also ends when nobody used it for the idle timeout, two hours by
default, before its absolute expiry. Resolution refuses a session whose
`last_seen_at` is older than the timeout, exactly as it refuses a revoked one
(the middleware answers `401`, browser pages redirect to the sign-in page),
and signing in again creates a new session. Resolving records the use at most
once a minute per session: a conditional `UPDATE` runs only when the stored
use is more than a minute old, so most requests only read and concurrent
requests record one use. The database clock decides both, so replicas with
skewed clocks agree.

```go
repository, err := authpostgres.New(database,
	authpostgres.WithSessionIdleTimeout(app.Config().Auth.SessionIdleTimeout))
```

`FORGE_AUTH_SESSION_IDLE_TIMEOUT` sets it, a duration from `5m` to `720h`.
There is no value that switches the idle check off: at `720h`, which equals
the 30-day maximum session lifetime (`WithSessionTTL`), the idle limit can no
longer end a session before its absolute expiry, so it no longer shortens
sessions, but every resolution still checks it. Generated applications pass
it as above.

Recording the use is part of resolving the session, and it fails closed: when
the conditional `UPDATE` fails, `ResolveSession` returns the error and the
request is answered `500` instead of being served on an unrecorded use. A
resolver connected to a read-only replica (`cannot execute UPDATE in a
read-only transaction`), a statement timeout (`FORGE_DATABASE_STATEMENT_TIMEOUT`)
or a lock wait on the session row therefore fails the request, at most once a
minute per session, since only a stale use is written. Resolve sessions on
the primary.

The migration `20261026090000_session_idle_timeout` adds the column; sessions
that exist when it runs count as used at that moment, so upgrading signs
nobody out. Run `forge migrate` before deploying the new code: the resolver
reads the column. `ALTER TABLE ... ADD COLUMN` with the stable default
`now()` does not rewrite the table, but it takes a brief `ACCESS EXCLUSIVE`
lock on `forge_sessions`, and it waits behind any open transaction that uses
the table, holding up every session lookup queued after it. On a busy table,
bound the wait and retry instead of stalling sign-ins:

```sh
forge migrate
echo 'FORGE_AUTH_SESSION_IDLE_TIMEOUT=30m' >> .env.local   # shorter, for example

# Busy forge_sessions table: give up after 5s instead of queueing every lookup
# behind the lock, and run `forge migrate` again if it times out.
PGOPTIONS='-c lock_timeout=5s' forge migrate
```

The plaintext bearer token is never stored.

## Password login

`auth.LoginService` is the application use case for password authentication.
It depends only on `PasswordStore` and `SessionCreator` ports. The PostgreSQL
repository implements both.

A login always names its tenant explicitly:

```json
{
  "email": "alice@example.com",
  "password": "correct horse battery staple",
  "tenant": "acme"
}
```

### Canonical logins

Storage (`ProvisionOwner`, `setup`), lookup, throttle keys and password
recovery all use one canonical form of the login (`auth.CanonicalLogin`,
`auth.NormalizeIdentity`): surrounding whitespace trimmed, ASCII letters
lowercased, exactly one `@` with text on both sides, at most 320 bytes, and
only printable ASCII: **no non-ASCII, whitespace or control character**.
Tenant slugs are trimmed and ASCII-lowercased and must match
`^[a-z][a-z0-9-]{1,62}$`. Unicode case mapping differs between Go, PostgreSQL
collations and mail systems (`strings.ToLower` folds the Kelvin sign to `k`,
and PostgreSQL can fold `user@İcloud.com` to `user@icloud.com`), so a
look-alike spelling could otherwise match another account, get its own
throttle counters or receive another account's reset link. A non-ASCII login
is an **unknown account**: it gets the same response, the same throttling and
the same work as any other miss.

Password bytes are never trimmed or normalized. An unknown email, wrong
password, unknown or malformed tenant, non-canonical login, disabled user,
disabled organization/tenant, or inactive membership all produce the same
public `credentials_invalid` result and the same work: the attempt is
throttled, the credential store is queried (with random identifiers when the
login or tenant is malformed, so malformed input is not cheaper than a miss)
and the password is verified against a dummy hash with the current algorithm
and cost (Argon2id, m=19456, t=2, p=1), through the same limiter.

**Upgrading existing data.** An account stored before this rule with a
non-ASCII `email_normalized` can no longer sign in or receive a reset (a
non-ASCII address could not receive mail from Forge anyway,
[ADR 0024](adr/0024-mail.md)). Find them, then give each owner an ASCII
address by hand (Forge does not guess one):

```sql
SELECT id, email, email_normalized FROM forge_users
WHERE email_normalized !~ '^[!-~]+$' OR email_normalized <> lower(email_normalized);
-- UPDATE forge_users SET email = 'new@example.com', email_normalized = 'new@example.com' WHERE id = '...';
```

Run `migrate` before deploying code that uses the PostgreSQL throttle: it adds
the pair counter scope (see below).

The default session lifetime is 24 hours and may be configured between five
minutes and 30 days. A successful login can upgrade an old password work factor
with a compare-and-swap update. If the password changed concurrently, Forge
does not issue a session from the stale credential.

`authhttp.NewLoginHandler` and `authhttp.NewLogoutHandler` are thin HTTP adapters.
The login response returns `access_token`, `token_type: "Bearer"`, and
`expires_at`. Logout accepts exactly one bearer credential and is idempotent;
a syntactically valid token that is unknown or already revoked still returns
`204`.

Applications should register these handlers only on POST routes.

## Login throttling

`LoginService` rate-limits every password attempt before credential lookup,
including attempts with a malformed login or tenant. It derives SHA-256 keys
for three counters, so the limiter never stores plaintext email addresses or
IP addresses:

| Counter | Default (15 minutes) | Stops |
| ------- | -------------------- | ----- |
| login and source (`PairLimit`) | 5 | a guesser, quickly, without locking the owner out |
| login, all sources (`AccountLimit`) | 50 | guessing one login from many sources |
| source (`SourceLimit`) | 300 | password spraying from one source |

The account is the canonical login alone, so changing the tenant field or the
letter case does not reset a count. IPv6 sources are counted by their `/64`
prefix (one subscriber's usual allocation) and IPv4-mapped IPv6 addresses as
IPv4. A refused attempt is not counted: a blocked attacker hammering one login
cannot burn the login's shared budget and lock the owner out. Configure the
limits with `LoginThrottleConfig`; a zero `PairLimit` disables the pair counter
(the account limit then applies to all sources together).

The default `MemoryLoginThrottler` is bounded to 20,000 counters. When the table
is full it evicts the oldest counters that block nothing and refuses new
attempts only when every counter is blocking, so filling it from many sources
neither flushes a blocked attacker's counter nor refuses everybody. A
successful login clears the login and pair counters but not the source counter,
so one successful account does not reset password-spraying pressure from that
source.

For horizontally scaled PostgreSQL deployments, use
`auth/postgres.NewLoginThrottler` through `WithLoginThrottler`. It stores
shared hashed counters in `forge_login_throttle` (the migration
`20261020120000_login_throttle_pair_scope` adds scope 3, the pair) and counts
an attempt in one transaction that locks the counters in a fixed order, so
concurrent attempts neither deadlock nor exceed a limit. It also prunes a few
expired counters on every attempt (more than one attempt creates), so the table
follows the live windows however many logins and sources an attacker cycles
through, with no scheduled job; `PruneExpired` still deletes expired counters in
bounded batches using `SKIP LOCKED` to empty a table that stopped receiving
attempts. `MaxEntries` bounds only the in-memory throttler. Generated
PostgreSQL applications wire it for sign-in, two-factor codes and password
resets.

The in-memory throttler counts per process: with several replicas a guesser
gets each replica's budget, and every restart or deploy resets it. When
`FORGE_ENV=production`, `NewLoginService`, `NewMFAService` and
`NewRecoveryService` therefore log a warning (through `slog.Default`, or the
recovery service's `WithRecoveryLogger`) when they count attempts in a
`MemoryLoginThrottler`, the default or an explicit one. It is a warning and
not a refusal, since a single-process application (SQLite, one replica) may
count in memory on purpose:

```text
level=WARN msg="auth: sign-in attempts are throttled in this process only (auth.MemoryLoginThrottler): ..." service=sign-in environment=production
```

A blocked login returns `login_throttled` with HTTP `429`; the HTTP adapter
also sends `Retry-After`. The source is the client address the HTTP server
resolved (`web.ClientIP`): the socket peer, unless the peer is a trusted proxy
listed in `FORGE_HTTP_TRUSTED_PROXIES`, in which case it is the first
untrusted `X-Forwarded-For` entry read from the right. The default trusts
nobody; `forge deploy` trusts the forge-proxy network, so clients behind
forge-proxy get their own counters.

See [ADR 0011](adr/0011-login-throttling.md) and
[ADR 0037](adr/0037-trusted-proxies-and-client-ip.md).

## Browser cookie sessions and CSRF

Browser applications can use `authhttp.NewCookieLoginHandler`,
`Middleware.AuthenticateCookie`, `authhttp.RequireCSRF`, and
`authhttp.NewCookieLogoutHandler`. Cookie authentication is an explicit transport
and never falls back to bearer authentication.

A successful cookie login sets two host-only cookies:

- `__Host-forge_session`: the opaque session token with `Secure`,
  `HttpOnly`, `Path=/`, and `SameSite=Lax`;
- `__Host-forge_csrf`: a separate random 256-bit token with `Secure`,
  `Path=/`, and `SameSite=Lax`.

The cookie login response never contains the session token. It returns the CSRF
token and session expiry, and is marked `Cache-Control: no-store` and
`Pragma: no-cache`. The bearer login response now uses the same no-cache
headers because it contains the plaintext session credential.

Cookie login requires an HTTPS `Origin` header whose host exactly matches the
request host. This protects the login endpoint itself from login CSRF before a
CSRF cookie exists. Cookie mode therefore intentionally requires HTTPS;
non-browser/API clients can continue using the bearer login endpoint.

For unsafe methods, `RequireCSRF` requires exactly one
`X-CSRF-Token` header whose canonical base64url value matches the CSRF cookie
in constant time. Safe methods (`GET`, `HEAD`, `OPTIONS`, `TRACE`) pass
through and must remain side-effect free.

Cookie logout requires CSRF, revokes a valid session credential when present,
clears both cookies, and remains idempotent for unknown or already-revoked
sessions.

When MFA is enabled, cookie login returns `202 mfa_required` without setting
session or CSRF cookies. `NewCookieMFACompletionHandler` performs the second
factor exchange and only then creates both browser cookies. This prevents the
password step from accidentally creating an authenticated browser session.

See [ADR 0012](adr/0012-cookie-sessions-and-csrf.md) and [MFA.md](MFA.md).

## Server-rendered sign-in pages

Generated PostgreSQL applications serve HTML sign-in, sign-out, second-factor
and password-reset pages, the equivalent of `rails generate authentication`
([ADR 0026](adr/0026-authentication-pages-and-setup.md)). The pages are
application code in `app/adapters/htmlauth` and `app/views/sessions`,
`app/views/passwords`, `app/views/mfa`; every security decision is made by
`authhttp.Browser`:

```go
browser, err := authhttp.NewBrowser(authhttp.BrowserOptions{
	Login:     login,           // *auth.LoginService (required)
	Sessions:  repository,      // auth.SessionRevoker (required)
	MFA:       mfaService,      // optional: accounts with TOTP
	Passwords: repository,      // re-checks the password before two-factor changes
	Recovery:  recoveryService, // optional: password reset forms
	Auditor:   repository,      // records sign-outs
})
// ...
if err := htmlauth.Register(app, browser, renderer, middleware); err != nil {
	return err
}
```

| Method | Route | Result |
| ------ | ----- | ------ |
| `GET` | `/session/new` | form: organization (tenant slug), email, password |
| `POST` | `/session` | `303` to the validated `return_to` (default `/`) with a notice; `422` with one message for any wrong field; `429` with `Retry-After` when throttled; an account with TOTP goes to `/session/mfa` |
| `DELETE` | `/session` | revoke the session, clear the cookies, `303` to the sign-in page |
| `GET`, `POST` | `/session/mfa` | authenticator or backup code (with an MFA service) |
| `GET` | `/mfa` | two-factor settings of the signed-in account (behind `AuthenticateBrowser`) |
| `POST` | `/mfa`, `/mfa/confirm` | turn on: the password, then the password and a code of the new key; backup codes once; every session revoked |
| `POST` | `/mfa/rotation`, `/mfa/rotation/confirm` | replace the authenticator: password and a current code at both steps |
| `POST` | `/mfa/backup_codes` | new backup codes (password and a current code) |
| `DELETE` | `/mfa` | turn off (password and a current code); every session revoked |
| `GET`, `POST` | `/passwords/new`, `/passwords` | request a reset: the same `303` and notice whether or not the account exists (mounted only with a recovery service) |
| `GET`, `PATCH` | `/passwords/edit?token=`, `/passwords` | choose a new password (12 characters or more); every session of the account is revoked |

How the flow stays safe:

- **Same cookies as cookie login.** `SignIn` and `CompleteMFA` set
  `__Host-forge_session` and a fresh `__Host-forge_csrf` through the helper
  `NewCookieLoginHandler` uses; the CSRF token is rotated at every sign-in.
  Redirect after they succeed: a page rendered in the same response would
  embed the old token.
- **CSRF fails closed.** Every `Browser` method requires `csrf.Verified(r)`,
  true only when `csrf.Protection` compared the form's
  `authenticity_token` (or `X-CSRF-Token`) with the cookie. An exempted
  route, a missing middleware or a JSON body is `403 csrf_invalid`. The
  sign-in page issues the token, which also prevents login CSRF.
- **Uniform failures.** A wrong organization, email or password and an
  inactive account, membership, tenant or organization are all
  `ErrCredentialsInvalid`; the generated page shows "That organization, email
  and password do not match an account." and never echoes the password.
  Reset requests answer the same, after at least 100 ms, for known, unknown
  and malformed accounts and for delivery failures, because the account is
  only looked up after the answer ([ACCOUNT_RECOVERY.md](ACCOUNT_RECOVERY.md)).
- **Throttling.** The login, MFA and recovery throttles apply unchanged;
  throttled calls return `*LoginThrottledError` or `*RecoveryThrottledError`
  and set `Retry-After`, and the pages answer `429`.
- **Second factor.** The pending challenge lives only in
  `__Host-forge_mfa` (`HttpOnly`, `Secure`, `SameSite=Strict`, expiring with
  the challenge); no session exists until the code is accepted.
- **Tenant selection.** The organization field holds the tenant slug,
  prefilled from `?tenant=`: logins always name their tenant and there is no
  default tenant. Forge does not derive the tenant from `Host`.

### JSON authentication APIs (opt-in)

`forge new` generates only the HTML flow above: it is the application's only
way to enter a password. The JSON authentication APIs are opt-in with
`forge new NAME --api-auth` (PostgreSQL), because each is another
credential-entry point to throttle, audit and keep in mind. With the flag the
generated `bootstrap.apiAuthRoutes` mounts:

| Method | Route | Handler |
| ------ | ----- | ------- |
| `POST` | `/auth/login`, `/auth/logout` | `NewLoginHandler`, `NewAuditedLogoutHandler`: bearer sessions for API clients |
| `POST` | `/auth/mfa/complete` | `NewMFACompletionHandler` (with `FORGE_AUTH_MFA_KEY`) |
| `POST` | `/auth/recovery/request`, `/auth/recovery/reset` | `NewAuditedPasswordRecoveryRequestHandler`, `NewAuditedPasswordResetHandler` |
| `POST` | `/auth/cookie/login`, `/auth/cookie/logout` | `NewCookieLoginHandler`, `NewAuditedCookieLogoutHandler`: the page cookies, for browser scripts |
| `POST` | `/auth/cookie/mfa/complete` | `NewCookieMFACompletionHandler` (with `FORGE_AUTH_MFA_KEY`) |
| `GET` | `/v1/auth/session` | the bearer session's principal, behind `Middleware.Authenticate` |

```sh
forge new myapi --api-auth
forge generate authentication --api-auth   # adds the JSON APIs to an existing application
```

`forge generate authentication --api-auth` never overwrites anything. In an
application that has every page file (one from `forge new` without the flag)
it writes nothing and prints only the JSON API wiring to paste into
`identityRoutes` after `htmlauth.Register`; in one without the pages it
writes them and prints both wirings; when only some page files exist it
refuses, naming the file in the way.

An application generated before this option mounts all of them;
`forge generate authentication` lists the ones its `bootstrap.go` still
mounts. Remove those no API client or script uses. Forge-Rust has the same
flag with the login, logout and session routes; its MFA completion, recovery
and cookie routes are tracked in
[forge-rust#82](https://github.com/rocketinsidecorp/forge-rust/issues/82)
([RAILS_PARITY.md](RAILS_PARITY.md), row `json-auth-routes`).

### Password-reset emails

Generated applications pass a recovery service to `authhttp.Browser` only when
outgoing mail is configured, so the reset pages and the "Forgot your
password?" link never promise an email that cannot be sent.
`bootstrap.passwordRecoveryDelivery` builds `auth/recoverymail` from
`FORGE_MAIL_URL`, `FORGE_MAIL_FROM` and `FORGE_AUTH_PASSWORD_RESET_URL` (the
absolute URL of `/passwords/edit`: https, or http on localhost); mail without
the reset URL is a startup error. Without mail the JSON recovery routes keep
their fail-closed, uniform behavior. See [MAIL.md](MAIL.md).

### Sending signed-out browsers to the sign-in page

Guard HTML pages with `Middleware.AuthenticateBrowser` instead of
`AuthenticateCookie`:

```go
page, err := authhttp.RequireAudited("reports:read", reportsPage, repository)
// ...
guarded, err := middleware.AuthenticateBrowser(htmlauth.SignInPath, page)
```

Authenticated responses are marked `Cache-Control: no-store` (a handler may
still set its own value). A request without a valid session that prefers HTML
(`web.PrefersHTML`) is answered `303 /session/new?return_to=%2Freports` (GET and HEAD keep the
path; other methods go to the sign-in page without one; with the i18n
middleware's `PathPrefix`, wherever it is registered, the locale prefix is kept:
`return_to=%2Fpt-BR%2Freports`). JSON and API
clients keep the `401`. `authhttp.ReturnPath(candidate, fallback)` accepts only
local paths (`web.IsLocalPath`: no scheme, host, `//` or `/\` prefix,
backslash, control, space or non-ASCII byte), so `return_to` cannot redirect
off the site. The wiring `forge generate scaffold` prints uses this guard.

## Creating the first owner: `setup`

```bash
forge migrate
forge setup --email owner@example.com                                # forge new --auth
forge setup --org acme --email owner@example.com --org-name "Acme Inc"   # forge new --tenants
```

`setup` (`auth<adapter>.SetupCommand(auth<adapter>.SetupOrganizations(Organizations))`,
registered by the generated `app/bootstrap/app_gen.go`, for `auth/postgres`,
`auth/mysql` and `auth/sqlite` alike) calls `Repository.ProvisionOwner`,
which creates the organization, tenant, user, active membership and an
`owner` role with every permission in `forge_permissions`, in one transaction
under the database's setup lock (an advisory lock on PostgreSQL, `GET_LOCK`
on MySQL and MariaDB, `BEGIN IMMEDIATE` on SQLite), with UUIDv7 IDs generated
in Go and an `auth.identity.provisioned` audit event. It refuses with
`ErrAlreadyProvisioned` once any tenant exists, so it cannot add an account
to, or take over, a running installation; run it again and nothing changes.

An application without organizations (`forge new --auth`) takes `--email`
only: setup creates the one organization every user belongs to, with the
hidden sign-in slug `default` (`auth.DefaultTenantSlug`), and refuses
`--org`. Its login and recovery services are built with
`auth.WithSingleTenant` and `auth.WithRecoverySingleTenant`, so every
sign-in and reset request is for that one, and a tenant the client posts
anyway is ignored; tenant isolation, roles and
permissions work as with organizations. With organizations (`--tenants`)
`--org SLUG` is required (`--tenant` is an alias, `--organization` one of
`--org-name`), and the sign-in page asks for it. `forge generate tenancy
--org SLUG` later renames `default` to `SLUG`.

The password is read from `FORGE_SETUP_PASSWORD`, from one line of standard
input with `--password-stdin`, or from a terminal prompt (twice, echo off);
it is never an argument, never printed and must be at least 12 characters.
In a container: `docker compose run --rm app setup --email ...` (plus `--org acme` with organizations).

The `owner` role holds every permission: setup grants all registered
permissions, and migrations from `forge generate resource` and
`forge generate scaffold` grant their new permissions to every role named
`owner`. Grant narrower roles to other members.

`forge seed` runs the application's idempotent `db.Seed`
(`postgres.SeedCommand`) in one transaction; it is not for accounts.

## Upgrading

Release note ([ADR 0052](adr/0052-import-boundaries.md)): the HTTP side of
`auth` (the middleware, `Require*`, the JSON handlers, `Browser`, the cookie
names, `ReturnPath`, `ErrCSRFInvalid`) moved to
`github.com/rocketinsidecorp/forge-go/auth/authhttp`, with the same names. No
aliases are left in `auth` (they would be an import cycle), so an application
created with `--auth`, `--tenants` or `forge generate authentication` before
the move stops compiling in `app/bootstrap/identity.go`,
`app/bootstrap/routes_gen.go`, `app/adapters/htmlauth` and their tests until
the qualifier changes. `auth.AuthorizeMFAChange` now takes the principal from
the request context only: `AuthorizeMFAChange(ctx, authorizer)`.

From the application's root, after updating Forge-Go (`go get
github.com/rocketinsidecorp/forge-go@<version>`), run the rewrite below. It
first checks the `forge:checksum` of the routes seam
(`app/bootstrap/routes_gen.go`, when the application has one) exactly as the
generators do, and stops without changing anything if the seam was edited by
hand; otherwise it rewrites the names, adds the import and re-seals the seam,
whose only change is then this rewrite. It runs in a subshell, so a stop does
not close your terminal, and running it again prints `nothing to rewrite`:

```sh
(
set -eu
seam=app/bootstrap/routes_gen.go
zero=0000000000000000000000000000000000000000000000000000000000000000
sealed() { sed -E "s#^// forge:checksum [0-9a-f]{64}\$#// forge:checksum $zero#" "$1" | sha256sum | cut -d' ' -f1; }
if [ -f "$seam" ] && [ "$(sed -nE 's#^// forge:checksum ([0-9a-f]{64})$#\1#p' "$seam")" != "$(sealed "$seam")" ]; then
  echo "$seam was edited by hand since a generator last wrote it (its forge:checksum no longer matches), so nothing was rewritten. Move your change into bootstrap.go (or copy a guard such as protect into another file of package bootstrap), restore the file with git checkout $seam, and run this again." >&2
  exit 1
fi
names='NewMiddleware|Middleware|Option|WithClock|WithAuthenticationAuditor|Require|RequireAudited|RequireCSRF|NewBrowser|BrowserOptions|Browser|SignInStep|SignInComplete|SignInNeedsMFA|SignInCredentials|ReturnPath|ReturnToParameter|SessionCookieName|CSRFCookieName|CSRFHeaderName|MFAChallengeCookieName|ErrCSRFInvalid|ErrMFAChallengeMissing|New\w*Handler'
files=$(grep -rlP --include='*.go' "\bauth\.($names)\b" app cmd 2>/dev/null || true)
if [ -z "$files" ]; then echo "nothing to rewrite"; exit 0; fi
perl -pi -e "s/\bauth\.($names)\b/authhttp.\$1/g" $files
perl -0pi -e 's#\n(\s*)"github.com/rocketinsidecorp/forge-go/auth"\n(?!\s*"github.com/rocketinsidecorp/forge-go/auth/authhttp")#\n$1"github.com/rocketinsidecorp/forge-go/auth"\n$1"github.com/rocketinsidecorp/forge-go/auth/authhttp"\n#' $files
if [ -f "$seam" ]; then
  sum=$(sealed "$seam")
  sed -i -E "s#^// forge:checksum [0-9a-f]{64}\$#// forge:checksum $sum#" "$seam"
fi
echo "rewrote: $(echo $files)"
)
```

Then check, re-vendoring first if the application vendors:

```sh
if [ -d vendor ]; then go mod vendor; fi
gofmt -l app cmd; go vet ./... && go test ./...
```

The commands use GNU `sed -i`, `grep -P` and `sha256sum`; on macOS install
GNU sed, grep and coreutils first. A file that no longer uses `auth` after the
rewrite fails to compile with `"…/auth" imported and not used`: delete that
import line. Code the application wrote itself, outside `app` and `cmd`,
changes the same way; the compiler lists every place.
`internal/scaffold/upgrade_doc_test.go` runs the rewrite as written here.

## Remaining Phase 3 work

Generated PostgreSQL applications wire the identity repository, login,
logout, current-session, the HTML pages above, `setup`, and (when
`FORGE_AUTH_MFA_KEY` is set) MFA challenge completion. The key must be
canonical unpadded base64url for 32 random bytes; use a secret manager in
deployed environments.

Inviting users and creating further tenants, and MFA
enrollment/rotation step-up policy remain application-specific. Framework
services support structured auditing; configure an auditor when composing
recovery and MFA services outside the generated defaults. PostgreSQL is the
only built-in identity persistence adapter. Other database adapters follow
the application-layer tenant-scoping contract in
[MULTI_TENANCY.md](MULTI_TENANCY.md); Forge does not provide database-enforced
tenant isolation for them.
