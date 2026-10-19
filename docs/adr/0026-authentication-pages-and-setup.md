# ADR 0026: Authentication pages, sign-in redirects and first-owner setup

- **Status:** Accepted
- **Date:** 2026-10-19

## Context

Forge had a complete authentication back end (tenant-scoped password login
with uniform failures and throttling, opaque digest-only sessions, secure
cookie transport, CSRF, recovery, TOTP MFA, audit; ADRs 0010-0016) and,
since [ADR 0025](0025-server-rendered-html.md), server-rendered forms. What a
Rails developer gets from `rails generate authentication` was still missing:

- there were no HTML sign-in, sign-out, password-reset or second-factor
  pages, only JSON endpoints;
- a scaffolded page answered a signed-out browser with a JSON `401`, because
  there was no page to send it to;
- nothing created the first tenant and owner account, so a new application
  could not be signed into without hand-written SQL;
- there was no seed-data convention.

The security-critical behavior must not be re-implemented by each
application, and generated code must stay readable and editable.

## Decision

### Split: security in the framework, pages in generated code

`auth.Browser` (framework) performs every credential-changing step for an
HTML form: `SignIn`, `CompleteMFA`, `SignOut`, `RequestPasswordReset`,
`ResetPassword`. It calls the existing services with the socket peer as the
throttle source, maps their errors to a small set (`ErrCredentialsInvalid`,
`*LoginThrottledError`, `ErrMFAInvalid`, `ErrMFAChallengeMissing`,
`ErrRecoveryInvalid`, `ErrPasswordInvalid`, `*RecoveryThrottledError`), sets
`Retry-After` and `Cache-Control: no-store`, and sets or clears cookies
through the helpers the JSON cookie handlers now share (`startCookieSession`,
`endCookieSession`). A form sign-in therefore sets exactly the cookie-login
cookies (`__Host-forge_session`, a fresh `__Host-forge_csrf`), and session
creation is not forked.

The pages are generated application code, like Rails:
`app/adapters/htmlauth/htmlauth.go` decodes the forms with `web.DecodeForm`,
picks messages, renders `app/views/sessions/{new,mfa,_sign_out}` and
`app/views/passwords/{new,edit}`, and redirects with flash messages.
Applications may rewrite markup and wording; the generated package comment
says which calls must stay.

### Routes

| Route | Behavior |
| ----- | -------- |
| `GET /session/new` | form: organization (tenant slug), email, password; hidden validated `return_to` |
| `POST /session` | `303` to the validated return path (default `/`) with "Signed in."; `422` with one uniform message; `429` page with `Retry-After`; MFA accounts go to `/session/mfa` |
| `DELETE /session` | `_method=DELETE` form: revoke, clear cookies, `303` to the sign-in page |
| `GET`/`POST /session/mfa` | code form while a challenge is pending (only with an MFA service) |
| `GET /passwords/new`, `POST /passwords` | request a reset; the same `303` and notice whether or not the account exists |
| `GET /passwords/edit?token=`, `PATCH /passwords` | choose a new password; `422` for short or mismatched passwords |

### CSRF fails closed in the framework

`csrf.Verified(r)` is new: true only when `csrf.Protection` compared a
submitted token with the cookie token and they matched. Every `Browser`
method requires it and otherwise returns `403 csrf_invalid`. A route that is
exempted, mounted without the middleware, or reached with a JSON body (which
skips the token check) therefore cannot sign anyone in or out. The sign-in
form's token is also the login-CSRF defense: the old JSON cookie login needed
an HTTPS `Origin` because no CSRF cookie existed yet; the HTML form gets its
cookie when the sign-in page renders. The CSRF token is rotated at every
sign-in, so a token planted or observed before authentication is useless
afterwards.

### Tenant selection

The sign-in and reset forms have an explicit organization field holding the
tenant slug, prefilled from `?tenant=`. This matches `LoginService`, whose
contract is that every login names its tenant, and the tenancy rule that
there is no default or ambient tenant. Host-based selection (subdomains) was
rejected for now: Forge does not trust `Host` or forwarded headers for
security decisions without a trusted-proxy policy, and it would not work on
`localhost`. An application can hide the field and fill it from its own
routing. The failure message never says which of organization, email or
password was wrong.

### Return paths and redirects

