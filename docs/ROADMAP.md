# Roadmap

Phases are sequential in intent but may overlap. Status reflects the current
branch. Nothing in a phase marked *planned* is implemented.

| Phase | Scope                                              | Status  |
| ----- | -------------------------------------------------- | ------- |
| 0     | Architecture, ADRs, threat model                   | Done    |
| 1     | Core runtime, config, CLI, lifecycle, HTTP         | Done    |
| 2     | Databases, migrations, repositories, transactions  | Done    |
| 3     | Authentication, authorization, tenancy             | In progress |
| 4     | Jobs, events, outbox, scheduling                   | Done    |
| 5     | OpenTelemetry                                      | Done    |
| 6     | OpenAPI and tooling                                | Planned |
| 7     | AI API and CLI subsystem                           | In progress |
| 8     | Code generation                                    | Planned |
| 9     | Performance                                        | Planned |
| 10    | Security hardening                                 | Planned |

## Phase 0 - Architecture

Architecture overview, deployment guide, threat model, security policy, and
ADRs 0001-0006.

## Phase 1 - Core runtime (done)

- `config`: environment loading, defaults, fail-fast validation, production
  transport rule.
- `router`: `ServeMux` patterns, freeze on first request, traversal and
  encoded-slash rejection.
- `httpserver`: timeouts, body limit, security headers, HSTS, `X-Request-ID`,
  panic recovery, graceful shutdown.
- `health`: readiness registry and `/health`, `/health/live`, `/health/ready`.
- `uuid`: monotonic RFC 9562 UUIDv7 with JSON, text, and SQL support.
- `fault`: typed application errors.
- `forge.Main` / `forge.Execute`: conventional entrypoint with `serve` and
  `healthcheck` subcommands, environment-dependent log format, signal
  handling.
- CLI: `new`, `dev`, `build`, `version`, `uuid`, `doctor`.
- `forge new`: application skeleton with Dockerfile, Compose, and CI.

## Phase 2 - Databases (done)

Database:

- `config.Database` with `FORGE_DATABASE_*` variables; the URL is a
  redacted `config.Secret`; `production` requires TLS to the database unless
  explicitly allowed.
- `postgres` adapter on pgx v5 ([ADR 0007](adr/0007-postgresql-adapter-with-pgx.md)):
  pool with statement timeout and startup ping, `DBTX` compatible with sqlc,
  `InTx`/`InTxWith` with isolation levels and retries on serialization
  failures and deadlocks, error translation to `fault`, transaction-scoped
  advisory locks, and the `Pool()` escape hatch.
- SQL migrations embedded in the binary
  ([ADR 0008](adr/0008-sql-migrations-embedded-in-the-binary.md)):
  checksums, advisory lock, per-migration transactions with a
  `no-transaction` opt-out, fail-closed checks, `migrate`, `migrate status`,
  and `rollback` subcommands.
- `pagination`: UUIDv7 keyset pagination with opaque cursors.
- `postgrestest`: a fresh database per test; CI mode that fails instead of
  skipping.
- Core/adapter boundary enforced by `architecture_test.go`.
- CLI: `forge migrate [status]`, `forge rollback [-steps N]`,
  `forge generate migration NAME`, `forge test`, `.env.development` /
  `.env.local` loading, `forge doctor` showing the database target without
  credentials.
- `forge new` generates PostgreSQL by default (`--skip-database` to omit it):
  database wiring, readiness check, embedded migrations directory, Compose
  stack with PostgreSQL 18 and a one-off `migrate` service, and a CI
  PostgreSQL service.
- `examples/basic-api`: a task API showing repositories, transactions,
  optimistic locking, and pagination.
- Documentation: [DATABASE.md](DATABASE.md).

Multiple databases, completed before Phase 3
([ADR 0009](adr/0009-multiple-databases.md)):

- `forge new --database postgresql|mysql|mariadb|sqlite|none` (`-d`), with
  per-database Compose service, CI service, development settings, and
  Dockerfile (a nonroot storage volume for SQLite).
- Adapter selection by the `FORGE_DATABASE_URL` scheme (`postgres://`,
  `mysql://`, `sqlite:`), with production TLS rules for PostgreSQL and MySQL.
- Standard-library core packages shared by all adapters: `sqldb` (one error
  translation for every database, retries, bounded close, `database/sql`
  wrapper), `migrate` (the migration engine, moved out of `postgres`, with a
  `Dialect` interface and a portable bookkeeping table), and `dbtest`.
