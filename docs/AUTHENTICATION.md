# Authentication and Authorization

Phase 3 now includes an API-oriented security core plus an opinionated
PostgreSQL identity/session adapter. The core remains database-independent.

## Opaque bearer sessions

`auth.NewToken` creates 32 random bytes with `crypto/rand` and encodes them as
unpadded base64url. Applications deliver `Token.Reveal()` once and persist
only `Token.Digest()` (SHA-256). A database disclosure therefore does not
directly disclose the active bearer credential. UUIDv7 values are identifiers,
not session tokens.

`auth.Middleware` accepts exactly one `Authorization: Bearer <token>` header.
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
middleware, err := auth.NewMiddleware(sessionRepository)
if err != nil {
    return err
}

permission, _ := auth.NewPermission("tasks:read")
handler, err := auth.Require(permission, listTasks)
if err != nil {
    return err
}

app.Handle("GET /v1/tasks", middleware.Authenticate(handler))
```

Applications may configure `auth.WithAuthenticationAuditor(auditor)` to record
syntactically valid bearer or cookie tokens rejected as unknown, revoked, or
expired. The event stores only the token digest; missing or malformed
credentials are not persisted. Audit failures are logged without changing the
existing `401` response. Because each rejected valid token can produce a
database write, deployments should apply request limits and plan audit
retention/capacity before enabling this option.

Use `auth.RequireAudited(permission, handler, auditor)` when permission
denials should be recorded. The event contains only the authenticated actor
ID; the permission and tenant are deliberately omitted from the audit payload.
`auth.Require` remains unchanged and does not persist denial events.

Permissions use explicit lowercase `resource:action` names and have no
wildcards. `auth.Require` denies by default: no principal is `401`; a current
principal without the permission is `403`. Authentication installs both the
principal and its validated tenant in the request context.

## Password hashing

`auth.HashPassword` uses the Go 1.27 standard-library `crypto/pbkdf2`
implementation with HMAC-SHA-256, a random 128-bit salt, a 256-bit derived key,
and a default work factor of 600,000 iterations. The encoded form carries a
version and work factor so future releases can identify hashes that should be
upgraded after successful authentication.

Forge never trims or normalizes passwords before hashing. Malformed hashes are
rejected fail-closed and comparisons use constant-time equality.

## PostgreSQL identity schema

`auth/postgres.Migrations()` provides the Phase 3 identity schema:

- users with normalized unique email, password hash, active/disabled status,
  and a credential/session version;
- organizations and tenants;
- memberships, roles, permissions, and role assignments;
- sessions containing only token digests, membership, credential version,
  expiry, and revocation time.

All entity identifiers default to PostgreSQL `uuidv7()`.

`auth/postgres.Repository` implements `auth.Resolver`. Every resolution
joins the live session, user, organization, tenant and membership state and
recomputes permissions from current role assignments. `RevokeSession` is
idempotent, while `RevokeUserSessions` increments the user's credential
version and revokes existing sessions atomically.

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
and PBKDF2 runs against a dummy hash.

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

`auth.NewLoginHandler` and `auth.NewLogoutHandler` are thin HTTP adapters.
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
attempts. `MaxEntries` bounds only the in-memory throttler.

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

Browser applications can use `auth.NewCookieLoginHandler`,
`Middleware.AuthenticateCookie`, `auth.RequireCSRF`, and
`auth.NewCookieLogoutHandler`. Cookie authentication is an explicit transport
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
`app/views/passwords`; every security decision is made by `auth.Browser`:

```go
browser, err := auth.NewBrowser(auth.BrowserOptions{
	Login:    login,           // *auth.LoginService (required)
	Sessions: repository,      // auth.SessionRevoker (required)
	MFA:      mfaService,      // optional: accounts with TOTP
	Recovery: recoveryService, // optional: password reset forms
	Auditor:  repository,      // records sign-outs
})
// ...
if err := htmlauth.Register(app, browser, renderer); err != nil {
	return err
}
```

| Method | Route | Result |
| ------ | ----- | ------ |
| `GET` | `/session/new` | form: organization (tenant slug), email, password |
| `POST` | `/session` | `303` to the validated `return_to` (default `/`) with a notice; `422` with one message for any wrong field; `429` with `Retry-After` when throttled; an account with TOTP goes to `/session/mfa` |
| `DELETE` | `/session` | revoke the session, clear the cookies, `303` to the sign-in page |
| `GET`, `POST` | `/session/mfa` | authenticator or backup code (with an MFA service) |
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

### Password-reset emails

Generated applications pass a recovery service to `auth.Browser` only when
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
page, err := auth.RequireAudited("reports:read", reportsPage, repository)
// ...
guarded, err := middleware.AuthenticateBrowser(htmlauth.SignInPath, page)
```

Authenticated responses are marked `Cache-Control: no-store` (a handler may
still set its own value). A request without a valid session that prefers HTML
(`web.PrefersHTML`) is answered `303 /session/new?return_to=%2Freports` (GET and HEAD keep the
path; other methods go to the sign-in page without one). JSON and API
clients keep the `401`. `auth.ReturnPath(candidate, fallback)` accepts only
local paths (`web.IsLocalPath`: no scheme, host, `//` or `/\` prefix,
backslash, control, space or non-ASCII byte), so `return_to` cannot redirect
off the site. The wiring `forge generate scaffold` prints uses this guard.

## Creating the first owner: `setup`

```bash
forge migrate
forge setup --tenant acme --email owner@example.com --organization "Acme Inc"
```

`setup` (`authpostgres.SetupCommand()`, registered by generated
`main.go`) calls `Repository.ProvisionOwner`, which creates the organization,
tenant, user, active membership and an `owner` role with every permission in
`forge_permissions`, in one transaction under an advisory lock, with UUIDv7
IDs generated in Go and an `auth.identity.provisioned` audit event. It
refuses with `ErrAlreadyProvisioned` once any tenant exists, so it cannot add
an account to, or take over, a running installation; run it again and
nothing changes.

The password is read from `FORGE_SETUP_PASSWORD`, from one line of standard
input with `--password-stdin`, or from a terminal prompt (twice, echo off);
it is never an argument, never printed and must be at least 12 characters.
In a container: `docker compose run --rm app setup --tenant acme --email ...`.

The `owner` role holds every permission: setup grants all registered
permissions, and migrations from `forge generate resource` and
`forge generate scaffold` grant their new permissions to every role named
`owner`. Grant narrower roles to other members.

`forge seed` runs the application's idempotent `db.Seed`
(`postgres.SeedCommand`) in one transaction; it is not for accounts.

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