`Middleware.AuthenticateBrowser(signInPath, next)` replaces
`AuthenticateCookie` for HTML pages. When the session cookie is missing,
invalid, revoked or expired and `web.PrefersHTML` is true, it answers `303`
to `signInPath?return_to=<path and query>` (GET and HEAD only; other methods
get the sign-in page without a return path, since the form cannot be
replayed). JSON and API clients keep the `401`, resolver failures stay `500`.
`auth.ReturnPath(candidate, fallback)` accepts only `web.IsLocalPath`
values: no scheme or host, no `//` or `/\` prefix, no backslash, control,
space or non-ASCII byte, at most 2048 bytes. The return path is validated
when the page renders it, when the form posts it, and before the redirect.

### Second factor

After a correct password for an account with TOTP, the challenge goes into
`__Host-forge_mfa` (`HttpOnly`, `Secure`, `SameSite=Strict`, `Path=/`,
expiring with the challenge), never into a URL, page or script, and no
session exists until `CompleteMFA` succeeds. A wrong code keeps the
challenge (the MFA throttle bounds attempts); a missing or malformed cookie
sends the user back to the sign-in form.

### Password reset links

The emailed link carries the token in the query (`recoverymail` default).
`GET /passwords/edit` checks its format only, moves it into a hidden field,
and marks the page `no-store`; Forge already sends
`Referrer-Policy: no-referrer`, and pages have no third-party resources. The
reset revokes every session of the account (PostgreSQL store) and clears the
browser's cookies. The reset form requires 12 characters, the same as
`setup`; the framework itself still accepts any non-empty password of at
most 4096 bytes, so existing JSON clients are unchanged.

### First owner: `setup`

`authpostgres.SetupCommand()` adds `app setup --tenant SLUG --email EMAIL
[--organization NAME] [--password-stdin]` (and `forge setup` in
development). `Repository.ProvisionOwner` creates the organization, tenant,
user, active membership and an `owner` role holding every permission in
`forge_permissions`, in one transaction under an advisory lock, with UUIDv7
IDs generated in Go and an `auth.identity.provisioned` audit event in the
same transaction. It refuses (`ErrAlreadyProvisioned`) when any tenant
exists: setup bootstraps an empty identity database and can never add an
account to, or take over, a running installation. Inviting users and
creating further tenants stay application features.

The password comes from `FORGE_SETUP_PASSWORD`, one line of piped standard
input (`--password-stdin`), or a terminal prompt typed twice with echo off
(`internal/terminal`, via `golang.org/x/sys`, already in the module graph).
It is never an argument (process lists and shell history expose those),
never printed or logged, and must be at least 12 characters.

The owner role is defined as holding every permission: setup grants all
registered permissions, and the migrations `forge generate resource` and
`forge generate scaffold` write grant their new permissions to every role
named `owner` (the down migration already removes the grants). A tenant
administrator who can create roles could already grant itself any
permission, so treating `owner` specially adds no privilege.

### Seeds

`postgres`, `mysql` and `sqlite` each provide `SeedCommand(SeedFunc)`:
`app seed` (`forge seed`) runs the application's `db.Seed` in one
transaction and commits only on success. Generated database applications get
an empty, documented `db/seeds.go`. Seeds must be idempotent; they are not a
place for accounts or credentials.

### Generation

`forge new -d postgresql` generates and wires the pages by default, as it
already wired the JSON identity routes: `bootstrap.Configure` builds
`auth.Browser`, one shared renderer and `htmlauth.Register`, and `main.go`
registers `setup` and `seed`. Other databases are unchanged except for
seeds. `forge generate authentication [--dry-run]` adds the same files
(`scaffold.AuthenticationFiles`, byte-identical) to an older PostgreSQL
application, refuses to overwrite, and prints the wiring. It refuses MySQL,
MariaDB, SQLite and database-less applications with an explanation: Forge
persists identities only in PostgreSQL today. The scaffold's printed cookie
guard now uses `AuthenticateBrowser("/session/new", ...)`.

## Consequences

- `forge new`, `forge migrate`, `forge setup`, `forge dev`, sign in, then
  `forge generate scaffold` works without hand-written code; the owner can
  use the scaffold's pages after `forge migrate`.
- Applications must keep `csrf.Protection` in front of the pages; without it
  sign-in fails closed with `403`.
- `forge.CommandEnv` gains `Stdin` and `LookupEnv`; `Execute` passes no
  standard input, `Main` passes `os.Stdin`.
- Generated applications send reset emails through `auth/recoverymail` when
  `FORGE_MAIL_URL`, `FORGE_MAIL_FROM` and `FORGE_AUTH_PASSWORD_RESET_URL` are
  set; otherwise `BrowserOptions.Recovery` is left empty, so the reset pages
  and the "Forgot your password?" link are not offered (security review
  follow-up). Mail without the reset URL fails at startup.
- `AuthenticateBrowser` marks authenticated responses `Cache-Control:
  no-store` before the handler runs.
- Not provided: user invitations and management pages, MFA enrollment
  pages, "remember me", host-based tenant selection, a signed-in indicator in
  the layout (the `sessions/_sign_out` partial is included where wanted), and
  identity persistence for MySQL, MariaDB or SQLite.