- `mysql` adapter for MySQL 8.4 and MariaDB 11.8: hardened driver
  configuration, strict session, flavor-specific statement timeout,
  `GET_LOCK` migration lock.
- `sqlite` adapter on the pure-Go modernc.org/sqlite driver: enforced
  pragmas, operating-system file lock for migrations.
- A uniform adapter surface (`Open`, `Translate`, `NewMigrator`, `Commands`,
  and `New`/`NewMigrated`/`Config`/`NewWithConfig` test helpers); readiness
  check named `database` in generated applications.
- CI: tests against PostgreSQL 18, MySQL 8.4, and MariaDB 11.8, and a Docker
  smoke test of a generated application per database.

Core changes made alongside:

- `web`: JSON responses, a single error body
  (`{"error":{"code","message","request_id"}}`), strict JSON decoding,
  request ID and request-scoped logger in the context.
- `httpserver`: request ID and logger set before panic recovery; recovered
  panics, oversized bodies, and request-ID failures answer in the JSON error
  format; requests still running at the shutdown deadline have their
  connections closed, cancelling their contexts.
- `router`: JSON `400`, `404` and `405` (with `Allow`) responses; atomic
  freeze flag.
- `fault`: category default statuses (`Status()`), `From`, and `Is` by code.
- `health`: per-check timeout (2 seconds by default).
- `forge`: `Configure` receives a context; `App.Config`, `App.Logger`,
  `App.OnShutdown`, `App.Close`; one shutdown budget shared by the HTTP drain
  and the hooks; application-defined subcommands (`forge.Command`, with
  `forge.ErrUsage` for exit status 2) and `help`.
- `forge new` adds `.github/dependabot.yml`.

## Known gaps

Not scheduled in a specific phase unless noted.

- `X-Forwarded-For` / `X-Forwarded-Proto` are not interpreted; handlers see
  the proxy's address behind `trusted-proxy`. `ratelimit.ByRemoteAddr`
  therefore puts every client behind a proxy in one bucket: key by an
  authenticated value instead ([RATE_LIMITING.md](RATE_LIMITING.md)).
- Readiness results are not cached; every probe runs the checks.
- Matched requests are looked up in `ServeMux` twice (once to detect
  unmatched routes); tracked for Phase 9.
- MySQL/MariaDB/SQLite tenant isolation is enforced at the application layer;
  only PostgreSQL currently has the built-in RLS transaction helper.
- Soft delete and audit fields (`created_by`, `updated_by`, `deleted_at`)
  have no framework support yet; applications add them in their own SQL.
- Keyset pagination orders by UUIDv7 only; there is no cursor for other sort
  orders.
- `forge migrate` and `forge rollback` require a single command under
  `cmd/`.
- MySQL/MariaDB TLS verifies against the system certificate pool only; a
  private CA cannot be configured.
- SQLite applications run as a single instance on a single host.
- Application-level lock helpers exist for PostgreSQL only.

## Phase 3 - Identity and tenancy

Implemented foundation:

- opaque 256-bit bearer tokens with digest-only persistence;
- a resolver port that must load current session, subject, membership and
  permissions on every request;
- strict bearer middleware and deny-by-default `resource:action` permissions;
- fail-closed typed tenant context;
- PostgreSQL `InTenantTx` with a transaction-local RLS setting and a real
  cross-tenant integration test;
- versioned PBKDF2-HMAC-SHA256 password hashing using Go's standard library;
- PostgreSQL user/organization/tenant/membership/role/permission/session schema;
- a concrete PostgreSQL session repository that implements `auth.Resolver`,
  recomputes current permissions, and supports per-session and user-wide
  revocation;
- tenant-scoped email/password login with uniform authentication failures,
  dummy-hash timing equalization, password-work-factor upgrades, and thin
  login/logout HTTP adapters;
- account/source login throttling with a bounded in-memory default, HTTP 429
  plus Retry-After, and a shared PostgreSQL adapter with bounded pruning
  ([ADR 0011](adr/0011-login-throttling.md));
- explicit browser-cookie sessions with secure `__Host-` cookies, same-origin
  login protection, double-submit CSRF validation, CSRF-protected logout, and
  no-store login responses
  ([ADR 0012](adr/0012-cookie-sessions-and-csrf.md));
- structured security-audit event contract plus append-only PostgreSQL
  persistence with request-ID correlation and no arbitrary secret-bearing
  metadata ([ADR 0016](adr/0016-structured-security-audit.md));
