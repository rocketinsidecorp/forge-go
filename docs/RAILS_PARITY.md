# Rails parity

Forge exists in two frameworks, Forge-Go and Forge-Rust, that ship the same
capabilities with the same commands, names and conventions (owner decision,
2026-10-20). This matrix tracks Ruby on Rails 8 capabilities and the places
where Forge intends to go further. A row is **Done** only when code, tests and
README commands exist on `main` of that framework.

Each framework's status is one of:

| Status | Meaning |
| --- | --- |
| **Done** | On `main` with code, tests and README commands. A Forge-Rust **Done** names the Rust command or file that proves it. |
| **Partial** | Part is on `main`; the cell says what is missing and cites the issue that tracks it. |
| **In review** | A pull request is open or the work is finished and waiting for its review; the cell cites it. |
| **Planned** | Not started; the cell cites the issue that tracks it. |
| **Not needed**, **Not applicable** | The capability does not apply to that framework; the cell says why. |

Owner decision D37 (2026-10-27): Forge-Rust takes no new package until
Forge-Go reaches 100%. Parity is therefore judged on two things: every Forge-Go
capability has an identical, tracked Forge-Rust plan (the same commands and
names, and a `forge-rust#NN` issue), and every capability Forge-Rust has first
is tracked on the Forge-Go side (`forge-go#NN`).

Every row has a stable `ID`. A test, `rails_parity_test.go` in the repository
root, parses this table and fails when:

- an ID is missing, malformed or repeated, or a status is not in the table
  above;
- a Forge-Rust cell that is not **Done**, **Not needed** or **Not applicable**
  cites no `forge-rust#NN` issue, or a Forge-Go cell that is not **Done**,
  **Not needed** or **Not applicable** cites no `forge-go#NN` issue;
- a Forge-Rust **Done** cell names no command, file or setting in backticks;
- a Forge-Go **Done** cell claims a `forge COMMAND` or `--flag` that
  `forge help --json` (the command catalog of `internal/cli`) does not list, or
  a Forge-Go **Planned** cell names a `forge COMMAND --flag` that already
  exists;
- a link in this file points to a document that does not exist;
- a reference is still a `forge-go#TBD-...` or `forge-rust#TBD-...`
  placeholder. A placeholder stands for an issue that is about to be filed;
  the test accepts it only with `FORGE_PARITY_ALLOW_TBD=1`, and it is replaced
  by the issue number before the change merges.

Run it with `go test -run TestRailsParity .`.

