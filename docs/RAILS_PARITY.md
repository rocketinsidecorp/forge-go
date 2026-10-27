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
| Database selection | `rails new -d` | Done (SQLite by default, PostgreSQL with `--tenants`; MySQL, MariaDB; [ADR 0050](adr/0050-opt-in-authentication-and-organizations.md)) | Done (PostgreSQL default; SQLite default and `--tenants` pending, see Parity follow-ups) |
| Authentication and organizations opt-in | `rails new` + `rails g authentication` | Done (`forge new --auth`, `--tenants`; `forge generate authentication` wires itself; `forge generate tenancy` converts; Forge identity on PostgreSQL, MySQL, MariaDB and SQLite; `org_id`/`--org-scoped`, `forge setup [--org]`) | Pending ([forge-rust#95](https://github.com/rocketinsidecorp/forge-rust/issues/95), see Parity follow-ups) |
| Server-rendered HTML, layouts, partials, forms, flash, CSRF | Action View | Done | Done |
| API-only application | `rails new --api` | Done (`forge new --api [-d DB]`: JSON `GET /`, `/v1/status`, health and OpenAPI; no identity schema (authentication is opt-in, ADR 0050); no `app/views`, `app/assets`, `app/javascript`, `config/locales`, sign-in pages or browser middleware; `--api-auth` and `--locale` conflict with it; `generate view\|scaffold\|authentication` and HTML `controller` refuse it, [GENERATORS.md](GENERATORS.md#in-an-api-only-application)) | Done (`forge new --api`: same flag, conflicts and refusals; `generate resource` and `controller --api` are refused too, see the follow-ups) |
| HTML + JSON CRUD generator | `rails g scaffold` | Done | Done |
| Authentication pages, password reset | `rails g authentication` | Done | Done |
| Sign-in through the HTML pages only by default; JSON login opt-in | (`rails new --api` for API-only apps) | Done (`forge new --api-auth`, `forge generate authentication --api-auth`; without the flag no JSON login is mounted) | Done (no JSON login exists; `--api-auth` not needed until one does) |
| Session idle timeout | (gems, e.g. Devise `timeoutable`) | Done (`FORGE_AUTH_SESSION_IDLE_TIMEOUT`, default 2h; `last_seen_at` written at most once a minute) | Done (`FORGE_AUTH_SESSION_IDLE_TIMEOUT`, default 2h, `5m` to `720h`; `forge_sessions.last_seen_at` written at most once a minute, [forge-rust#75](https://github.com/rocketinsidecorp/forge-rust/issues/75), merged in forge-rust#78) |
| CSRF token masked per render (BREACH) | `masked_authenticity_token` | Done (`csrf.Token`, `csrf.MaskToken`) | Done (`csrf_meta`, `csrf_field`, `forge::views::mask_csrf_token`/`unmask_csrf_token`; same encoding, forge-rust#78) |
| First tenant and owner, seeds | `db:seed` | Done (`forge setup`, `forge seed`) | In review (`forge setup`, `forge seed`; the application commands `setup` and `seed` are Done) |
| Multi-tenancy with database enforcement | (gems) | Done (PostgreSQL RLS; `app_migrator`/`app_runtime` roles, staging and production refuse a serving role that could bypass RLS, tests run as the runtime role; [ADR 0046](adr/0046-postgresql-runtime-and-migration-roles.md)) | Done (PostgreSQL RLS; `app_migrator`/`app_runtime` roles, Forge-Rust ADR 0011; production refusal, [forge-rust#70](https://github.com/rocketinsidecorp/forge-rust/issues/70), [forge-rust#76](https://github.com/rocketinsidecorp/forge-rust/pull/76)) |
| TOTP MFA | (gems) | Done (sign-in code step and generated `/mfa` settings pages: enrollment, rotation, backup codes, disable; [ADR 0045](adr/0045-generated-mfa-settings-pages.md)) | Done (`FORGE_AUTH_MFA_KEY`, `/session/mfa`, generated `/mfa` settings pages; Forge-Rust ADR 0040) |
| Persistent jobs, cron, outbox | Solid Queue | Done | Done (framework crates; `forge new` wires the job queue into PostgreSQL applications: `jobs` feature, `.jobs(WorkerMode::InServe)`, `jobs_configure`, forge-rust#48) |
| Batch claims, `LISTEN/NOTIFY` wake-up, one replica per scheduled tick | Solid Queue (`batch_size`, `polling_interval`, recurring tasks run once) | Done (`WorkerOptions.BatchSize`, `queuepostgres.WithNotify`, `DispatcherOptions.BatchSize`, `jobs.SchedulerOptions.Ticks` with `forge_job_schedules`, wired by `forge new`; [ADR 0047](adr/0047-batch-claims-notify-and-recorded-ticks.md)) | Partial (jobs claim up to the free slots, outbox `DispatcherSettings::batch` 50, ticks recorded once in a table; no wake-up; see Parity follow-ups) |
| Mailer with previews | Action Mailer | Done | Done (`forge::mail`, password-reset emails, `/_mail` previews, `forge generate mailer`; Forge-Rust ADR 0035 and 0038) |
| Rate limiting | `rate_limit` | Done | Done |
| Argon2id password hashing, bounded concurrent hashing | (`has_secure_password` uses bcrypt) | Done (Argon2id `m=19456,t=2,p=1` with Forge-Rust's encoding, PBKDF2 hashes rehashed at sign-in with a legacy timing floor, `auth.PasswordWorkLimiter`: one hash per CPU, at most 16, then `503` with `Retry-After`; `-tags forge_pbkdf2` for FIPS builds; [ADR 0048](adr/0048-argon2id-password-hashing.md)) | Done (Argon2id `m=19456,t=2,p=1`, `forge::auth::PasswordWorkLimiter`; no PBKDF2 verification, FIPS option or `Retry-After`, see Parity follow-ups) |
| Client address behind trusted proxies | `request.remote_ip`, `trusted_proxies` | Done (`FORGE_HTTP_TRUSTED_PROXIES`, ADR 0037) | Done (`FORGE_HTTP_TRUSTED_PROXIES`, Forge-Rust ADR 0032; `forge deploy` sets it for the proxied role, Forge-Rust ADR 0042) |
| Cache | Solid Cache | Done | Done |
| File storage with signed URLs | Active Storage | Done (disk) | Done (disk) |
| Import maps, Turbo, Stimulus | Hotwire | Done | Done |
| TypeScript front end by default | none: `rails new` ships importmap-rails with plain JavaScript, and Turbo has no types (owner decision D32) | Done (`forge new` generates `app/javascript/**/*.ts`, a strict `tsconfig.json` and Turbo and Stimulus declarations; `forge dev\|build\|test\|check` compile it with esbuild's Go API into the embedded `app/javascript/dist`, no Node; `forge check` runs `tsc --noEmit` when `tsc` is installed; `forge new --js` keeps JavaScript, [ADR 0049](adr/0049-typescript-by-default.md)) | Planned, after forge-go 100% (decision D37): same commands and flag, with oxc instead of esbuild (see the porting notes in ADR 0049) |
| Turbo Streams over SSE | Action Cable | Done | Done (`scaffold --broadcast`, Forge-Rust ADR 0031) |
| Rails-style development log | `config.log_level`, `config.colorize_logging`, `ActiveRecord::LogSubscriber` | Done (`FORGE_LOG_FORMAT=auto\|pretty\|text\|json`: colored request, SQL and job lines on a terminal in development, `NO_COLOR`, `queries=N` per request, `FORGE_LOG_SLOW_QUERY`, statements without arguments, route pattern instead of path, default key redaction in every format, `FORGE_LOG_REDACT_KEYS`; JSON in production with the same event names; [OBSERVABILITY.md](OBSERVABILITY.md#development-log), decision D28) | Planned, after forge-go 100% (decision D37) |
| Traces, metrics, log correlation | (gems) | Done (OpenTelemetry, Prometheus) | Done (OpenTelemetry, Prometheus; Forge-Rust ADR 0033) |
| Preconfigured Prometheus, Grafana, Tempo, Loki | — | Done (`compose.observability.yaml`, [ADR 0038](adr/0038-local-observability-stack.md)) | Done (`compose.observability.yaml`, Forge-Rust ADR 0034) |
| Prometheus alert rules in the local stack | — | Done (`observability/alerts.yml`: target down, no metrics, 5xx ratio, p95 latency, database pool saturation and waiters, dead queue jobs, failing scheduled jobs; a test checks every metric and label against what the exporter registers; [OBSERVABILITY.md](OBSERVABILITY.md#alert-rules)) | Planned, after forge-go 100% (decision D37) |
| Zero-downtime deploys, rollback, remote logs | Kamal | Done (`forge deploy`, `forge logs`, `forge-proxy`) | In review (`forge deploy`, `forge logs`, `forge app`, `forge-proxy` with ACME TLS, same `config/deploy.yml`, Forge-Rust ADR 0042) |
| Live reload and database lifecycle | `bin/dev`, `db:*` | Done (`forge dev`, `forge db`) | In review (`forge dev`, `forge db create\|drop\|reset\|setup\|prepare\|migrate\|rollback\|status\|seed`, `forge migrate\|rollback\|setup\|seed`, `forge doctor`, `forge version`; `forge db console`, `forge routes` and the `.env.development`/`.env.local` files are Done (`forge routes` and the `.env` files in forge-rust#81); `forge db schema:dump\|schema:load` and browser reload are still open; Forge-Rust ADR 0036) |
| One name per setting and command (naming decision D2) | — | Done (`FORGE_HTTP_*`, `FORGE_DATABASE_*`, `FORGE_CACHE_DEFAULT_TTL`, `FORGE_STORAGE_LINK_TTL`; `forge uuid [--count N]`; `forge new --forge-path`, with `--framework-path` a deprecated alias for one minor version) | Done (forge-rust#81: reads the same `FORGE_HTTP_*`, `FORGE_CACHE_DEFAULT_TTL` and `FORGE_STORAGE_LINK_TTL` names and duration formats, `forge uuid [--count N]`; its older spellings are deprecated aliases until the next minor version) |
| Development environment files | `dotenv` gems, `.env.development` | Done (`.env.development` then `.env.local`, process environment wins, `forge dev\|test\|db\|migrate\|rollback\|doctor`) | Done (forge-rust#81: same files, format, precedence and line-only errors; `forge new` writes `.env.development`; differences: no `FORGE_TEST_DATABASE_URL` or generated `.env.local`) |
| Type-check, format and lint the application | `bin/rubocop`, `rails zeitwerk:check` | Done (`forge check`: `go build ./...` and `go vet ./...`; `forge format [--check]`: `gofmt -l -w`; `forge lint`: `golangci-lint run ./...`, a clear error naming the install command when it is missing) | Done (`forge check`, `forge format`, `forge lint`: `cargo check`, `cargo fmt`, `cargo clippy`; no `--check` yet, see Parity follow-ups) |
| Catch-all route segment | `get "files/*path"` | Done (`GET /files/{key...}`, `http.ServeMux`) | Done (forge-rust#81: `{name...}` as the last segment; see the `/files` difference under Parity follow-ups) |
| List the application's routes | `rails routes` | Done (`forge routes`: the routes registered while the application composes, including the `/v1/<plural>` routes of generated resources) | Done (forge-rust#81: same command and columns, also from the composed application) |
| One page generator | — | Done (`forge generate view [DIR/]NAME`, `forge destroy view`) | Done (forge-rust#81: same command and arguments; refuses `--api` applications instead of adding a views base) |
| Rollback never reverts framework migrations; disabled framework sets tolerated | (engine migrations copied into the app) | Done (`rollback`, `Migrator.DownFramework`, ADR 0039) | Done (`rollback`, `rollback_framework`, ADR 0030) |
| Encrypted credentials, attribute encryption | `credentials:edit`, AR Encryption | Done (`forge credentials`, `encryption`; PostgreSQL tenant tables rotate per tenant with `postgres.WithTenant`, unscoped runs under row-level security are refused) | Done (`forge credentials init\|edit\|show\|rotate-key`, `forge::credentials`, `forge::encryption`; PostgreSQL tenant tables rotate per tenant with `EncryptedColumn::tenant`, unscoped runs under row-level security are refused; forge-rust#37, Forge-Rust ADR 0041) |
| i18n | I18n | Done (`config/locales/*.json`, `t`, CLDR plurals, `forge new --locale`, `forge i18n check`, [ADR 0042](adr/0042-internationalization.md)) | Done (`config/locales/*.json`, `layout.i18n.t`, CLDR plurals, `forge new --locale`, `forge i18n check`, Forge-Rust ADR 0037) |
| Locale path prefix removed before every middleware (no path-guard bypass in any order) | — | In review (`httpserver.Rewriter`, forge-go#56) | Done (`Middleware::rewrite`, forge-rust#69) |
| OpenAPI from typed routes | — | Done (`openapi.Describe`, `openapi.Typed`, `GET /openapi.json`, `forge openapi [--output\|--check]`, `FORGE_OPENAPI_ENABLED`, [ADR 0043](adr/0043-openapi-from-typed-routes.md)) | Done (`#[derive(OpenApiSchema)]`, `forge::openapi::typed`/`describe`, `GET /openapi.json`, `FORGE_OPENAPI_ENABLED`, `forge openapi`, Forge-Rust ADR 0039) |
| Controller, model, mailer and job generators, `destroy` | `rails g`, `rails d` | Done (`forge generate model\|controller\|mailer\|job --queue`, `forge destroy`, [ADR 0041](adr/0041-generators-and-destroy.md)) | Done (`forge generate model\|controller\|mailer\|job --queue`, `forge destroy`, Forge-Rust ADR 0038) |
| Generators mount their own routes, no paste (routes seam) | `rails g scaffold` edits `config/routes.rb` | Done (`app/bootstrap/routes_gen.go`: one `// forge:KIND NAME` block per generator, package-level guards, checksum refusal of hand edits, `destroy` unwires, same arguments again is a no-op; [GENERATORS.md](GENERATORS.md#the-routes-seam)) | In progress ([forge-rust#93](https://github.com/rocketinsidecorp/forge-rust/issues/93), before associations and v0.1.0) |
| `g` and `d` command aliases | `rails g`, `rails d` | Done (`forge g`, `forge d`, also in `forge help`) | In progress ([forge-rust#93](https://github.com/rocketinsidecorp/forge-rust/issues/93), before associations and v0.1.0) |
| Inferred add/remove column migrations | `rails g migration AddSkuToProducts sku:string` | Done (`forge g migration add_COLUMNS_to_TABLE field:type...` and `remove_COLUMNS_from_TABLE`, per dialect; other names stay blank with a warning) | In progress ([forge-rust#93](https://github.com/rocketinsidecorp/forge-rust/issues/93), before associations and v0.1.0) |
| Explicitly public generated routes without Forge identity | — | Done (`--public` on `generate resource\|scaffold` with `--global`, refused with Forge identity; deny-all otherwise) | In progress ([forge-rust#93](https://github.com/rocketinsidecorp/forge-rust/issues/93), before associations and v0.1.0) |
| README quickstart run end to end in CI | Rails Guides "Getting Started" | Done (the generated README's Quickstart block run as written by a real `forge`, then sign-in, CRUD and JSON over HTTP for SQLite `--auth`, MySQL/MariaDB `--auth` and PostgreSQL `--tenants` with a cross-organization `404` check: `TestGeneratedApplication/readme_quickstart_*`, CI jobs `generated` and `mariadb`) | In progress ([forge-rust#93](https://github.com/rocketinsidecorp/forge-rust/issues/93), before associations and v0.1.0) |
| Continuous fuzzing, SBOM, signed images | — | Done (every fuzz target per PR and nightly, `scripts/fuzz.sh`; CycloneDX/SPDX SBOMs; keyless cosign-signed images from `v*` tags; [SUPPLY_CHAIN.md](SUPPLY_CHAIN.md), [ADR 0044](adr/0044-continuous-fuzzing-sbom-signed-images.md)) | Done (`scripts/fuzz.sh`, `scripts/sbom.sh`, signed forge-proxy image, SHA/digest pins enforced by `crates/forge/tests/supply_chain.rs`, Forge-Rust ADR 0043) |
| Architecture test in the generated application | Packwerk-style boundary specs (gems) | Done (`app/architecture_test.go` in every `forge new`: `go/parser` on the imports, `domain` and `application` import inward only, nothing but `app/bootstrap` composes, a failure names file, line and import; [ARCHITECTURE.md](ARCHITECTURE.md#architecture-test-of-the-generated-application)) | Planned, after forge-go 100% (decision D37) |
| `AGENTS.md` for AI coding agents in the generated application | — | Done (`forge new` writes `AGENTS.md`, under 120 lines: rings, commands, routes seam, security defaults, tests; only what that application has; a test checks its commands and files; [AI_AGENT_GUIDE.md](AI_AGENT_GUIDE.md#the-generated-agentsmd)) | Planned, after forge-go 100% (decision D37) |
| Published benchmarks | — | In review (`go test -bench`, `scripts/bench.sh`, [BENCHMARKS.md](BENCHMARKS.md)) | Done (`cargo bench -p forge-bench`, `scripts/bench.sh`) |
| Getting Started guide | Rails Guides | Done ([GETTING_STARTED.md](GETTING_STARTED.md)) | Done (`docs/getting-started.md`) |

## Parity follow-ups

- Opt-in authentication and organizations, SQLite by default (owner
  decisions D30, D31, D33-D36 of 2026-10-26, [ADR
  0050](adr/0050-opt-in-authentication-and-organizations.md)). Forge-Rust
  still lacks: `forge new --auth` and `--tenants` (and a plain `forge new`
  without identity), SQLite as the default database, identity stores for
  MySQL/MariaDB and SQLite with a shared contract suite, `forge setup
  --email` with a hidden default organization and `--org`, `org_id` and
  `--org-scoped` in generated code, a self-wiring `forge generate
  authentication`, and `forge generate tenancy`. Per the owner's decision D37
  of 2026-10-27, Forge-Rust takes no new package until Forge-Go reaches 100%;
  this follows then, tracked in
  [forge-rust#95](https://github.com/rocketinsidecorp/forge-rust/issues/95).

- Rails-style development log (forge-go decision D28,
  [OBSERVABILITY.md](OBSERVABILITY.md#development-log)). Forge-Rust has none:
  it is planned after forge-go reaches 100% (decision D37), with the same
  `FORGE_LOG_FORMAT`, `FORGE_LOG_LEVEL`, `FORGE_LOG_SLOW_QUERY` and
  `FORGE_LOG_REDACT_KEYS` variables, `NO_COLOR`, the same HTTP, SQL and JOB
  line layout and the same redaction list. Forge-Go itself still lacks the
  request ID on the logs of a job enqueued by a request: the job queue stores
  no metadata, and adding it is a migration of the released queue tables on
  every adapter, and the principal ID on the request logger after
  authentication, which needs a hook in `auth`.

- Argon2id password hashing (forge-go QG13, audit S-9 and P-7, [ADR
  0048](adr/0048-argon2id-password-hashing.md)). Both frameworks write the same
  `$argon2id$v=19$m=19456,t=2,p=1$…` hashes and verify each other's. Forge-Rust
  still lacks: a bound on the cost a stored hash may name (its verifier takes
  any `m`, `t`, `p` the `argon2` crate allows, and versions other than 19);
  verification and sign-in rehash of Forge-Go's PBKDF2 hashes, for databases
  that move between the frameworks; a PBKDF2 option for FIPS builds;
  `Retry-After` on the `503 authentication_unavailable` of a saturated
  `PasswordWorkLimiter`; a legacy-hash timing floor (Forge-Go pads every
  failed check to one PBKDF2-600k verification while PBKDF2 hashes may exist,
  `auth.WithLegacyHashTimingFloor`), needed once Forge-Rust verifies PBKDF2.
  Forge-Go also rehashes an Argon2id hash whose salt is not 16 bytes;
  Forge-Rust rehashes only on other parameters. Tracked in
  [forge-rust#91](https://github.com/rocketinsidecorp/forge-rust/issues/91),
  for the next Forge-Rust security batch after QR8, before v0.1.0.

- RLS, environment and lifecycle hardening (forge-go QG9, audit items #14,
  #15 and #20; tracked for Forge-Rust in
  [forge-rust#85](https://github.com/rocketinsidecorp/forge-rust/issues/85)). Forge-Go now: refuses a
  serving role that holds `TRUNCATE`, `REFERENCES` (also per column) or
  `TRIGGER` on a table with row-level security, directly, through `PUBLIC`
  or through any membership (`has_table_privilege`,
  `has_any_column_privilege`), with the reason "holds TRUNCATE, REFERENCES
  or TRIGGER", and one with any privilege on a partition without row-level
  security of a partitioned table with it (`pg_partition_ancestors`); runs the refusal by default in every
  `postgres.Open` outside development and test (`config.Database.Environment`),
  with `postgres.AllowRowSecurityBypass()` as the only opt-out; refuses
  to open a pool while `PGOPTIONS` is set and ignores `PGTZ` and `PGAPPNAME`
  unless the URL names them (the migrator keeps all three; Forge-Rust
  refuses the same way, since SQLx cannot drop `PGOPTIONS`); refuses symlinked credential key and
  `.json.enc` files (`credentials.ErrSymlink`, as Rust's `O_NOFOLLOW`);
  exposes `App.Serving()` (false under `routes` and `openapi`), and the
  generated bootstrap starts its scheduler and recovery queue only when
  serving and opens its pool lazily otherwise (`postgres.Lazy`, `mysql.Lazy`;
  a lazy PostgreSQL pool checks each connection's role); skips stale
  scheduler ticks (`MaxLateness`); guards generated JSON routes, in an
  application without bearer sign-in, with the session cookie, the CSRF token
  and the permission (it wired them closed like Rust's `DenyAll`, from
  forge-rust#83, until the end-to-end quickstart, decision D21, needed JSON as
  the signed-in owner); pins the generated `Dockerfile` bases by digest; generates
  identity IDs in Go with no `DEFAULT uuidv7()`; counts and logs cache values
  that no longer decode (`cache.DecodeFailures`). Forge-Rust adopts: the
  `TRUNCATE`/`REFERENCES`/`TRIGGER` rule (column `REFERENCES` included) and
  the unprotected-partition rule with the same catalog predicates and
  reasons in `require_row_security`; the refusal on every pool a command
  opens (it runs for `serve`, `jobs work` and the `jobs` commands, while
  application commands such as `seed` and `setup` get an unchecked pool),
  with an opt-out named like `AllowRowSecurityBypass`; digest-pinned bases in its generated
  `Dockerfile`; a counter and one warning per key for undecodable cache
  values.
  Both frameworks now refuse `PGOPTIONS` for every pool but the migrator's,
  floor an explicit scheduler lateness at a minute, and refuse a
  `FORGE_TELEMETRY_METRICS_ADDR` that overlaps `FORGE_HTTP_ADDR` by the same
  rule (same non-zero port on the same or an unspecified host; port 0 never
  overlaps).

- CLI differences that remain after `forge check`, `forge format` and
  `forge lint` (forge-go QG10, audit gap 3). Flag-parse and usage errors of
  every Forge-Go subcommand now exit 2, like Forge-Rust's clap errors.
  - `forge format --check` (list the files, write nothing, exit 1) exists in
    Forge-Go; Forge-Rust's `forge format` takes no flags. Forge-Rust adds
    `--check` (`cargo fmt --all --check`) with the same meaning. Not started.
  - `forge new --skip-lockfile` (Forge-Rust's name) is accepted by Forge-Go as
    an alias of `--skip-deps`: neither resolves dependencies (`go get` and
    `go mod tidy`; `cargo generate-lockfile`). Forge-Rust adopts `--skip-deps`
    as an alias of `--skip-lockfile`. Not started.
  - `forge new --skip-docker` and `--skip-ci` (Forge-Rust) do not exist in
    Forge-Go. The generated README, the deploy configuration and the
    generated tests assume the Dockerfile, the compose stack and the CI
    workflow, so the flags need conditional templates and a README that
    changes with them; Forge-Go adopts both flags with Forge-Rust's meaning
    (no `Dockerfile`, `.dockerignore`, `compose.yaml`,
    `compose.observability.yaml`, `observability/`, `docker/`; no
    `.github/`) in a change that owns the scaffold templates. Not started.
  - `forge new --api` exists in both (forge-go QG11, audit P-1) with the same
    conflicts (`--api-auth`, `--locale`) and the same refusals (`generate
    view`, `scaffold`, `authentication`, an HTML `controller`). (1) In a
    Forge-Go `--api` application `generate resource` and `controller --api`
    write their JSON layer and print wiring for `Configure`; Forge-Rust
    refuses both because its `--api` bootstrap has no `fn compose` to wire
    them into (`crates/forge-cli/src/scaffold/mod.rs` and `controller.rs`).
    Forge-Rust adopts Forge-Go's behaviour by giving `--api` applications a
    `compose` hook. Next forge-rust batch after QR7 (QR7b). (2) The schema of
    an `--api` PostgreSQL application: Done (orchestrator decision D15,
    Forge-Rust's schema is the reference). Both keep Forge's identity, audit
    and session migrations: Forge-Rust's `forge_audit`, `forge_identity` and
    `forge_session_idle_timeout`, Forge-Go's `authpostgres.Migrations()` in
    `cmd/NAME/main.go`. Forge-Go's identity set is one ordered framework set,
    so it also creates the tables Forge-Rust adds only with the sign-in pages
    (tenants, password resets, MFA, login throttle): a superset, never a
    missing table. Neither serves them: no browser pipeline, no sign-in pages,
    no `setup` command, and the generators print no identity wiring. (3)
    Forge-Rust's matrix has no `rails new --api` row yet; it adds one, Done.
    Next forge-rust batch after QR7 (QR7b).
  - `forge inspect` and `forge help --json` exist in Forge-Go only.

- Queue and scheduler throughput (forge-go QG4c, audit PF-1, PF-2, PF-5,
  PF-7; [ADR 0047](adr/0047-batch-claims-notify-and-recorded-ticks.md)).
  Forge-Rust already claims jobs in batches (up to a queue's free slots,
  without a knob), batches outbox events (`DispatcherSettings::batch`,
  default 50, a lease covering the batch) and runs each scheduled tick once
  (`forge_job_schedules.last_tick_at`, advanced by `forge_jobs_fire_tick`),
  which Forge-Go now mirrors with the same table and column names
  (`jobs.TickClaimer`); Go runs the tick in process where Rust enqueues it as
  a job. Both skip stale ticks instead of replaying them (Rust
  `Schedule::max_lateness`, Go `SchedulerOptions.MaxLateness` since forge-go
  QG9: the latest due tick runs once, earlier ones are skipped and reported);
  the default is one interval of the schedule in both, and an explicit
  `MaxLateness` below a minute is raised to a minute in both (Rust also
  floors the default, which only matters for a Go `Every` below a minute).
  Rust lacks the
  wake-up: it adopts `NOTIFY forge_jobs` (payload: the queue name, sent in
  `forge_jobs_enqueue` for due jobs) and a worker `LISTEN` owned by `Jobs::run`
  with the same reconnect backoff (0.5s to 1s, doubling to 15s to 30s) behind
  an opt-in setting with the same meaning as Go's `WithNotify`, and a
  `WorkerSettings` batch bound (Go's `BatchSize`, default 10, never above the
  free slots). The outbox defaults (Go 10, Rust 50) are to be agreed. Tracked
  in [forge-rust#80](https://github.com/rocketinsidecorp/forge-rust/issues/80).

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

- Stream signing key rotation (forge-go QG4b,
  [forge-rust#77](https://github.com/rocketinsidecorp/forge-rust/issues/77)): Forge-Go's
  generated `htmlstreams.NewSigner` now also reads `FORGE_STREAMS_PREVIOUS_KEYS`
  (comma-separated base64 keys, at most `broadcast.MaxPreviousKeys` = 3, never
  echoed in an error), the name Forge-Rust's parity doc already proposes, so
  rotating `FORGE_STREAMS_KEY` no longer breaks open pages. Forge-Rust's
  `Signer::new(key, previous)` already verifies previous keys, but its
  generated applications read `FORGE_STREAMS_KEY` only: it adds the variable
  with the same name, separator, limit and redaction (tracked in
  [forge-rust#77](https://github.com/rocketinsidecorp/forge-rust/issues/77),
  after forge-rust#14).
  Until then the Rust rotation procedure is in its
  [deployment](https://github.com/rocketinsidecorp/forge-rust/blob/main/docs/deployment.md#rotating-the-stream-signing-key).

- forge-proxy connection and header limits (forge-go QG4b, audit S-8):
  Forge-Go's `forge-proxy run` has `-max-header-bytes` (default 65536, 4096 to
  1048576, `431` above it; was 1 MiB) and `-max-connections` (default 10000
  per listener, admission before accept). Forge-Rust caps a listener at the
  same 10,000 connections with a constant and sets hyper's `max_buf_size` to
  1 MiB: it adopts both flags with the same names, defaults and range and the
  64 KiB header default (tracked in
  [forge-rust#77](https://github.com/rocketinsidecorp/forge-rust/issues/77)).
  Neither framework exposes them in `config/deploy.yml`.

- Welcome page CSP and revendoring (forge-go QG4b,
  [forge-rust#77](https://github.com/rocketinsidecorp/forge-rust/issues/77)):
  Forge-Go's generated welcome page allows its one inline stylesheet by
  SHA-256 hash instead of `style-src 'unsafe-inline'`, with a generated test
  that recomputes the hash from the served page, and `forge generate ...` runs
  `go mod vendor` in an application that vendors its dependencies. Forge-Rust
  adopts the hash (its welcome page and any generated inline style) and checks
  whether its generators need an equivalent step in a vendored application.

- Production PostgreSQL TLS (forge-go QG4b): both frameworks refuse
  `sslmode=require`, a missing `sslmode` and, in production, anything that
  does not verify the server. A deliberate difference remains: Forge-Rust
  refuses `verify-ca` altogether, because SQLx adds the web roots even with
  `sslrootcert`, so a certificate from any public CA would pass. Forge-Go
  keeps `verify-ca`, but only with `sslrootcert` in the URL (config refuses
  it without): pgx then trusts that file alone. `verify-full` needs neither.
  Forge-Go also refuses, at startup, a `PGSSLROOTCERT`, `PGSSLCERT`,
  `PGSSLKEY` or `PGSSLPASSWORD` that the URL does not repeat, instead of
  ignoring it; Forge-Rust reads PG* defaults through SQLx
  ([forge-rust#77](https://github.com/rocketinsidecorp/forge-rust/issues/77)
  records the comparison).

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
  goes further in three ways Forge-Rust adopted: outside development and
  test `postgres.Open` (by default since forge-go QG9; earlier through
  `postgres.RequireRowSecurity`) refuses a serving role that logs in as
  or acts as a superuser, a `BYPASSRLS` or `CREATEROLE` role or a member of a
  superuser or `BYPASSRLS` role, or whose URL switches it to another role;
  the generated repository tests connect as the runtime role
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
  adopted the refusal and both tests in
  [forge-rust#70](https://github.com/rocketinsidecorp/forge-rust/issues/70)
  (merged in [forge-rust#76](https://github.com/rocketinsidecorp/forge-rust/pull/76)).
  Done for both (forge-go#63, forge-rust#76): the refusal also covers
  `REPLICATION`, membership in a superuser, `BYPASSRLS` or `REPLICATION` role,
  membership in `pg_execute_server_program`, `pg_write_server_files` or
  `pg_read_server_files`, and owning (or being able to `SET ROLE` to the owner
  of) a table with row-level security, with the same escape rules (the
  catalog predicates), reasons and fix text in both frameworks.
  `forge deploy` gives the migration credential to the migrate step alone
  through `migrate.env`
  ([forge-go#57](https://github.com/rocketinsidecorp/forge-go/issues/57));
  Forge-Rust's `forge deploy` has the same `migrate.env`
  ([forge-rust#73](https://github.com/rocketinsidecorp/forge-rust/issues/73),
  merged in forge-rust#74).

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
  Forge-Rust shipped all three in forge-rust#78 (for
  [forge-rust#75](https://github.com/rocketinsidecorp/forge-rust/issues/75)):
  (1) the same idle timeout, variable name and default in its generated
  session store (a `last_seen_at` column in a new migration, the same
  once-a-minute write); (2) masked CSRF tokens from `csrf_field`/`csrf_meta`
  with the same encoding, so a page can be served by either framework;
  (3) the production warning for `MemoryLoginThrottle` in custom wiring.
  `--api-auth` needs nothing in Forge-Rust while it has no JSON login; if it
  gains one, it is opt-in under the same flag name.

- Catch-all segment and the bare prefix (forge-rust#81, documented difference):
  with only `/files/{key...}` registered, Go's `http.ServeMux` answers a request
  for `/files` with a redirect to `/files/`, while Forge-Rust's router
  (catch-all merged in forge-rust#81) answers `404`. This is a documented difference, not a
  gap: each framework follows its own router.

## Review gate

Every pull request passes the Claude Code Review before it merges to `main`,
including a screen for malicious or exfiltrating code: unexpected network
calls, access to credentials, wallets or crypto keys, obfuscated code, build
scripts and new dependencies.