- one-time password recovery with digest-only tokens, non-enumerating request
  responses, global session revocation on reset, and bounded cleanup
  ([ADR 0013](adr/0013-password-recovery.md));
- replay-safe TOTP MFA with encrypted seeds, short-lived digest-only challenges,
  one-time backup codes, factor rotation/disable, credential-version-bound
  session creation, and bearer/browser completion flows
  ([ADR 0014](adr/0014-totp-mfa.md), [ADR 0015](adr/0015-mfa-factor-lifecycle.md)).
- PostgreSQL identity wiring in generated applications, including migrations,
  login/logout/session routes, optional MFA challenge completion, and the
  `FORGE_AUTH_MFA_KEY` configuration.
- Generated PostgreSQL applications include bearer and secure-cookie login,
  logout and session routes, plus password-recovery request/reset routes.
  Recovery delivery deliberately fails closed until an application installs a
  `RecoverySender`; the generated placeholder must never claim delivery or log
  recovery tokens.
- Structured audit events for login, logout, password recovery/reset, MFA
  challenge and factor lifecycle operations. Audit persistence is opt-in for
  framework services; generated PostgreSQL login, MFA, and logout flows wire
  the repository as their auditor.
- Optional authentication middleware and `RequireAudited` hooks for rejected
  valid sessions and permission denials. These are opt-in because each rejected
  valid token can cause a database write; deployments should limit request
  volume and plan retention/capacity.

- Milestone M3, authentication you can see
  ([ADR 0026](adr/0026-authentication-pages-and-setup.md)): generated
  PostgreSQL applications ship Rails-style sign-in, sign-out, two-factor and
  password-reset pages (`app/adapters/htmlauth`, `app/views/sessions`,
  `app/views/passwords`) on top of `auth.Browser`, which keeps CSRF
  verification (`csrf.Verified`, fail closed), uniform failures, throttling,
  cookies and return-path validation in the framework;
  `Middleware.AuthenticateBrowser` redirects signed-out browsers to the
  sign-in page while API clients keep `401`; `forge generate authentication`
  adds the pages to older PostgreSQL applications and refuses other
  databases; `app setup` / `forge setup` provisions the first organization,
  tenant and owner role once, audited, with the password never in argv; and
  `app seed` / `forge seed` runs an idempotent `db.Seed` in one transaction
  for every database.

Remaining boundaries: inviting further users and creating more tenants are
application features; password-recovery delivery needs a configured sender;
MFA enrollment/rotation still requires an application-specific step-up policy
and has no generated pages; and tenants are chosen with an explicit field,
not by host name.
Those framework primitives require application-specific decisions and are documented in
[AUTHENTICATION.md](AUTHENTICATION.md) and [MFA.md](MFA.md). PostgreSQL is the
only built-in identity persistence adapter and the only adapter with a
database-enforced RLS helper. MySQL, MariaDB, and SQLite use the documented
application-layer tenant-scoping contract; Forge does not claim equivalent
database enforcement ([MULTI_TENANCY.md](MULTI_TENANCY.md)). Broader audit
coverage for missing/malformed credentials and infrastructure failures during
session resolution is intentionally omitted to avoid persisting unbounded
untrusted input or coupling audit availability to resolver outages.

## Phase 4 - Asynchronous work

The event envelope, atomic PostgreSQL persistence, and bounded concurrent
dispatcher with renewable leases, token-fenced acknowledgements, exponential
retry, and a terminal dead-letter state are implemented. Dead-letter replay,
queue metrics (`Dispatcher.Stats`, `ListDeadLetters`), an in-process job
scheduler (`jobs.Scheduler`), and generated-application wiring are complete.
See [ADR 0018](adr/0018-transactional-outbox-foundation.md),
[ADR 0019](adr/0019-outbox-dispatch.md), and
[ADR 0020](adr/0020-background-jobs-scheduling.md).

## Phase 5 - Observability

Foundation:

- [x] Optional application HTTP middleware composition through
  `forge.WithHTTPMiddleware`, with request-ID context, body limits, panic
  recovery, and security headers preserved.

Implemented on the standard library only
([ADR 0034](adr/0034-dependency-free-telemetry.md), [OBSERVABILITY.md](OBSERVABILITY.md)):

- [x] OpenTelemetry server spans and trace-context propagation: strict,
  fuzz-tested W3C `traceparent`/`tracestate`, parent-based ratio sampling,
  route-pattern span names, a propagating `http.Client` transport, and OTLP/HTTP
  JSON export with bounded batching, retries and shutdown flush.