| ID | Capability | Rails 8 | Forge-Go | Forge-Rust |
| --- | --- | --- | --- | --- |
| `new-application` | New application with Docker and CI | `rails new` | Done (`forge new NAME`: Dockerfile, `compose.yaml`, GitHub Actions CI; a plain `forge new` is a single-tenant SQLite application, [ADR 0050](adr/0050-opt-in-authentication-and-organizations.md)) | Done (`forge new NAME`: Dockerfile, `compose.yaml`, `.github/workflows/ci.yml`, tests; `crates/forge-cli`) |
| `database-selection` | Database selection | `rails new -d` | Done (SQLite by default, PostgreSQL with `--tenants`; MySQL, MariaDB; [ADR 0050](adr/0050-opt-in-authentication-and-organizations.md)) | Partial (`forge new -d postgresql\|mysql\|mariadb\|sqlite3\|none`; PostgreSQL stays the default, so the SQLite default and `--tenants` selecting PostgreSQL follow in [forge-rust#95](https://github.com/rocketinsidecorp/forge-rust/issues/95)) |
| `opt-in-auth-tenancy` | Authentication and organizations opt-in | `rails new` + `rails g authentication` | Done (`forge new --auth`, `--tenants`; `forge generate authentication` wires itself; `forge generate tenancy` converts; Forge identity on PostgreSQL, MySQL, MariaDB and SQLite; `org_id`/`--org-scoped`, `forge setup [--org]`) | Planned, after forge-go 100% (decision D37; [forge-rust#95](https://github.com/rocketinsidecorp/forge-rust/issues/95)): same commands and flags, login on SQLite, MySQL and MariaDB with the shared isolation suite) |
| `server-rendered-views` | Server-rendered HTML, layouts, partials, forms, flash, CSRF | Action View | Done | Done (`forge new`: `templates/`, Askama views, layout, partials, forms, flash, CSRF fields; `crates/forge-views`, Forge-Rust ADR 0022 and 0023) |
| `api-only-app` | API-only application | `rails new --api` | Done (`forge new --api [-d DB]`: JSON `GET /`, `/v1/status`, health and OpenAPI; no identity schema (authentication is opt-in, ADR 0050); no `app/views`, `app/assets`, `app/javascript`, `config/locales`, sign-in pages or browser middleware; `--api-auth` and `--locale` conflict with it; `generate view\|scaffold\|authentication` and HTML `controller` refuse it, [GENERATORS.md](GENERATORS.md#in-an-api-only-application)) | Done (`forge new --api`: same flag, conflicts and refusals; `forge generate resource` and `controller --api` write their JSON layer and wire it through `fn compose` in `src/bootstrap/mod.rs`, forge-rust#90) |
| `crud-generator` | HTML + JSON CRUD generator | `rails g scaffold` | Done | Done (`forge generate scaffold`: HTML pages and the JSON API under `/v1/<plural>` behind bearer tokens, Forge-Rust ADR 0024 and 0045) |
| `authentication-pages` | Authentication pages, password reset | `rails g authentication` | Done | Done (`forge generate authentication`, `crates/forge-cli/src/authentication.rs`, Forge-Rust ADR 0026) |
| `json-login-opt-in` | Sign-in through the HTML pages only by default; JSON login opt-in | (`rails new --api` for API-only apps) | Done (`forge new --api-auth`, `forge generate authentication --api-auth`; without the flag no JSON login is mounted) | Done (`forge new --api-auth`, `forge generate authentication --api-auth`: `POST /auth/login`, `POST /auth/logout`, `GET /v1/auth/session`, bearer tokens in `forge_bearer_sessions`; Forge-Rust ADR 0045) |
| `json-auth-routes` | JSON MFA completion, recovery and cookie routes of the opt-in JSON login | (gems) | Done (`forge new --api-auth`: `/auth/mfa/complete`, `/auth/recovery/*` and `/auth/cookie/*` beside `/auth/login`) | Planned ([forge-rust#82](https://github.com/rocketinsidecorp/forge-rust/issues/82)): a two-factor account gets `403 mfa_required` and the recovery and cookie routes are not ported) |
| `session-idle-timeout` | Session idle timeout | (gems, e.g. Devise `timeoutable`) | Done (`FORGE_AUTH_SESSION_IDLE_TIMEOUT`, default 2h; `last_seen_at` written at most once a minute) | Done (`FORGE_AUTH_SESSION_IDLE_TIMEOUT`, default 2h, `5m` to `720h`; `forge_sessions.last_seen_at` written at most once a minute, [forge-rust#75](https://github.com/rocketinsidecorp/forge-rust/issues/75), merged in forge-rust#78) |
| `csrf-masked` | CSRF token masked per render (BREACH) | `masked_authenticity_token` | Done (`csrf.Token`, `csrf.MaskToken`) | Done (`csrf_meta`, `csrf_field`, `forge::views::mask_csrf_token`/`unmask_csrf_token`; same encoding, forge-rust#78) |
| `setup-seed` | First tenant and owner, seeds | `db:seed` | Done (`forge setup`, `forge seed`) | Partial (`forge setup --tenant SLUG --email EMAIL [--organization NAME] [--password-stdin]` and `forge seed` exist; the names `--org` and `--org-name` and the hidden default organization follow in [forge-rust#95](https://github.com/rocketinsidecorp/forge-rust/issues/95)) |
| `multi-tenancy-rls` | Multi-tenancy with database enforcement | (gems) | Done (PostgreSQL RLS; `app_migrator`/`app_runtime` roles, staging and production refuse a serving role that could bypass RLS, tests run as the runtime role; [ADR 0046](adr/0046-postgresql-runtime-and-migration-roles.md)) | Done (PostgreSQL RLS; `app_migrator`/`app_runtime` roles, Forge-Rust ADR 0011 and 0044; production refuses a serving role that could bypass RLS: the same `TRUNCATE`/`REFERENCES`/`TRIGGER`, partition, owner, `REPLICATION` and server-file rules, every pool checked unless `PoolSettings::allow_row_security_bypass()`; forge-rust#70, forge-rust#76, forge-rust#85) |
| `totp-mfa` | TOTP MFA | (gems) | Done (sign-in code step and generated `/mfa` settings pages: enrollment, rotation, backup codes, disable; [ADR 0045](adr/0045-generated-mfa-settings-pages.md)) | Done (`FORGE_AUTH_MFA_KEY`, `/session/mfa`, generated `/mfa` settings pages; Forge-Rust ADR 0040) |
| `jobs-outbox` | Persistent jobs, cron, outbox | Solid Queue | Done (`queue`, `jobs` and `outbox` packages, [JOBS.md](JOBS.md)) | Done (`crates/forge-jobs` and `crates/forge-events`: durable jobs, cron schedules, outbox; Forge-Rust ADR 0025) |
| `new-wires-queue` | `forge new` wires the persistent job queue, so `jobs status` works on a fresh application | `rails new` with Solid Queue (default since Rails 8) | Planned (forge-go#88): today `forge new` leaves `queue<db>.Migrations()` and `queue<db>.Commands()` to add by hand (README, Jobs section) | Done (PostgreSQL applications from `forge new`: the `jobs` feature, `.jobs(forge::WorkerMode::InServe)` in `src/bootstrap/mod.rs`, `jobs_configure`, `jobs work\|status\|dead\|retry\|discard`; forge-rust#48) |
| `jobs-batch-claims-ticks` | Batch claims, `LISTEN/NOTIFY` wake-up (store option), one replica per scheduled tick | Solid Queue (`batch_size`, `polling_interval`, recurring tasks run once) | Done (`WorkerOptions.BatchSize`, `queuepostgres.WithNotify`, `DispatcherOptions.BatchSize`, `jobs.SchedulerOptions.Ticks` with `forge_job_schedules`, wired by `forge new`; [ADR 0047](adr/0047-batch-claims-notify-and-recorded-ticks.md)) | Done (`WorkerSettings::batch` 10, never above the free slots; `PgJobStore::notify(true)` announces due jobs on `forge_jobs` and `Jobs::run` listens; `DispatcherSettings::batch` 10; ticks recorded once in `forge_job_schedules`; Forge-Rust ADR 0046, forge-rust#80) |
| `jobs-notify-switch` | `FORGE_JOBS_NOTIFY` and an application-level wake-up switch | (none) | Partial ([forge-go#72](https://github.com/rocketinsidecorp/forge-go/issues/72)): `FORGE_JOBS_NOTIFY=true\|false` is read by `config` (`config.Jobs.Notifies`) and documented in DEPLOYMENT.md and JOBS.md; the generated bootstrap honours it once `forge new` wires the queue (forge-go#88) | Done (`FORGE_JOBS_NOTIFY=true\|false`, `App::jobs_notify(true)`, `PgJobStore::notify(true)`; Forge-Rust ADR 0046, forge-rust#80) |
| `mailer` | Mailer with previews | Action Mailer | Done | Done (`forge::mail`, password-reset emails, `/_mail` previews, `forge generate mailer`; Forge-Rust ADR 0035 and 0038) |
| `rate-limiting` | Rate limiting | `rate_limit` | Done | Done (`crates/forge-ratelimit`, Forge-Rust ADR 0027) |
| `argon2id` | Argon2id password hashing, bounded concurrent hashing | (`has_secure_password` uses bcrypt) | Done (Argon2id `m=19456,t=2,p=1` with Forge-Rust's encoding, PBKDF2 hashes rehashed at sign-in with a legacy timing floor, `auth.PasswordWorkLimiter`: one hash per CPU, at most 16, then `503` with `Retry-After`; `-tags forge_pbkdf2` for FIPS builds; [ADR 0048](adr/0048-argon2id-password-hashing.md)) | Partial (Argon2id `m=19456,t=2,p=1`, `forge::auth::PasswordWorkLimiter`; bounds on the cost a stored hash may name, PBKDF2 verification and rehash, `Retry-After` on the `503`, the FIPS option and the legacy timing floor follow in [forge-rust#91](https://github.com/rocketinsidecorp/forge-rust/issues/91)) |
| `trusted-proxies` | Client address behind trusted proxies | `request.remote_ip`, `trusted_proxies` | Done (`FORGE_HTTP_TRUSTED_PROXIES`, ADR 0037) | Done (`FORGE_HTTP_TRUSTED_PROXIES`, Forge-Rust ADR 0032; `forge deploy` sets it for the proxied role, Forge-Rust ADR 0042) |
| `cache` | Cache | Solid Cache | Done | Done (`crates/forge-cache`, Forge-Rust ADR 0028) |
| `file-storage` | File storage with signed URLs | Active Storage | Done (disk) | Done (disk, `crates/forge-storage`, Forge-Rust ADR 0029) |
| `hotwire` | Import maps, Turbo, Stimulus | Hotwire | Done (import map, Turbo and Stimulus served from the embedded assets, [VIEWS.md](VIEWS.md)) | Done (`crates/forge-views`: import map, Turbo, Stimulus; Forge-Rust ADR 0022) |
| `typescript` | TypeScript front end by default | none: `rails new` ships importmap-rails with plain JavaScript, and Turbo has no types (owner decision D32) | Done (`forge new` generates `app/javascript/**/*.ts`, a strict `tsconfig.json` and Turbo and Stimulus declarations; `forge dev\|build\|test\|check` compile it with esbuild's Go API into the embedded `app/javascript/dist`, no Node; `forge check` runs `tsc --noEmit` when `tsc` is installed; `forge new --js` keeps JavaScript, [ADR 0049](adr/0049-typescript-by-default.md)) | Planned, after forge-go 100% (decision D37; [forge-rust#97](https://github.com/rocketinsidecorp/forge-rust/issues/97)): same commands and flag, with oxc instead of esbuild (see the porting notes in ADR 0049) |
| `turbo-streams-sse` | Turbo Streams over SSE | Action Cable | Done | Done (`scaffold --broadcast`, Forge-Rust ADR 0031) |
| `dev-log` | Rails-style development log | `config.log_level`, `config.colorize_logging`, `ActiveRecord::LogSubscriber` | Done (`FORGE_LOG_FORMAT=auto\|pretty\|text\|json`: colored request, SQL and job lines on a terminal in development, `NO_COLOR`, `queries=N` per request, `FORGE_LOG_SLOW_QUERY`, statements without arguments, route pattern instead of path, default key redaction in every format, `FORGE_LOG_REDACT_KEYS`; JSON in production with the same event names; [OBSERVABILITY.md](OBSERVABILITY.md#development-log), decision D28) | Planned, after forge-go 100% (decision D37; [forge-rust#98](https://github.com/rocketinsidecorp/forge-rust/issues/98)): same `FORGE_LOG_*` variables, line layout and redaction list) |
| `telemetry` | Traces, metrics, log correlation | (gems) | Done (OpenTelemetry, Prometheus) | Done (OpenTelemetry, Prometheus; `crates/forge-telemetry`, Forge-Rust ADR 0033) |
| `observability-stack` | Preconfigured Prometheus, Grafana, Tempo, Loki | — | Done (`compose.observability.yaml`, [ADR 0038](adr/0038-local-observability-stack.md)) | Done (`compose.observability.yaml`, Forge-Rust ADR 0034) |
| `alert-rules` | Prometheus alert rules in the local stack | — | Done (`observability/alerts.yml`: target down, no metrics, 5xx ratio, p95 latency, database pool saturation and waiters, dead queue jobs, failing scheduled jobs; a test checks every metric and label against what the exporter registers; [OBSERVABILITY.md](OBSERVABILITY.md#alert-rules)) | Planned, after forge-go 100% (decision D37; forge-rust#101): the same rules in `observability/alerts.yml` and a test of every metric and label) |
| `deploy` | Zero-downtime deploys, rollback, remote logs | Kamal | Done (`forge deploy`, `forge logs`, `forge app`, `forge-proxy`) | Done (`forge deploy`, `forge logs`, `forge app`, `forge-proxy` with ACME TLS, same `config/deploy.yml`; `crates/forge-cli/src/deploy`, Forge-Rust ADR 0042) |
| `deploy-lock-logs-polish` | Bounded `forge logs` concurrency, the `ln` error text on a held deploy lock, partial `lock status` | Kamal | Done (`forge logs` reads at most `parallelism` hosts at once, every host at once with `-f`; `forge deploy lock acquire` on a held lock without a record appends the redacted remote stderr, such as `ln`'s, to the held message; partial `lock status` records were already returned with the error) | Partial (`forge logs` reads at most `parallelism` hosts at once without `-f`; the `ln` error text and the partial `lock status` follow in forge-rust#108) |
| `deploy-yml-caps` | `deploy.yml` size and nesting caps | Kamal | Done (`forge deploy`: `config/deploy.yml` over 1 MiB (`MaxConfigBytes`) or nested deeper than 100 levels (`MaxConfigDepth`) is refused with a line-numbered error, `deploy config: yaml: line N: exceeded max depth of 100`, as in Rust; [DEPLOYMENT.md](DEPLOYMENT.md)) | Done (`config/deploy.yml` over 1 MiB or nested deeper than 100 levels is refused with a line-numbered error; `crates/forge-cli/src/deploy/yaml.rs`, `config.rs`) |
| `dev-db-lifecycle` | Live reload and database lifecycle | `bin/dev`, `db:*` | Done (`forge dev`, `forge db`) | Done (`forge dev [NAME] [--no-reload]` restarts on change; `forge db create\|drop\|reset\|setup\|prepare\|migrate\|rollback\|status\|seed\|console`, `forge migrate`, `forge rollback`, `forge doctor`, `forge version`; Forge-Rust ADR 0036) |
| `dev-browser-reload` | Browser reload after each restart of `forge dev` | `bin/dev` with live reload gems | Done (`forge dev` reloads the browser after each restart; `--no-reload` turns watching and reload off; [ADR 0035](adr/0035-development-loop.md)) | Planned (forge-rust#106): `forge dev` restarts the binary but has no browser-reload proxy) |
| `db-schema-dump-load` | Schema dump and load | `db:schema:dump`, `db:schema:load` | Done (`forge db schema:dump`, `forge db schema:load`) | Planned (forge-rust#107): the commands need the credential passed to `pg_dump` and `psql` through the child's environment, never an argument) |
| `naming-d2` | One name per setting and command (naming decision D2) | — | Done (`FORGE_HTTP_*`, `FORGE_DATABASE_*`, `FORGE_CACHE_DEFAULT_TTL`, `FORGE_STORAGE_LINK_TTL`; `forge uuid [--count N]`; `forge new --forge-path`, with `--framework-path` a deprecated alias for one minor version) | Done (forge-rust#81: reads the same `FORGE_HTTP_*`, `FORGE_CACHE_DEFAULT_TTL` and `FORGE_STORAGE_LINK_TTL` names and duration formats, `forge uuid [--count N]`; its older spellings are deprecated aliases until the next minor version) |
| `dev-env-files` | Development environment files | `dotenv` gems, `.env.development` | Done (`.env.development` then `.env.local`, process environment wins, `forge dev\|test\|db\|migrate\|rollback\|doctor`) | Done (forge-rust#81: same files, format, precedence and line-only errors; `forge new` writes `.env.development`; differences: no `FORGE_TEST_DATABASE_URL` or generated `.env.local`) |
| `check-format-lint` | Type-check, format and lint the application | `bin/rubocop`, `rails zeitwerk:check` | Done (`forge check`: `go build ./...` and `go vet ./...`; `forge format [--check]`: `gofmt -l -w`; `forge lint`: `golangci-lint run ./...`, a clear error naming the install command when it is missing) | Done (`forge check`, `forge format [--check]`, `forge lint`: `cargo check`, `cargo fmt`, `cargo clippy`; forge-rust#89) |
| `catch-all-route` | Catch-all route segment | `get "files/*path"` | Done (`GET /files/{key...}`, `http.ServeMux`) | Done (forge-rust#81: `{name...}` as the last segment; see the `/files` difference under Parity follow-ups) |
| `list-routes` | List the application's routes | `rails routes` | Done (`forge routes`: the routes registered while the application composes, including the `/v1/<plural>` routes of generated resources) | Done (`forge routes [NAME]`: same command and columns, also from the composed application; forge-rust#81) |
| `page-generator` | One page generator | — | Done (`forge generate view [DIR/]NAME`, `forge destroy view`) | Done (forge-rust#81: same command and arguments; refuses `--api` applications instead of adding a views base) |
| `rollback-framework` | Rollback never reverts framework migrations; disabled framework sets tolerated | (engine migrations copied into the app) | Done (`rollback`, `Migrator.DownFramework`, ADR 0039) | Done (`rollback`, `rollback_framework`, ADR 0030) |
| `credentials-encryption` | Encrypted credentials, attribute encryption | `credentials:edit`, AR Encryption | Done (`forge credentials`, `encryption`; PostgreSQL tenant tables rotate per tenant with `postgres.WithTenant`, unscoped runs under row-level security are refused) | Done (`forge credentials init\|edit\|show\|rotate-key`, `forge::credentials`, `forge::encryption`; PostgreSQL tenant tables rotate per tenant with `EncryptedColumn::tenant`, unscoped runs under row-level security are refused; forge-rust#37, Forge-Rust ADR 0041) |
| `i18n` | i18n | I18n | Done (`config/locales/*.json`, `t`, CLDR plurals, `forge new --locale`, `forge i18n check`, [ADR 0042](adr/0042-internationalization.md)) | Done (`config/locales/*.json`, `layout.i18n.t`, CLDR plurals, `forge new --locale`, `forge i18n check`, Forge-Rust ADR 0037) |
| `locale-prefix` | Locale path prefix removed before every middleware (no path-guard bypass in any order) | — | Done (`httpserver.Rewriter`, forge-go#56) | Done (`Middleware::rewrite`, forge-rust#69) |
| `openapi` | OpenAPI from typed routes | — | Done (`openapi.Describe`, `openapi.Typed`, `GET /openapi.json`, `forge openapi [--output\|--check]`, `FORGE_OPENAPI_ENABLED`, [ADR 0043](adr/0043-openapi-from-typed-routes.md)) | Done (`#[derive(OpenApiSchema)]`, `forge::openapi::typed`/`describe`, `GET /openapi.json`, `FORGE_OPENAPI_ENABLED`, `forge openapi`, Forge-Rust ADR 0039) |
| `generators-destroy` | Controller, model, mailer and job generators, `destroy` | `rails g`, `rails d` | Done (`forge generate model\|controller\|mailer\|job --queue`, `forge destroy`, [ADR 0041](adr/0041-generators-and-destroy.md)) | Done (`forge generate model\|controller\|mailer\|job --queue`, `forge destroy`, Forge-Rust ADR 0038) |
| `routes-seam` | Generators mount their own routes, no paste (routes seam) | `rails g scaffold` edits `config/routes.rb` | Done (`app/bootstrap/routes_gen.go`: one `// forge:KIND NAME` block per generator, package-level guards, checksum refusal of hand edits, `destroy` unwires, same arguments again is a no-op; [GENERATORS.md](GENERATORS.md#the-routes-seam)) | Done (`crates/forge-cli/src/wiring.rs`: one `// forge:KIND NAME` block per generator and a `// forge:checksum` line in `src/bootstrap/routes_gen.rs`; forge-rust#94) |
| `g-d-aliases` | `g` and `d` command aliases | `rails g`, `rails d` | Done (`forge g`, `forge d`, also in `forge help`) | Done (`forge g`, `forge d`; forge-rust#94) |
| `inferred-migrations` | Inferred add/remove column migrations | `rails g migration AddSkuToProducts sku:string` | Done (`forge g migration add_COLUMNS_to_TABLE field:type...` and `remove_COLUMNS_from_TABLE`, per dialect; other names stay blank with a warning) | Done (`forge g migration add_COLUMNS_to_TABLE field:type...` and `remove_COLUMNS_from_TABLE`, per dialect; forge-rust#94) |
| `public-routes` | Explicitly public generated routes without Forge identity | — | Done (`--public` on `generate resource\|scaffold` with `--global`, refused with Forge identity; deny-all otherwise) | Done (`--public` with `--global`, refused with identity; forge-rust#94) |
| `associations` | Associations: belongs_to and has_many, with what deleting a parent does | `rails g scaffold comment post:references`, `has_many`, `dependent:` | Done (`forge generate scaffold comment body:text post:references`, also `model` and `resource`, `belongs_to` alias, optional `post:references?`: index and foreign key, composite `(org_id, post_id)` to the parent's `UNIQUE (org_id, id)` with organizations; `ON DELETE RESTRICT` by default (`409 post_in_use` or a flash message, on every backend) or `--on-delete=cascade`; a parent select gated on `posts:read`, `/posts/{id}/comments` pages and `/v1/posts/{id}/comments`, Turbo Frames on the post page; `forge destroy` refuses a referenced resource and keeps a unique key others use; [ADR 0051](adr/0051-associations.md), decision D23) | Planned, after forge-go 100% (decision D37; [forge-rust#96](https://github.com/rocketinsidecorp/forge-rust/issues/96)): the same commands, flags, type names, generated routes and refusals) |
| `join-generator` | Many-to-many join table, its pages and API, and its removal | `has_and_belongs_to_many` + `rails g migration CreateJoinTablePostTag post tag` | Done (`forge generate join post tag`: `post_tags` with a primary key over the pair and cascading foreign keys (composite with organizations), its layers, JSON routes and the link and unlink page; the reversed pair is refused; `forge destroy join post tag --migrations` removes it; [ADR 0051](adr/0051-associations.md), [GENERATORS.md](GENERATORS.md)) | Planned, after forge-go 100% (decision D37; [forge-rust#96](https://github.com/rocketinsidecorp/forge-rust/issues/96)): the same commands, flags, type names, generated routes and refusals) |
| `field-types` | Canonical generator field types and aliases | `string text integer boolean datetime references` | Done (`forge generate scaffold post title:string body:text views:integer published:boolean published_at:timestamp ref:uuid`: canonical `string text integer boolean timestamp uuid references`, aliases `int bool time datetime belongs_to`, optional with a trailing `?`; decision D22, [ADR 0051](adr/0051-associations.md)) | Planned, after forge-go 100% (decision D37; [forge-rust#96](https://github.com/rocketinsidecorp/forge-rust/issues/96)): the same commands, flags, type names, generated routes and refusals) |
| `readme-quickstart-ci` | README quickstart run end to end in CI | Rails Guides "Getting Started" | Done (the generated README's Quickstart block run as written by a real `forge`, then sign-in, CRUD and JSON over HTTP for SQLite `--auth`, MySQL/MariaDB `--auth` and PostgreSQL `--tenants` with a cross-organization `404` check: `TestGeneratedApplication/readme_quickstart_*`, CI jobs `generated` and `mariadb`) | Done (`crates/forge-cli/tests/readme_quickstart.rs` runs the generated README's Quickstart as written on PostgreSQL and SQLite, from `.github/workflows/ci.yml`; forge-rust#94) |
| `fuzz-sbom-images` | Continuous fuzzing, SBOM, signed images | — | Done (every fuzz target per PR and nightly, `scripts/fuzz.sh`; CycloneDX/SPDX SBOMs; keyless cosign-signed images from `v*` tags; [SUPPLY_CHAIN.md](SUPPLY_CHAIN.md), [ADR 0044](adr/0044-continuous-fuzzing-sbom-signed-images.md)) | Done (`scripts/fuzz.sh`, `scripts/sbom.sh`, signed forge-proxy image, SHA/digest pins enforced by `crates/forge/tests/supply_chain.rs`, Forge-Rust ADR 0043) |
| `release-binaries` | Signed release binaries, checksums, provenance | `gh release` assets by hand | Done (a `v*` tag builds `forge` and `forge-proxy` for linux, darwin and windows on amd64 and arm64 with `scripts/release-build.sh`, SHA-256 `checksums.txt` signed with keyless cosign, one GitHub build provenance attestation covering every file and a draft release; `forge version` and `forge-proxy version` print version, commit and Go toolchain; no installer script, pinned `go install ...@vX.Y.Z`; [RELEASING.md](RELEASING.md), [SUPPLY_CHAIN.md](SUPPLY_CHAIN.md#signed-release-binaries), decision D24) | Planned, after forge-go 100% (decision D37; [forge-rust#100](https://github.com/rocketinsidecorp/forge-rust/issues/100)): same archive names, `checksums.txt`, cosign and attestation commands, `forge version` fields and `forge-proxy version`; its release workflow signs only images today) |
| `architecture-test` | Architecture test in the generated application | Packwerk-style boundary specs (gems) | Done (`app/architecture_test.go` in every `forge new`: `go/parser` on the imports, `domain` and `application` import inward only, nothing but `app/bootstrap` composes, a failure names file, line and import; [ARCHITECTURE.md](ARCHITECTURE.md#architecture-test-of-the-generated-application)) | Done (`tests/architecture.rs` in every `forge new`: `use` trees flattened and inline paths scanned, an inner ring never imports an outer ring or a transport, database or vendor crate; syntactic, so `super::` paths that climb out of a ring are not seen) |
| `agents-md` | `AGENTS.md` for AI coding agents in the generated application | — | Done (`forge new` writes `AGENTS.md`, under 120 lines: rings, commands, routes seam, security defaults, tests; only what that application has; a test checks its commands and files; [AI_AGENT_GUIDE.md](AI_AGENT_GUIDE.md#the-generated-agentsmd)) | Planned, after forge-go 100% (decision D37; forge-rust#102): `forge new` writes the same `AGENTS.md` for Rust applications, with a test that checks its commands and files) |
| `forge-inspect` | `forge inspect`: read-only inventory of commands, migrations, static routes, modules and database adapter | — | Done (`forge inspect --json`; never executes application code) | Planned (forge-rust#103): Rust routes are registered by closures and computed paths, so the inventory needs the composed application, like `forge routes` (Forge-Rust ADR 0036) |
| `help-json-error-format` | Versioned command catalog and secret-safe JSON errors for automation | — | Done (`forge help --json`, `forge --error-format=json`; [AI_AGENT_GUIDE.md](AI_AGENT_GUIDE.md)) | Planned (forge-rust#104): the same schema-versioned catalog and error object |
| `new-flag-aliases` | `forge new` flag spellings: `--skip-deps` and `--skip-database` | — | Done (`forge new --skip-lockfile` is an alias of `--skip-deps`, Forge-Rust's name; `forge new --skip-database` is `-d none`) | Planned (forge-rust#105): `--skip-deps` as an alias of `--skip-lockfile` and `--skip-database` as an alias of `-d none`) |
| `new-skip-docker-ci` | `forge new` without Docker files or without CI | `rails new --skip-docker` | Planned (forge-go#87): `--skip-docker` and `--skip-ci` with conditional templates and a README that changes with them; the generated tests assume the Dockerfile, the compose stack and the workflow today) | Done (`forge new --skip-docker`: no `Dockerfile`, `.dockerignore`, `compose.yaml`, `compose.observability.yaml`, `observability/`; `forge new --skip-ci`: no `.github/`) |
| `doctor-database-probe` | `forge doctor` checks that the database server answers | `rails db:version` | Done (`forge doctor [--json]` opens a TCP connection to the database host within 3 seconds, never logs in, prints `database reachability: the server accepts connections` or `unreachable (REASON)`, reports the diagnostic `database.unreachable` and exits 1; SQLite reports whether the file exists) | Done (`forge doctor [--json]` opens a TCP connection to the database host within 3 seconds, with the redacted description of the URL; `crates/forge-cli/src/doctor.rs`) |
| `benchmarks` | Published benchmarks | — | Done (`go test -bench`, `scripts/bench.sh`, [BENCHMARKS.md](BENCHMARKS.md)) | Done (`cargo bench -p forge-bench`, `scripts/bench.sh`) |
| `getting-started` | Getting Started guide | Rails Guides | Done ([GETTING_STARTED.md](GETTING_STARTED.md)) | Done (`docs/getting-started.md`) |
| `head-routing` | `HEAD` on every `GET` route | `HEAD` routed to `GET` | Done (`http.ServeMux` serves `HEAD` with the `GET` route) | Done (the router serves `HEAD` with the `GET` route unless a `HEAD` route exists, the server drops the body, `Allow` lists `HEAD`; `crates/forge-http`) |
| `panic-500` | Handler panic answered with `500` | `ShowExceptions` | Done (`httpserver` recoverer: JSON error body with the request id) | Done (text body `internal server error`, `x-request-id`, logged with the request id; `crates/forge-http`) |
| `hsts` | HSTS behind TLS | `config.force_ssl` | Done (`Strict-Transport-Security` unless `FORGE_HTTP_TRANSPORT=plain`) | Done (same header and rule, `FORGE_HTTP_TRANSPORT=plain\|trusted-proxy`, no `tls` mode) |
| `db-tls-pool` | Verified database TLS in production, pool limits | `sslmode` in `database.yml`, `pool` | Done (`sslmode=verify-full\|verify-ca`, MySQL `tls=true`, `FORGE_DATABASE_ALLOW_PLAINTEXT`, `FORGE_DATABASE_MAX_CONNS`, `FORGE_DATABASE_STATEMENT_TIMEOUT` and the other pool keys) | Done (`sslmode=verify-full`, SQLx's MySQL spelling `ssl-mode=VERIFY_IDENTITY`; `verify-ca` is refused, a deliberate difference; `FORGE_DATABASE_ALLOW_PLAINTEXT`, `FORGE_DATABASE_MAX_CONNS` and the other pool keys; forge-rust#85) |
| `request-body-bound` | Bound on buffered request bodies | — | Not needed (bodies are streamed through `MaxBytesReader`) | Done (`FORGE_MAX_IN_FLIGHT_BODY_BYTES`, 256 MiB for all bodies at once, `503` with `Retry-After`) |
| `http-timeouts` | HTTP timeouts and request head bound | Puma `first_data_timeout`, `persistent_timeout` | Done (`FORGE_HTTP_READ_HEADER_TIMEOUT`, `FORGE_HTTP_READ_TIMEOUT`, `FORGE_HTTP_WRITE_TIMEOUT`, `FORGE_HTTP_IDLE_TIMEOUT`, `FORGE_HTTP_MAX_HEADER_BYTES`) | Done (same names, defaults and duration syntax; `FORGE_REQUEST_TIMEOUT_SECS` is the deprecated spelling of `FORGE_HTTP_WRITE_TIMEOUT`; forge-rust#85) |
| `shared-login-throttle` | Sign-in, reset and MFA throttles shared by replicas | `rate_limit` with a shared cache store | Done (`auth/postgres` `LoginThrottler`, `forge_login_throttle`) | Done (`forge::auth::PgLoginThrottle`, same table and transaction, wired in generated PostgreSQL applications; forge-rust#72) |
| `stream-key-rotation` | Stream signing key rotation without breaking open pages | `MessageVerifier#rotate` | Done (`FORGE_STREAMS_PREVIOUS_KEYS`, at most 3, redacted, `broadcast.DecodePreviousKeys`) | Done (`FORGE_STREAMS_PREVIOUS_KEYS`, same name, separator, limit and redaction; `Signer::from_config`, forge-rust#77) |
| `proxy-bounds` | forge-proxy bounds on request headers and connections | nginx `large_client_header_buffers`, `worker_connections` | Done (`forge-proxy run -max-header-bytes` 65536, `431` above; `-max-connections` 10000 per listener) | Done (`-max-header-bytes` and `-max-connections`, same names, defaults and range; forge-rust#77) |
| `csp-inline-hash` | Inline stylesheets allowed by hash, not `'unsafe-inline'` | `content_security_policy` | Done (the welcome page, with a test that recomputes the hash) | Done (the `/_mail` preview pages, with a test that recomputes the hash; the generated welcome page has no inline style) |
| `revendor-after-generate` | Re-vendor after `forge generate` | — | Done (`go mod vendor` in an application with `vendor/`, never on `--dry-run`) | Not applicable (`forge new` sets up no vendoring and no generator changes `Cargo.toml` or `Cargo.lock`) |
| `migrate-env` | Migration credential reaches the migrate step alone | — | Done (`migrate.env` in `config/deploy.yml`, forge-go#57) | Done (`migrate.env`, same schema, messages and dry-run plan; forge-rust#73) |
| `metrics-port-zero` | Metrics listener on a kernel-chosen port | — | Done (`FORGE_TELEMETRY_METRICS_ADDR=127.0.0.1:0`, bound address logged) | Done (same `FORGE_TELEMETRY_METRICS_ADDR=127.0.0.1:0`; forge-rust#85) |
| `stale-ticks` | Stale scheduler ticks skipped, not replayed | Solid Queue recurring tasks | Done (`SchedulerOptions.MaxLateness`, default one interval, skipped ticks reported) | Done (`Schedule::max_lateness`, default one interval, skipped ticks logged as a warning; forge-rust#85) |
| `cache-decode-failures` | Undecodable cached values counted and logged | — | Done (`cache.DecodeFailures()`, warn once per key with an HMAC-SHA256 key prefix) | Done (`forge_cache::decode_failures()`, the same log; forge-rust#85) |
| `dockerfile-pins` | Generated Dockerfile pinned to the framework's own images | — | Done (`TestDockerfilesPinBaseImagesByDigest`) | Done (the supply-chain gate `crates/forge/tests/supply_chain.rs` requires forge-proxy's exact references; forge-rust#85) |

## Parity follow-ups

- Associations and canonical field types (owner decisions D22 and D23 of
  2026-10-27, [ADR 0051](adr/0051-associations.md)). Forge-Rust lacks
  `field:references` (`belongs_to`) with its index and foreign key, composite
  `(org_id, post_id)` keys with organizations and the parent's
  `UNIQUE (org_id, id)`, `--on-delete=restrict|cascade`, the typed in-use
  conflict on every backend, the nested `/posts/{id}/comments` routes and
  parent select, the parent page's `_associations` Turbo Frames, `forge
  generate join` and `forge destroy join`, `forge destroy` refusing a
  referenced resource, `forge generate tenancy` refusing associations, and
  the canonical type names with their aliases. Planned after Forge-Go
  reaches 100% (decision D37), in
  [forge-rust#96](https://github.com/rocketinsidecorp/forge-rust/issues/96),
  with the same commands, names and generated routes.

- Signed release binaries (forge-go decision D24,
  [RELEASING.md](RELEASING.md)). Forge-Rust's `release.yml` publishes signed
  images only. It still lacks: archives of `forge` and `forge-proxy` for
  linux, darwin and windows on amd64 and arm64 named
  `<command>_<version>_<os>_<arch>` with the same layout (binary, LICENSE,
  README.md), a signed `checksums.txt`, build provenance attestations, a
  draft release with the SBOMs, a `forge-proxy version` command, the Go
  toolchain in `forge version` (Rust: the `rustc` version), the README
  Install section and a `RELEASING.md`. Per decision D37 it follows after
  Forge-Go reaches 100%; tracked in
  [rocketinsidecorp/forge-rust#100](https://github.com/rocketinsidecorp/forge-rust/issues/100).

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
  it is planned after forge-go reaches 100% (decision D37, tracked in
  [forge-rust#98](https://github.com/rocketinsidecorp/forge-rust/issues/98)),
  with the same `FORGE_LOG_FORMAT`, `FORGE_LOG_LEVEL`, `FORGE_LOG_SLOW_QUERY` and
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
  that no longer decode (`cache.DecodeFailures`). Forge-Rust adopted all of it
  in [forge-rust#85](https://github.com/rocketinsidecorp/forge-rust/issues/85)
  (closed): the `TRUNCATE`/`REFERENCES`/`TRIGGER` rule (column `REFERENCES`
  included) and the unprotected-partition rule with the same catalog
  predicates and reasons in `require_row_security`; the refusal on every pool
  a command opens (it runs for `serve`, `jobs work` and the `jobs` commands,
  while application commands such as `seed` and `setup` connect with the
  migration role and opt out), with `PoolSettings::allow_row_security_bypass()`
  as the opt-out; digest-pinned bases checked by its supply-chain gate; a
  counter and one warning per key for undecodable cache values.
  Both frameworks now refuse `PGOPTIONS` for every pool but the migrator's,
  floor an explicit scheduler lateness at a minute, and refuse a
  `FORGE_TELEMETRY_METRICS_ADDR` that overlaps `FORGE_HTTP_ADDR` by the same
  rule (same non-zero port on the same or an unspecified host; port 0 never
  overlaps).

- CLI differences that remain after `forge check`, `forge format` and
  `forge lint` (forge-go QG10, audit gap 3). Flag-parse and usage errors of
  every Forge-Go subcommand now exit 2, like Forge-Rust's clap errors.
  - `forge format --check` (list the files, write nothing, exit 1) exists in
    Forge-Go; Forge-Rust's `forge format` took no flags and added
    `--check` (`cargo fmt --all -- --check --files-with-diff`) with the same
    meaning since forge-rust#89. Done in both.
  - `forge new --skip-lockfile` (Forge-Rust's name) is accepted by Forge-Go as
    an alias of `--skip-deps`: neither resolves dependencies (`go get` and
    `go mod tidy`; `cargo generate-lockfile`). Forge-Rust adopts `--skip-deps`
    as an alias of `--skip-lockfile`, and `--skip-database` as an alias of
    `-d none`: row `new-flag-aliases`.
  - `forge new --skip-docker` and `--skip-ci` (Forge-Rust) do not exist in
    Forge-Go. The generated README, the deploy configuration and the
    generated tests assume the Dockerfile, the compose stack and the CI
    workflow, so the flags need conditional templates and a README that
    changes with them; Forge-Go adopts both flags with Forge-Rust's meaning
    (no `Dockerfile`, `.dockerignore`, `compose.yaml`,
    `compose.observability.yaml`, `observability/`, `docker/`; no
    `.github/`) in a change that owns the scaffold templates. Row
    `new-skip-docker-ci`.
  - `forge new --api` exists in both (forge-go QG11, audit P-1) with the same
    conflicts (`--api-auth`, `--locale`) and the same refusals (`generate
    view`, `scaffold`, `authentication`, an HTML `controller`). (1) In a
    Forge-Go `--api` application `generate resource` and `controller --api`
    write their JSON layer and print wiring for `Configure`; Forge-Rust
    refused both while its `--api` bootstrap had no `fn compose` to wire
    them into. Forge-Rust adopted Forge-Go's behaviour by giving `--api`
    applications a `compose` hook (forge-rust#90, QR7b): Done in both. (2)
    The schema of
    an `--api` PostgreSQL application: Done (orchestrator decision D15,
    Forge-Rust's schema is the reference). Both keep Forge's identity, audit
    and session migrations: Forge-Rust's `forge_audit`, `forge_identity` and
    `forge_session_idle_timeout`, Forge-Go's `authpostgres.Migrations()` in
    `cmd/NAME/main.go`. Forge-Go's identity set is one ordered framework set,
    so it also creates the tables Forge-Rust adds only with the sign-in pages
    (tenants, password resets, MFA, login throttle): a superset, never a
    missing table. Neither serves them: no browser pipeline, no sign-in pages,
    no `setup` command, and the generators print no identity wiring. (3)
    Forge-Rust's matrix has the `rails new --api` row, Done.
  - `forge inspect`, `forge help --json` and `--error-format=json` exist in
    Forge-Go only (rows `forge-inspect` and `help-json-error-format`).
    `forge db schema:dump`, `forge db schema:load` and the browser reload of
    `forge dev` too (rows `db-schema-dump-load` and `dev-browser-reload`).

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
  Rust adopted the wake-up in
  [forge-rust#80](https://github.com/rocketinsidecorp/forge-rust/issues/80)
  (closed, ADR 0046): `NOTIFY forge_jobs` (payload: the queue name), a worker
  `LISTEN` owned by `Jobs::run` with the same reconnect backoff, behind
  `PgJobStore::notify(true)`, a `WorkerSettings` batch bound of 10 never above
  the free slots, and an outbox batch of 10, the same default as Go. Rust
  also reads `FORGE_JOBS_NOTIFY` and has `App::jobs_notify(true)`; Go has the
  store option and, since forge-go#72, the variable (`config.Jobs.Notifies`;
  row `jobs-notify-switch`). Differences that are not gaps: Go exposes
  `Worker.Listening()` where Rust logs the subscription, and Go's idle poll
  doubles to `MaxPollInterval` (10 s) where Rust polls every `poll_interval`
  (1 s).

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
  rotating `FORGE_STREAMS_KEY` no longer breaks open pages. Forge-Rust
  adopted the variable with the same name, separator, limit and redaction
  (`Signer::from_config`, forge-rust#77, closed); its rotation procedure is in
  its
  [deployment](https://github.com/rocketinsidecorp/forge-rust/blob/main/docs/deployment.md#rotating-the-stream-signing-key).

- forge-proxy connection and header limits (forge-go QG4b, audit S-8):
  Forge-Go's `forge-proxy run` has `-max-header-bytes` (default 65536, 4096 to
  1048576, `431` above it; was 1 MiB) and `-max-connections` (default 10000
  per listener, admission before accept). Forge-Rust caps a listener at the
  same 10,000 connections with a constant and sets hyper's `max_buf_size` to
  1 MiB: it adopted both flags with the same names, defaults and range and the
  64 KiB header default (forge-rust#77, closed). Neither framework exposes
  them in `config/deploy.yml`.

- Welcome page CSP and revendoring (forge-go QG4b,
  [forge-rust#77](https://github.com/rocketinsidecorp/forge-rust/issues/77)):
  Forge-Go's generated welcome page allows its one inline stylesheet by
  SHA-256 hash instead of `style-src 'unsafe-inline'`, with a generated test
  that recomputes the hash from the served page, and `forge generate ...` runs
  `go mod vendor` in an application that vendors its dependencies. Forge-Rust
  allows its mail preview pages' inline stylesheet by hash with a test that
  recomputes it, its generated welcome page has no inline style, and it needs
  no re-vendor step (no generator changes its dependency set; a test runs every
  generator against a vendored application).

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
  variants and the output format of both suites; Forge-Rust adopted them
  (`cargo bench -p forge-bench`, forge-rust#41, closed) and its results table
  uses the same scenarios and columns, so the two can be read side by side.

- Deploy hardening (forge-go#36, from the review of forge-rust#39):
  `ssh.proxy_jump` and `registry.server` refuse a leading `-` with the same
  patterns in both frameworks, `retain` must be 2 or more (1 pruned the
  previous version, so rollback was impossible), and the deploy lock is
  released only while its record is the deploy's own, with a record-less lock
  older than a minute recovered without `--force`. Both refuse YAML
  nesting deeper than 100 levels and a `deploy.yml` over 1 MiB with a
  line-numbered error (forge-go#89, row `deploy-yml-caps`). Forge-Rust mirrors the lock scripts and the four golden plan
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
