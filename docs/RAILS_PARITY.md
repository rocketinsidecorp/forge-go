# Rails parity

Forge exists in two frameworks, Forge-Go and Forge-Rust, that ship the same
capabilities with the same commands, names and conventions (owner decision,
2026-10-20). This matrix tracks Ruby on Rails 8 capabilities and the places
where Forge intends to go further. A row is **Done** only when code, tests and
README commands exist on `main`; **In review** means a pull request is open or
the work is finished and waiting for its review; **Planned** means not started.

| Capability | Rails 8 | Forge-Go | Forge-Rust |
| --- | --- | --- | --- |
| New application with Docker and CI | `rails new` | Done | Done |
| Database selection | `rails new -d` | Done (PostgreSQL, MySQL, MariaDB, SQLite) | Done (PostgreSQL, MySQL, MariaDB, SQLite) |
| Server-rendered HTML, layouts, partials, forms, flash, CSRF | Action View | Done | Done |
| HTML + JSON CRUD generator | `rails g scaffold` | Done | Done |
| Authentication pages, password reset | `rails g authentication` | Done | Done |
| Sign-in through the HTML pages only by default; JSON login opt-in | (`rails new --api` for API-only apps) | Done (`forge new --api-auth`, `forge generate authentication --api-auth`; without the flag no JSON login is mounted) | Done (no JSON login exists; `--api-auth` not needed until one does) |
| Session idle timeout | (gems, e.g. Devise `timeoutable`) | Done (`FORGE_AUTH_SESSION_IDLE_TIMEOUT`, default 2h; `last_seen_at` written at most once a minute) | Planned (see Parity follow-ups) |
| CSRF token masked per render (BREACH) | `masked_authenticity_token` | Done (`csrf.Token`, `csrf.MaskToken`) | Planned (see Parity follow-ups) |
| First tenant and owner, seeds | `db:seed` | Done (`forge setup`, `forge seed`) | In review (`forge setup`, `forge seed`; the application commands `setup` and `seed` are Done) |
| Multi-tenancy with database enforcement | (gems) | Done (PostgreSQL RLS; `app_migrator`/`app_runtime` roles, staging and production refuse a serving role that could bypass RLS, tests run as the runtime role; [ADR 0046](adr/0046-postgresql-runtime-and-migration-roles.md)) | Done (PostgreSQL RLS; `app_migrator`/`app_runtime` roles, Forge-Rust ADR 0011; production refusal planned in [forge-rust#70](https://github.com/rocketinsidecorp/forge-rust/issues/70)) |
| TOTP MFA | (gems) | Done (sign-in code step and generated `/mfa` settings pages: enrollment, rotation, backup codes, disable; [ADR 0045](adr/0045-generated-mfa-settings-pages.md)) | Done (`FORGE_AUTH_MFA_KEY`, `/session/mfa`, generated `/mfa` settings pages; Forge-Rust ADR 0040) |
| Persistent jobs, cron, outbox | Solid Queue | Done | Done (framework crates; `forge new` wiring planned) |
| Mailer with previews | Action Mailer | Done | Done (`forge::mail`, password-reset emails, `/_mail` previews, `forge generate mailer`; Forge-Rust ADR 0035 and 0038) |
| Rate limiting | `rate_limit` | Done | Done |
| Client address behind trusted proxies | `request.remote_ip`, `trusted_proxies` | Done (`FORGE_HTTP_TRUSTED_PROXIES`, ADR 0037) | Done (`FORGE_HTTP_TRUSTED_PROXIES`, Forge-Rust ADR 0032; `forge deploy` sets it once ported) |
| Cache | Solid Cache | Done | Done |
| File storage with signed URLs | Active Storage | Done (disk) | Done (disk) |
| Import maps, Turbo, Stimulus | Hotwire | Done | Done |
| Turbo Streams over SSE | Action Cable | Done | Done (`scaffold --broadcast`, Forge-Rust ADR 0031) |
| Traces, metrics, log correlation | (gems) | Done (OpenTelemetry, Prometheus) | Done (OpenTelemetry, Prometheus; Forge-Rust ADR 0033) |
| Preconfigured Prometheus, Grafana, Tempo, Loki | — | Done (`compose.observability.yaml`, [ADR 0038](adr/0038-local-observability-stack.md)) | Done (`compose.observability.yaml`, Forge-Rust ADR 0034) |
| Zero-downtime deploys, rollback, remote logs | Kamal | Done (`forge deploy`, `forge logs`, `forge-proxy`) | Planned |
| Live reload and database lifecycle | `bin/dev`, `db:*` | Done (`forge dev`, `forge db`) | In review (`forge dev`, `forge db create\|drop\|reset\|setup\|prepare\|migrate\|rollback\|status\|seed`, `forge migrate\|rollback\|setup\|seed`, `forge doctor`, `forge version`; `forge routes`, `forge db schema:dump\|schema:load\|console`, browser reload and `.env` files are still open; Forge-Rust ADR 0036) |
| Rollback never reverts framework migrations; disabled framework sets tolerated | (engine migrations copied into the app) | Done (`rollback`, `Migrator.DownFramework`, ADR 0039) | Done (`rollback`, `rollback_framework`, ADR 0030) |
| Encrypted credentials, attribute encryption | `credentials:edit`, AR Encryption | Done (`forge credentials`, `encryption`; PostgreSQL tenant tables rotate per tenant with `postgres.WithTenant`, unscoped runs under row-level security are refused) | Done (`forge credentials init\|edit\|show\|rotate-key`, `forge::credentials`, `forge::encryption`; PostgreSQL tenant tables rotate per tenant with `EncryptedColumn::tenant`, unscoped runs under row-level security are refused; forge-rust#37, Forge-Rust ADR 0041) |
| i18n | I18n | Done (`config/locales/*.json`, `t`, CLDR plurals, `forge new --locale`, `forge i18n check`, [ADR 0042](adr/0042-internationalization.md)) | Done (`config/locales/*.json`, `layout.i18n.t`, CLDR plurals, `forge new --locale`, `forge i18n check`, Forge-Rust ADR 0037) |
| Locale path prefix removed before every middleware (no path-guard bypass in any order) | — | In review (`httpserver.Rewriter`, forge-go#56) | In review (`Middleware::rewrite`, forge-rust#69) |
| OpenAPI from typed routes | — | Done (`openapi.Describe`, `openapi.Typed`, `GET /openapi.json`, `forge openapi [--output\|--check]`, `FORGE_OPENAPI_ENABLED`, [ADR 0043](adr/0043-openapi-from-typed-routes.md)) | Done (`#[derive(OpenApiSchema)]`, `forge::openapi::typed`/`describe`, `GET /openapi.json`, `FORGE_OPENAPI_ENABLED`, `forge openapi`, Forge-Rust ADR 0039) |
| Controller, model, mailer and job generators, `destroy` | `rails g`, `rails d` | Done (`forge generate model\|controller\|mailer\|job --queue`, `forge destroy`, [ADR 0041](adr/0041-generators-and-destroy.md)) | Done (`forge generate model\|controller\|mailer\|job --queue`, `forge destroy`, Forge-Rust ADR 0038) |
| Continuous fuzzing, SBOM, signed images | — | Done (every fuzz target per PR and nightly, `scripts/fuzz.sh`; CycloneDX/SPDX SBOMs; keyless cosign-signed images from `v*` tags; [SUPPLY_CHAIN.md](SUPPLY_CHAIN.md), [ADR 0044](adr/0044-continuous-fuzzing-sbom-signed-images.md)) | Planned |
| Published benchmarks | — | In review (`go test -bench`, `scripts/bench.sh`, [BENCHMARKS.md](BENCHMARKS.md)) | Planned |
| Getting Started guide | Rails Guides | Done ([GETTING_STARTED.md](GETTING_STARTED.md)) | Planned |

## Parity follow-ups

- Two-factor settings hardening (forge-go#32, from the review of the `/mfa`
  pages): Forge-Go now refuses `POST /mfa` and `POST /mfa/confirm` for an
  account that already has a factor before it checks the password (a wrong and
  a right password got `422` and `303`), refuses `ConfirmTOTPEnrollment` of an
  account with a factor explicitly (no 500 from the primary key), and the
  generated pages tell the user to wait for the next code or use a backup code
  after starting a rotation (`mfa.show.rotate_hint` in `en`, `es`, `pt-BR`).
  Forge-Rust's generated `step_up` has the same password-oracle ordering and
  its pages lack the hint (its `docs/mfa.md` has the sentence); its store's
  confirmation needs the same explicit-refusal review. Forge-Rust adopts all
  three in its next pull request that touches `/mfa`, tracked in
  [forge-rust#36](https://github.com/rocketinsidecorp/forge-rust/issues/36).

- Stream signing key rotation (forge-go QG4b, forge-rust#14): Forge-Go's
  generated `htmlstreams.NewSigner` now also reads `FORGE_STREAMS_PREVIOUS_KEYS`
  (comma-separated base64 keys, at most `broadcast.MaxPreviousKeys` = 3, never
  echoed in an error), the name Forge-Rust's parity doc already proposes, so
  rotating `FORGE_STREAMS_KEY` no longer breaks open pages. Forge-Rust's
  `Signer::new(key, previous)` already verifies previous keys, but its
  generated applications read `FORGE_STREAMS_KEY` only: it adds the variable
  with the same name, separator, limit and redaction (tracked in
  [forge-rust#14](https://github.com/rocketinsidecorp/forge-rust/issues/14)).
  Until then the Rust rotation procedure is in its
  [deployment](https://github.com/rocketinsidecorp/forge-rust/blob/main/docs/deployment.md#rotating-the-stream-signing-key).

- forge-proxy connection and header limits (forge-go QG4b, audit S-8):
  Forge-Go's `forge-proxy run` has `-max-header-bytes` (default 65536, 4096 to
  1048576, `431` above it; was 1 MiB) and `-max-connections` (default 10000
  per listener, admission before accept). Forge-Rust caps a listener at the
  same 10,000 connections with a constant and sets hyper's `max_buf_size` to
  1 MiB: it adopts both flags with the same names, defaults and range and the
  64 KiB header default (no Forge-Rust
  issue exists yet: open one when this lands). Neither framework exposes them in
  `config/deploy.yml`.

- Benchmarks: [BENCHMARKS.md](BENCHMARKS.md) defines the scenario names, the
  variants and the output format of both suites; Forge-Rust adopts them in
  its own benchmark pull request (tracked in
  [forge-rust#41](https://github.com/rocketinsidecorp/forge-rust/issues/41)). Its first results table must use the same
  scenarios and the same columns, so the two can be read side by side.

- Deploy hardening (forge-go#36, from the review of forge-rust#39):
  `ssh.proxy_jump` and `registry.server` refuse a leading `-` with the same
  patterns in both frameworks, `retain` must be 2 or more (1 pruned the
  previous version, so rollback was impossible), and the deploy lock is
  released only while its record is the deploy's own, with a record-less lock
  older than a minute recovered without `--force`. One difference is
  deliberate: Forge-Rust refuses YAML nesting deeper than 100 levels (and a
  `deploy.yml` over 1 MiB) with a line-numbered error, while Forge-Go relies
  on yaml.v3's own maximum depth (10,000 levels) and reads the file without a
  size cap. Forge-Rust mirrors the lock scripts and the four golden plan
  files in `internal/deploy/testdata` and the `retain` minimum, tracked in
  [forge-rust#40](https://github.com/rocketinsidecorp/forge-rust/issues/40).

- Locale path prefix and path guards (forge-go#56, the Go side of
  forge-rust QR1): both frameworks remove the `PathPrefix` locale segment
  once per request before the first application middleware runs, so a path
  guard registered before or after the negotiation sees `/admin` for
  `/pt-BR/admin` and `/PT-br/admin/users`, and `/pt-BR/en/admin` is routed as
  `/en/admin`. Both keep the path as sent in request telemetry, and both
  name the negative test after the same guarantee
  (`a_locale_prefix_cannot_bypass_a_path_guard_registered_before_the_negotiation`,
  `TestALocalePrefixCannotBypassAPathGuardRegisteredBeforeTheNegotiation`).
  The mechanism follows each language: Forge-Rust's router calls every
  layer's `Middleware::rewrite` hook; in Forge-Go the i18n middleware's
  handler implements `httpserver.Rewriter`, which `forge.App` and
  `httpserver` collect when they compose the chain (`App.Use`,
  `forge.WithHTTPMiddleware`). One difference is deliberate: Forge-Rust
  reads the segment from the escaped path, so `/pt-BR%2F..%2Fadmin` has no
  prefix (and is not found); Forge-Go matches the decoded segment as before,
  leaving `/../admin`, which its router refuses with `400`. Neither serves
  the guarded page.
- RLS defense in depth (QG1, audit findings S-1 and S-2,
  [ADR 0046](adr/0046-postgresql-runtime-and-migration-roles.md)): Forge-Go
  now generates Forge-Rust's development roles, `app_migrator` and
  `app_runtime` with their names as passwords, from
  `docker/postgres/init.sql` mounted as
  `/docker-entrypoint-initdb.d/001-forge.sql`, and gives
  `FORGE_MIGRATION_DATABASE_URL` only to the `migrate` container. Forge-Go
  goes further in three ways Forge-Rust adopts: outside development and
  test `postgres.RequireRowSecurity` refuses a serving role that logs in as
  or acts as a superuser, a `BYPASSRLS` or `CREATEROLE` role or a member of a
  superuser or `BYPASSRLS` role, or whose URL switches it to another role
  (Forge-Rust's bootstrap has no check yet); the generated repository tests connect as the runtime role
  (`postgrestest.NewRLS`; Forge-Rust's `tests_repository.rs.tmpl` switches
  to a probe role inside the superuser's transaction); and every PostgreSQL
  resource gets a test that the runtime role cannot run DDL, temporary tables
  included (Forge-Go's `init.sql` also revokes `TEMPORARY` and grants
  sequences `USAGE, SELECT` only). Differences
  kept deliberately: without `FORGE_MIGRATION_DATABASE_URL`, Forge-Go's
  `migrate` falls back to `FORGE_DATABASE_URL` (Forge-Rust requires it), and
  the variable's database is ignored as `forge db` already did; Forge-Go puts
  the schema and default privileges in `template1` instead of replaying
  `init.sql` after `forge db reset`; its Compose health check probes TCP
  (open only after `init.sql`) instead of querying the runtime role's schema
  privilege; the Compose superuser's password is `development`. Forge-Rust
  adopts the refusal and both tests in
  [forge-rust#70](https://github.com/rocketinsidecorp/forge-rust/issues/70).
  `forge deploy` gives the migration credential to the migrate step alone
  through `migrate.env`
  ([forge-go#57](https://github.com/rocketinsidecorp/forge-go/issues/57));
  Forge-Rust's `forge deploy` has no `migrate.env` yet (tracked in [forge-rust#73](https://github.com/rocketinsidecorp/forge-rust/issues/73)).

- Security defaults (QG4a, audit findings S-3, S-4, S-7 and PF-3). Forge-Go
  now: ends a session after `FORGE_AUTH_SESSION_IDLE_TIMEOUT` without a
  request (default `2h`, `5m` to `720h`; migration
  `20261026090000_session_idle_timeout` adds `forge_sessions.last_seen_at`,
  written at most once a minute per session by a conditional `UPDATE`, sessions
  open at the upgrade count as used then); generates only the HTML sign-in
  flow, with the JSON authentication APIs opt-in through `forge new
  --api-auth` (and `forge generate authentication --api-auth`); embeds the
  CSRF token masked with a fresh one-time pad on every render (Rails'
  format: pad then token XOR pad, base64url, 86 characters), accepting the
  canonical token too (scripts read it from the cookie; pages rendered before
  the upgrade embed it, a transition for one release); and logs a warning when
  a production process (`FORGE_ENV=production`) builds its sign-in, MFA or
  password-reset service on the in-memory throttler (generated applications
  already use the PostgreSQL throttler, as Forge-Rust's do since QR2).
  Forge-Rust status and follow-ups, tracked in
  [forge-rust#75](https://github.com/rocketinsidecorp/forge-rust/issues/75):
  (1) the same idle timeout, variable name and default in its generated
  session store (a `last_seen_at` column in a new migration, the same
  once-a-minute write); (2) masked CSRF tokens from `csrf_field`/`csrf_meta`
  with the same encoding, so a page can be served by either framework;
  (3) the production warning for `MemoryLoginThrottle` in custom wiring.
  `--api-auth` needs nothing in Forge-Rust while it has no JSON login; if it
  gains one, it is opt-in under the same flag name.

## Review gate

Every pull request passes the Claude Code Review before it merges to `main`,
including a screen for malicious or exfiltrating code: unexpected network
calls, access to credentials, wallets or crypto keys, obfuscated code, build
scripts and new dependencies.