- [x] Bounded-cardinality HTTP metrics and a documented export path:
  `http.server.request.duration` by method, route pattern and status class,
  in-flight gauge, per-instrument series cap, Prometheus text on a separate
  listener (`FORGE_TELEMETRY_METRICS_ADDR`) and OTLP metric push.
- [x] Correlate structured logs with active trace/span identifiers: request
  loggers and `*Context` log calls carry `trace_id`/`span_id`.
- [x] Database and background-job instrumentation without coupling the
  standard-library core to an exporter: `db.client.operation.duration` and
  statement spans for PostgreSQL (pgx tracer), MySQL/MariaDB and SQLite
  (`database/sql` driver wrapper), queue worker/enqueue, outbox dispatcher,
  scheduler and mail spans and histograms; disabled telemetry adds zero
  allocations.

Not included: OTLP gRPC/protobuf, logs export, baggage, exemplars, and
stored enqueue context for jobs (linked by job ID instead).

## Phase 6 - API tooling

OpenAPI description and related tooling.

## Phase 7 - AI subsystem

An API and CLI surface for AI-driven operations. Every action passes through a
fixed pipeline:

```text
Intent -> Policy -> Validation -> Authorization -> Execution -> Audit
```

No step may be skipped, and execution never runs with more privilege than the
authorizing principal.

AI-friendly developer experience checklist:

- [x] `forge help --json`: versioned command/flag schema and declared
  side-effect categories; covered by `TestRunHelpJSONProvidesCompleteCommandCatalog`.
- [x] `forge doctor --json`: schema-versioned diagnostics with stable codes
  and remediation, tested for valid/invalid configuration and secret redaction.
- [x] Extend the catalog with per-command options, copyable examples,
  environment-file/variable inputs and process/command exit codes.
- [x] `forge inspect --json`: report module, Go/framework versions, database adapter,
  migrations, routes and enabled first-party modules without executing writes.
- [x] Structured CLI errors: stable codes and safe context, with human output
  remaining the default and exit codes documented.
- [x] `forge generate migration|view NAME --dry-run`: show exact planned files
  without creating directories or files; existing outputs are refused.
- [x] Provider-neutral `agent.Engine`: only registered actions run, and each
  call passes policy, input validation, exact principal permission, execution,
  and mandatory journal steps. Destructive/external effects additionally
  require the application's explicit approval adapter.
- [x] PostgreSQL execution journal persists an attempt before effects,
  rejects tenant/actor-scoped intent replay, omits prompts and payloads, and
  protects records with RLS. Uncertain post-execution outcomes stay visible.
- [x] Bounded, tenant-scoped reconciliation listing for reserved and unresolved
  agent executions; requires the explicit `agent:reconcile` permission and
  returns metadata only. Applying a reconciliation remains application-owned.
- [x] Record one operator decision for an explicitly unresolved execution in a
  separate tenant-RLS-protected table; serialize concurrent decisions and never
  mutate the original journal row or automatically retry the action.
- [ ] Define action-specific worker-liveness/fencing for executions left
  `reserved` after process crashes. A lease expiry alone cannot prove that an
  executor or external operation stopped; current API deliberately refuses
  generic resolution or retry of these rows.
- [ ] First-party execution journals and migration wiring for MySQL, MariaDB
  and SQLite; provider runtime and generated agent actions are also not included.
- [x] Agent-oriented CLI guide: quickstart, copyable commands, machine-readable
  catalog/error references and explicit database/security limits
  ([AI_AGENT_GUIDE.md](AI_AGENT_GUIDE.md)).
- [x] CLI contract tests prove stable JSON error output, secret redaction and
  that `forge inspect --json` does not modify project files.
- [x] Agent tests prove policy/validation/permission/approval/journal ordering,
  refusal of unauthorized actions and replay, tenant isolation, and surfaced
  uncertain outcomes. The CLI remains outside this authorization boundary.

## Phase 8 - Code generation

`forge generate resource` and related generators that create hexagonal layers
(`app/domain`, `app/application`, `app/adapters`, `app/infrastructure`) only
when they have code to hold. `forge generate migration` already exists.

Generator checklist (planned unless an implementation and its tests prove
otherwise):

- [x] `forge generate resource NAME field:type...`: coherent migration,
  validation, persistence, HTTP and tests for PostgreSQL, MySQL, MariaDB and
  SQLite; no empty layers or public CRUD by default
  ([ADR 0021](adr/0021-resource-generator.md)).
