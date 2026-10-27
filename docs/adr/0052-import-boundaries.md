# ADR 0052: Import boundaries: auth without HTTP, a leaf command package

- **Status:** Accepted
- **Date:** 2026-10-27 (quality audit, findings A-1 to A-5 and N3-08)
- **Amends:** [ADR 0006](0006-zero-dependency-core.md) and
  [ADR 0007](0007-postgresql-adapter-with-pgx.md) (the import rules of
  `architecture_test.go`), [ADR 0012](0012-cookie-sessions-and-csrf.md),
  [ADR 0026](0026-authentication-pages-and-setup.md) and
  [ADR 0045](0045-generated-mfa-settings-pages.md) (where the HTTP side of
  `auth` lives), [ADR 0023](0023-persistent-job-queue.md) (`queuecmd`),
  [ADR 0029](0029-hotwire-assets-and-live-updates.md) (`broadcast`'s imports)
- **Parity with:** Forge-Rust's crates already keep the domain apart from the
  HTTP adapter (the `forge-auth` crate depends on no HTTP crate); nothing to port.

## Context

The framework claims hexagonal boundaries, but five dependencies ran the
wrong way:

1. `auth`, the authentication domain, imported `csrf`, `i18n`, `openapi`,
   `web` and `net/http`: its handlers, cookies, browser flows and OpenAPI
   operations lived beside the services and ports (A-1).
2. Thirteen framework packages (`migrate`, the database adapters, the auth
   stores and setup command, the queue stores and `queuecmd`) imported the
   root `forge` package only for `forge.Command`, `forge.CommandEnv` and
   `forge.ErrUsage`: the composition root was a dependency of what it
   composes (A-2, N3-08).
3. `config` imported `i18n` to validate `FORGE_I18N_DEFAULT_LOCALE` (A-3).
4. `broadcast` imported `auth` to key its per-principal stream cap, so
   `views` → `broadcast` → `auth` (A-4).
5. The queue stores imported `queuecmd`, a command-line adapter (A-5).

## Decision

- **`auth/authhttp`** holds everything of `auth` that speaks HTTP: the bearer
  and cookie `Middleware`, `Require`, `RequireAudited`, `RequireCSRF`, the
  JSON handlers (`New*Handler`, `NewAudited*Handler`), `Browser` and
  `BrowserOptions`, `SignInStep`, `SignInCredentials`, `ReturnPath`, the
  cookie names, `ErrCSRFInvalid`, `ErrMFAChallengeMissing` and the OpenAPI
  operations. `auth` keeps the services, ports, errors and rules and imports
  none of `csrf`, `i18n`, `openapi`, `web` or `net/http`, directly or
  through another framework package. (It still links `net/http` through
  `fault`, which maps errors to HTTP statuses; that is the one exception.)
  - What the adapters reached into is now domain API: `MFAStepUpGate` (the
    throttled, audited password-and-code re-authentication before a
    two-factor change, which `Browser` used to run inline),
    `AuthorizeMFAChange(ctx, authorizer)` (the only way besides
    `VerifyMFAStepUp` to obtain an `MFAStepUpProof`: for the signed-in
    principal of `ctx` only, after the application's `MFAChangeAuthorizer`;
    it never takes a principal as an argument, so no caller can name another
    account),
    `ValidTOTPIssuer` and `PasswordWorkRetryAfter`.
  - **No aliases are kept in `auth`.** Every moved name returns an
    `http.Handler`, takes an `*http.Request` or is used only with them, and
    `authhttp` imports `auth`: an alias in `auth` would make `auth` import
    `authhttp`, an import cycle. Callers change the qualifier, as the
    generated applications, the routes seam and the docs do.
- **`command`** is a leaf package (standard library and `config` only) with
  `Command`, `Env` and `ErrUsage`. Framework packages import it; the root
  keeps `forge.Command`, `forge.CommandEnv` and `forge.ErrUsage` as aliases
  of the same types and error, so applications are unchanged. Only programs
  under `cmd/` and the example applications import the root.
- **`internal/langtag`** holds the locale grammar; `i18n.CanonicalLocale`
  and `config` share it, so `config` does not import `i18n`.
- **`internal/principalctx`** holds the request-context key of the
  authenticated principal. `auth.FromContext` reads it, `authhttp`'s
  middleware writes it, and `broadcast` keys `MaxPerPrincipal` by a small
  interface (`SubjectID() uuid.UUID`) that `auth.Principal` satisfies, so
  `broadcast` does not import `auth`. Being internal, applications cannot use
  it to forge a principal; within the framework only `auth`, `auth/authhttp`
  and `broadcast` may import it, and only `auth/authhttp` (the middleware,
  after resolving a session) may write it, so `AuthorizeMFAChange` sees only
  principals that signed in.
- **`auth/internal/backupcode`** is the backup-code format (generation,
  parsing, digest), used by `auth` and by the tests of `authhttp`.
- **`queue/internal/jobscmd`** implements the `jobs` command. The stores'
  `Commands()` and `queuecmd.Commands(opener)` both use it; neither the
  stores nor `queuecmd` depends on the other.

`architecture_test.go` enforces each direction (`TestImportRules`), with a
unit test of the checker, and that only `auth/authhttp` calls
`principalctx.With` (`TestOnlyTheAuthenticationMiddlewareWritesThePrincipal`);
reintroducing any of the forbidden imports or writers fails them.

## Consequences

- Applications that called the HTTP side of `auth` change
  `auth.NewMiddleware`, `auth.Require`, `auth.NewBrowser`,
  `auth.New*Handler`, `*auth.Middleware`, `*auth.Browser` and the like to
  `authhttp.…` and import `github.com/rocketinsidecorp/forge-go/auth/authhttp`.
  Applications generated before this change also have the old names in
  `app/bootstrap/identity.go`, `app/adapters/htmlauth` and
  `app/bootstrap/routes_gen.go`; [Upgrading in AUTHENTICATION.md](../AUTHENTICATION.md#upgrading)
  has the commands that rewrite them and re-seal the seam's checksum.
- `auth.AuthorizeMFAChange` takes no principal: code that called it with
  one passes only the request context.
- `forge.Command` and friends keep working; packages outside the framework
  that contribute commands may import `command` instead.
- `auth`'s tests that exercised HTTP moved with the code to `auth/authhttp`
  (all 220 tests and benchmarks remain); the floor tests of the step-up now
  drive `MFAStepUpGate` directly.