- [x] Explicit resource scope: `--tenant-scoped` versus `--global`, with the
  correct database-specific isolation pattern (forced RLS on PostgreSQL,
  statement predicates elsewhere) and cross-tenant tests, including a
  non-superuser RLS test on PostgreSQL.
- [x] Resource-generator `--dry-run` and refuse-overwrite behavior with stable,
  reviewable output (all-or-nothing; the wiring for `bootstrap.go` is printed,
  never applied).
- [x] `forge generate job NAME`: cancellation-aware job stub and generated
  tests; docs cover bounded scheduler concurrency, shutdown context,
  idempotency across replicas, and the absence of automatic retries.
- [x] `forge generate view [DIR/]NAME`: server-rendered Go HTML page in the
  shared view tree (`app/views/DIR/NAME.gohtml`, rendered inside the
  application layout) with contextual escaping and buffered execution; adds
  the views base `forge new` generates when the application has none; no
  database write semantics.
- [x] `forge generate scaffold NAME field:type...`: the resource plus
  server-rendered HTML CRUD (index/show/new/edit/create/update/destroy), forms
  mapped from field types, `422` field errors, flash notices, `303`
  redirects, CSRF, `_method`, optimistic-lock `409`, and generated handler,
  CSRF, permission and template tests; guarded like the JSON routes, never
  public ([ADR 0021](adr/0021-resource-generator.md), addendum).
- [ ] SQL view generators remain separate from server-rendered HTML and need
  explicit database-dialect and tenant-isolation contracts.
- [ ] Generated applications compile, vet, test and pass database/Docker
  smoke checks for every supported profile. (The end-to-end test generates a
  tenant-scoped and a global resource per database, wires them as printed, and
  runs vet, build, the generated tests and migrate/rollback; Docker smoke
  checks run in CI.)

## Developer loop (Rails parity)

[ADR 0035](adr/0035-development-loop.md); commands in the
[README](../README.md#development-loop).

- [x] M4: `forge dev` watches Go sources, templates, assets, migrations and
  `.env.*` files with a bounded polling watcher, rebuilds, keeps the old process
  serving until the build succeeds, restarts it gracefully (SIGTERM, wait) and
  reports build errors without stopping the application; `--no-reload`,
  `--poll-interval`, `--debounce`.
- [x] M4: browser reload in development only, through a proxy inside the forge
  CLI that injects a same-origin script (CSP nonce when needed) and streams
  reload and build-error events; nothing is compiled into applications.
- [x] M4: `forge db create|drop|reset|setup|prepare|migrate|rollback|status|
  seed|schema:dump|schema:load|console` for PostgreSQL, MySQL/MariaDB and
  SQLite, with a production guard, credentials kept out of arguments and a
  deterministic `db/schema.sql`.
- [ ] Partial hot swap for template and asset changes without a rebuild;
  `forge stats`; a Procfile-style `bin/dev` that also runs background
  processes.

## Server-rendered HTML (Rails parity)

Rails-style HTML CRUD on the standard-library core
([ADR 0025](adr/0025-server-rendered-html.md), [VIEWS.md](VIEWS.md)).

- [x] G1a: `views.Load` with layouts, partials and one template set per page;
  CSRF, flash, method-override, DOM id and `dict` template helpers; strict
  page Content-Security-Policy.
- [x] G1a: `web.DecodeForm` with typed field errors for `422` re-rendering,
  local-only `303` redirects, content negotiation, flash messages and method
  override.
- [x] G1a: `csrf` protection for HTML forms sharing the cookie-login token,
  with cross-origin rejection; negative tests for missing, wrong, query-string,
  malformed, cross-site and oversized cases.
- [x] G1a: `Router.Resources`, `App.Use`, `forge routes`; `forge new`
  generates the layout, flash partial, stylesheet, views package and
  middleware wiring for every database option, and a test compiles, vets,
  lints and tests generated applications.
- [x] G1b: `forge generate scaffold` (HTML resource on top of the above); a
  test generates a tenant-scoped and a global scaffold per database, wires
  them as printed, and vets, builds, lints and tests the application.
- [x] M5 Hotwire ([ADR 0029](adr/0029-hotwire-assets-and-live-updates.md)):
  digested embedded assets and an import map allowed by CSP hash, vendored
  integrity-pinned Turbo 8.0.23 and Stimulus 3.2.2, `app/javascript` with a
  hello controller, Turbo Stream responses (`turbo`), real-time broadcasts over
  Server-Sent Events with HMAC-signed, tenant-scoped, expiring channel names
  (`broadcast`), PostgreSQL LISTEN/NOTIFY fan-out across replicas
  (`broadcast/postgres`), and `forge generate scaffold --broadcast`.
- [ ] Hotwire follow-ups: replay of missed messages, WebSocket transport,
  Turbo Frame helpers, morphing/page refreshes, a controller generator.
- [ ] Multipart HTML forms (`csrf.Protection` reads urlencoded bodies only;
  upload routes send the token in `X-CSRF-Token`, see
  [STORAGE.md](STORAGE.md)), per-page layout selection. (The upload helper and file storage themselves are done; see
  "Runtime services".)

## Runtime services (Rails 8 parity)

Rate limiting, application cache and file storage on the standard-library core,
each with shared stores for PostgreSQL, MySQL/MariaDB and SQLite (rate limit and
cache) and a conformance suite
([ADR 0031](adr/0031-rate-limiting.md), [ADR 0032](adr/0032-cache.md),
[ADR 0033](adr/0033-file-storage.md)).

- [x] `ratelimit`: fixed-window `Limiter`, bounded in-memory store with no
  background goroutines, HTTP middleware (`429`, `Retry-After`, JSON or HTML),
  per-route and per-scope use, fail-closed by default; shared
  `ratelimit/postgres`, `ratelimit/mysql`, `ratelimit/sqlite` with bounded
  pruning ([RATE_LIMITING.md](RATE_LIMITING.md)).
- [x] `cache`: `Cache` interface with `Fetch` stampede protection, bounded LRU +
  TTL memory cache, typed JSON helpers; Solid Cache equivalent in
  `cache/postgres`, `cache/mysql`, `cache/sqlite` with expiry pruning and entry
  bounds ([CACHING.md](CACHING.md)).
- [x] `storage`: `Storage` port, `storage/disk` (traversal-safe, atomic,
  private), HMAC-signed expiring links, safe serving handler, strict multipart
  `Receive` ([STORAGE.md](STORAGE.md)).
- [x] `FORGE_RATE_LIMIT_*`, `FORGE_CACHE_*`, `FORGE_STORAGE_*` configuration with
  fail-fast validation; production requires a storage signing key.
- [ ] S3-compatible `storage.Storage` adapter (needs a reviewed SigV4
  implementation verified against a real service). The port is documented for
  it; there is no placeholder.
- [ ] Per-route request body limits: `FORGE_HTTP_MAX_BODY_BYTES` caps every
  route and an upload route cannot raise it, so uploads are bounded by the
  global limit.
- [ ] Trusted-proxy policy for client addresses (limiter keys, audit sources).
- [ ] Generated-application wiring (`forge new` options for cache, storage and
  rate limit, a `forge generate` attachment helper), image variants, malware
  scanning hook, cache tags/pattern invalidation, a distributed cache lock.

## Deployment (Kamal parity)

`forge deploy`, `forge logs`, `forge app` and forge-proxy
([ADR 0030](adr/0030-forge-deploy.md),
[DEPLOYMENT.md](DEPLOYMENT.md#forge-deploy)).

- [x] Strict `config/deploy.yml`, `.forge/secrets` with environment
  passthrough, 0600 env files on hosts, redaction of every output.
- [x] `setup`, `deploy`, `rollback`, `prune`, `details`, `lock`,
  `maintenance`, `init`; `--dry-run` and `--json`; stable exit codes; hooks.
- [x] All-or-nothing health-gated switch with automatic revert; migration
  before any switch; retained versions.
- [x] forge-proxy: host routing, atomic switch with draining, ACME TLS,
  body limits, maintenance page, JSON access logs, persisted state; a test
  switches targets under concurrent load without a failed request.
- [x] Integration tests over real `sshd` with a docker shim (deploy,
  migration failure, partial health failure, lock contention, rollback,
  prune, log streaming and cancellation, exec, dry run).
- [ ] End-to-end test against a Docker daemon in CI (build and run the
  forge-proxy image, deploy a generated application).
- [ ] Static TLS certificates, several proxied roles, accessory
  upgrade/reboot commands, proxy image upgrade without a restart gap,
  `destinations` (staging/production files).

## Phase 9 - Performance

Benchmarks, profiling, and allocation work on hot paths (router, middleware,
UUID generation).

## Phase 10 - Security hardening

Review against the [threat model](THREAT_MODEL.md), broader fuzzing, and
supply-chain hardening (pinned base images and actions, SBOM, signed images).

## Also planned (unscheduled)

- Dev container definition.
