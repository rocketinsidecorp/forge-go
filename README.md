# Forge

Forge is a Go framework for building HTTP services on PostgreSQL, MySQL,
MariaDB, or SQLite with secure defaults, a conventional application layout,
and a production container from the first commit. The core packages use only
the Go standard library; each database adapter adds its driver.

## Status

Pre-release. Phases 1 (core runtime, configuration, CLI, lifecycle, HTTP) and
2 (databases, migrations, transactions, repositories) are implemented,
including support for several databases. Phase 3 provides an identity and
tenancy foundation with PostgreSQL persistence, password and browser-session
flows, recovery and MFA primitives, and security auditing. Phase 4 adds
background jobs, a transactional outbox with bounded concurrent dispatch,
dead-letter replay, and an in-process scheduler. Its remaining scope and
adapter boundaries are listed in the roadmap. APIs may change
without notice until a tagged release.

Phase 7 now includes a provider-neutral `agent` action engine and a
PostgreSQL-backed execution journal. It does not call an LLM: applications
register actions and supply policy, approval, and identity integrations. The
engine takes identity only from authenticated middleware, checks tenant
consistency, and durably marks uncertain post-execution outcomes for
reconciliation. Policy and approval callbacks remain application trust points.
Destructive/external actions require an approval adapter; the CLI itself is
not an agent authorization boundary. See the [AI agent guide](docs/AI_AGENT_GUIDE.md).

The PostgreSQL transactional outbox supports bounded concurrent delivery,
leases, acknowledgements, bounded retries, dead-letter replay, and queue
metrics. An in-process job scheduler (`jobs.Scheduler`) runs recurring work
with configurable concurrency. Optional OpenTelemetry server spans, HTTP
duration metrics, and scheduled-job spans are available; Forge's request-scoped
logs carry trace IDs. Database/outbox instrumentation and global `slog` bridging
remain in progress.
OpenAPI, resource code generation, performance work, and further security
hardening remain planned. See [docs/ROADMAP.md](docs/ROADMAP.md) for the full
status.

## Packages

Core packages import only the standard library and each other; a test
(`architecture_test.go`) enforces this
([ADR 0007](docs/adr/0007-postgresql-adapter-with-pgx.md),
[ADR 0009](docs/adr/0009-multiple-databases.md)).

| Package      | Purpose |
| ------------ | ------- |
| `forge`      | `forge.Main` entrypoint, `App` composition, shutdown hooks, binary subcommands, health routes |
| `auth`       | Opaque bearer-session tokens, current-session resolver middleware, principals and deny-by-default permissions |
| `agent`      | Provider-neutral allowlisted action pipeline with policy, validation, principal permissions, explicit approval and mandatory journaling |
| `tenancy`    | Fail-closed tenant context; used by authentication and tenant-aware database transactions |
| `config`     | Environment configuration with fail-fast validation; database selection by URL scheme; `config.Secret` for redacted values |
| `router`     | Route registry on `net/http.ServeMux` (Go 1.22 patterns), frozen after the first request; JSON `400`/`404`/`405` |
| `httpserver` | HTTP server with timeouts, body limit, security headers, request IDs, panic recovery, graceful shutdown, and composable application middleware |
| `observability` | Optional OpenTelemetry HTTP spans and server metrics; application-owned providers and exporters |
| `web`        | JSON responses, the single error format, strict JSON decoding, request ID and request-scoped logger |
| `views`      | Concurrent-safe server-rendered HTML templates with contextual escaping and buffered rendering |
| `pagination` | Keyset pagination over UUIDv7 with opaque cursors |
| `events`     | Versioned event envelope with bounded JSON payloads |
| `jobs`       | In-process scheduler with bounded concurrency, fixed-rate, daily, and numeric 5-field cron schedules |
| `health`     | Readiness check registry; `/health`, `/health/live`, `/health/ready` |
| `uuid`       | RFC 9562 UUIDv7 (default identifier), monotonic generator, JSON/text/SQL support |
| `fault`      | Typed application errors (code, message, category, cause, metadata, retryable, HTTP status) |
| `sqldb`      | Database-independent error translation, transaction retries, bounded pool shutdown, `database/sql` wrapper with a sqlc-compatible `DBTX` |
| `migrate`    | Migration engine (SQL files, checksums, locks, `migrate`/`rollback` commands) with a per-database `Dialect` |
| `dbtest`     | Test-server discovery shared by the adapters' test packages |

Adapter packages (third-party dependencies allowed):

| Package                 | Purpose |
| ----------------------- | ------- |
| `postgres`              | PostgreSQL on pgx v5: pool, `DBTX`, transactions with retry, error translation, advisory locks, migrations |
| `outbox/postgres`       | PostgreSQL transactional outbox persistence and concurrent dispatcher |
| `agent/postgres`        | Tenant-RLS-protected PostgreSQL journal with replay refusal, bounded reconciliation listing, and one-time operator decisions |
| `mysql`                 | MySQL and MariaDB on go-sql-driver/mysql: hardened session, error translation, migrations |
| `sqlite`                | SQLite on the pure-Go modernc.org/sqlite: enforced pragmas, error translation, migrations with a file lock |
| `postgres/postgrestest`, `mysql/mysqltest`, `sqlite/sqlitetest` | A fresh database per test |

An application links only the adapter it imports. The `forge` CLI lives in
`cmd/forge`.

## Quickstart

Requires Go 1.27.1+ and Docker with BuildKit and Compose.

```sh
go install github.com/fabriciobonjorno/forge-go/cmd/forge@latest
forge new myapp                 # PostgreSQL; or -d mysql, -d mariadb, -d sqlite, -d none
cd myapp
docker compose up --build
curl -i http://127.0.0.1:8080/v1/status
curl -i http://127.0.0.1:8080/health/ready   # includes the "database" check
```

| `--database` (`-d`) | Compose service | Adapter |
| ------------------- | --------------- | ------- |
| `postgresql` (default; also `postgres`, `pg`) | `postgres:18-alpine` | `postgres` |
| `mysql` | `mysql:8.4` | `mysql` |
| `mariadb` | `mariadb:11.8` | `mysql` |
| `sqlite` (also `sqlite3`) | none; the database file is on a `storage` volume | `sqlite` |
| `none` (same as `--skip-database`) | none | none |

`docker compose up` starts the database server (if any), runs the
application's `migrate` command once, then starts the application. A database
server is published on `127.0.0.1:<port>`, a port between 20000 and 29999
derived from the application name so it does not clash with a local server
on its default port; the generated README and `.env.development` contain the
exact value.

Running the application outside Docker (server databases):

```sh
docker compose up -d postgres   # or mysql, mariadb
forge migrate
forge dev
```

With SQLite, `forge migrate` and `forge dev` use `storage/development.db`
directly.

Adding a table:

```sh
forge generate migration create_notes --dry-run # preview exact migration files first
forge generate migration create_notes   # edit db/migrations/<timestamp>_create_notes.{up,down}.sql
forge migrate
forge test
```

Generate a server-rendered HTML view (templates use Go's contextual HTML
escaping and are embedded into the application binary):

```sh
forge generate view welcome --dry-run   # preview files without writing
forge generate view welcome
forge generate job cleanup --dry-run   # preview job and test files
forge generate job cleanup
```

The generator creates `app/views/welcome.gohtml` and, on the first view,
`app/views/renderer.go`. Build one renderer during application composition
with `views.NewRenderer()`, then call `renderer.Render(w, "welcome.gohtml",
http.StatusOK, data)` from an HTTP handler. Execution errors happen before
response headers or body bytes are written.

`forge new myapp` generates:

```text
myapp/
  go.mod
  cmd/myapp/main.go            # forge.Main(bootstrap.Configure, <adapter>.Commands(db.Migrations())...)
  app/bootstrap/bootstrap.go   # opens the database, readiness check "database", GET /v1/status
  app/bootstrap/bootstrap_test.go
  db/db.go                     # embeds db/migrations into the binary
  db/migrations/.keep
  .env.development             # committed development settings; no secrets
  Dockerfile                   # multi-stage, distroless, nonroot
  .dockerignore                # excludes .git, .github, storage, all .env* files
  compose.yaml                 # database server (if any), one-off migrate, app
  .gitignore                   # ignores .env, .env.local, .env.*.local (and /storage/ for SQLite)
  README.md
  .github/workflows/ci.yml     # gofmt, vet, test -race (with a database service), govulncheck, docker build
  .github/dependabot.yml       # weekly Go module, Actions and Docker updates
```

With `--database none`, `db/`, the database wiring, and the Compose and CI
database services are omitted. With SQLite, the image creates
`/app/storage` owned by the nonroot user and sets
`FORGE_DATABASE_URL=sqlite:///app/storage/production.db`; mount a persistent
volume there.

The generated `Dockerfile` builds a static binary (`CGO_ENABLED=0`, also for
SQLite) and runs it in `gcr.io/distroless/static-debian12:nonroot`,
configured for production behind a TLS-terminating proxy. See
[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) and
[docs/DATABASE.md](docs/DATABASE.md).

## CLI

| Command | Description |
| ------- | ----------- |
| `forge new NAME [--database DB] [--module PATH] [--dir DIR] [--framework-path DIR] [--skip-deps] [--skip-database]` | Create an application. Refuses to write into a non-empty directory. |
| `forge dev [NAME]` | Build `./cmd/<name>` into a temporary directory and run it, forwarding interrupts for graceful shutdown |
| `forge test [ARGS]` | `go test` with the development environment loaded; `ARGS` default to `./...` |
| `forge build [NAME]` | `go build -trimpath` into `bin/<name>` |
| `forge migrate [status]` | Build the application and run its `migrate` (or `migrate status`) command |
| `forge rollback [-steps N]` | Build the application and run its `rollback` command (default 1 step) |
| `forge generate migration NAME [--dry-run]` | Create or preview `db/migrations/<UTC timestamp>_NAME.{up,down}.sql` |
| `forge generate view NAME [--dry-run]` | Create or preview an embedded server-rendered Go HTML template with contextual escaping |
| `forge generate job NAME [--dry-run]` | Create or preview a cancellation-aware recurring job stub and tests; registration and schedule remain explicit |
| `forge doctor [--json]` | Load and validate configuration; JSON mode emits versioned, secret-safe diagnostics with remediation codes |
| `forge inspect --json` | Read-only, versioned inventory of commands, migrations, static routes, modules and database adapter |
| `forge help --json` | Emit the versioned command catalog with per-command flags, examples, environment inputs, exit codes and side effects |
| `forge uuid [--count N]` | Print N UUIDv7 values (1-1000) |
| `forge version` | Print CLI version, commit, and build date |

`NAME` for `dev` and `build` is needed only if `cmd/` has several commands.
`migrate` and `rollback` require exactly one.

Flags for `forge new`:

- `NAME` must match `^[a-z][a-z0-9-]{0,62}$`.
- `--module PATH` sets the Go module path (default: `NAME`).
- `--dir DIR` sets the output directory (default: `NAME`); it must be absent
  or empty.
- `--skip-deps` skips dependency resolution (`go get`, `go mod tidy`).
- `--database DB` / `-d DB` chooses the database: `postgresql` (default;
  aliases `postgres`, `pg`), `mysql`, `mariadb`, `sqlite` (alias `sqlite3`), or
  `none`.
- `--skip-database` is `--database none`; combining it with `--database` is
  an error.
- `--framework-path DIR` is for framework contributors: it adds a `replace`
  directive pointing at a local checkout, then runs `go mod tidy` and
  `go mod vendor` so the Docker build context is self-contained. Without it,
  `forge new` runs `go get github.com/fabriciobonjorno/forge-go@<cli version or
  latest>` followed by `go mod tidy`.

`forge generate migration NAME` requires `NAME` to match
`^[a-z][a-z0-9_]{0,99}$` and `db/migrations` to exist. If a migration with the
same second already exists, the version is moved forward one second. It never
overwrites a file.

`forge generate job NAME [--dry-run]` creates `app/jobs/NAME.go` and its test
file. The generated stub returns an explicit not-implemented error until it
is replaced, so registering it cannot silently report successful work. It does
not edit `bootstrap.JobsConfigure`: choose a schedule and register the handler
there explicitly. `NAME` must be lowercase snake_case with each segment
starting with a letter. Scheduler failures are reported but are not retried
automatically; jobs that run on multiple replicas must be idempotent or use a
PostgreSQL advisory lock when only one replica may perform a schedule tick.

### Development environment files

`forge dev`, `forge test`, `forge migrate`, `forge rollback`, and
`forge doctor` read `.env.development` and then `.env.local` from the current
directory; a later file overrides an earlier one, and variables already set in
the process environment always win. `dev`, `test`, `migrate`, and `rollback`
set `FORGE_ENV=development` when no source sets it.

The format is deliberately small: `KEY=VALUE` lines, blank lines and lines
starting with `#` ignored, an optional `export ` prefix, and one pair of
matching single or double quotes removed from the value. There is no variable
interpolation and no inline comment syntax. Parse errors name the line
number, never its content.

`.env.development` is committed and must not contain real secrets; put
personal overrides in `.env.local`, which is git-ignored.

### Application binary commands

Binaries built with `forge.Main` accept:

| Command | Description |
| ------- | ----------- |
| `serve` | Default. Serve HTTP until `SIGINT`/`SIGTERM` |
| `healthcheck` | Probe `/health/live` on loopback; used by the Docker `HEALTHCHECK` |
| `help` | List the available commands |
| `migrate [status]`, `rollback [-steps N]` | Added by `<adapter>.Commands` (generated applications with a database) |

Exit status is `0` on success, `1` on failure (including invalid
configuration), and `2` for an unknown command or invalid arguments.

For agent and script integrations, prefix any Forge CLI command with
`--error-format=json` to emit a versioned, secret-safe error object on stderr
when the command fails. Error messages intentionally omit underlying error
details and input values; `doctor --json` and `inspect --json` keep their
command-specific diagnostic reports on stdout.

## Example

[`examples/basic-api`](examples/basic-api) is the reference service: a task
API on PostgreSQL laid out in hexagonal layers (`app/domain`,
`app/application`, `app/adapters/persistence`, `app/adapters/httpapi`,
`app/bootstrap`) with SQL migrations in `db/migrations`. It demonstrates
repositories on `DBTX`, a transaction, optimistic locking with a `version`
column, keyset pagination, and the JSON error format. Endpoints:
`POST /v1/tasks`, `GET /v1/tasks`, `GET /v1/tasks/{id}`, and
`POST /v1/tasks/{id}/complete`.

The repository's `compose.yaml` runs it with PostgreSQL:

```sh
docker compose up --build
curl -i -X POST -H 'Content-Type: application/json' -d '{"title":"try forge"}' \
  http://127.0.0.1:8080/v1/tasks
```

## Developing the framework

```sh
go vet ./...
golangci-lint run
go test -race ./...
govulncheck ./...
```

SQLite tests always run. PostgreSQL and MySQL/MariaDB tests are skipped
unless a server is configured whose user can create databases:

```sh
export FORGE_TEST_POSTGRES_URL='postgres://forge:forge@127.0.0.1:5432/postgres?sslmode=disable'
export FORGE_TEST_MYSQL_URL='mysql://root:forge@127.0.0.1:3306/mysql'   # MySQL or MariaDB
```

Set `FORGE_TEST_REQUIRE_DATABASE=true` to make a missing server a failure.

CI (`.github/workflows/ci.yml`):

- `quality`: gofmt, vet, golangci-lint, race tests against PostgreSQL 18
  and MySQL 8.4 services, fuzzing of the scaffold name validation and the
  pagination cursor, benchmarks, and govulncheck;
- `mariadb`: the `mysql` adapter's tests against MariaDB 11.8;
- `docker`: for each of PostgreSQL, MySQL, MariaDB, and SQLite, generates an
  application, builds its image, and smoke-tests its Compose stack with
  `docker compose up --wait` (the PostgreSQL run also builds the example
  image).

## Documentation

- [Architecture](docs/ARCHITECTURE.md)
- [Authentication and authorization](docs/AUTHENTICATION.md)
- [AI agent guide and current authorization boundaries](docs/AI_AGENT_GUIDE.md)
- [Database](docs/DATABASE.md)
- [Deployment](docs/DEPLOYMENT.md)
- [Observability](docs/OBSERVABILITY.md)
- [Roadmap](docs/ROADMAP.md)
- [Threat model](docs/THREAT_MODEL.md)
- [Multi-tenancy](docs/MULTI_TENANCY.md)
- [Security policy](SECURITY.md)
- Architecture decision records: [docs/adr](docs/adr)
  - [0001 UUIDv7 as the default identifier](docs/adr/0001-uuidv7-default-identifier.md)
  - [0002 Standard library `net/http` core](docs/adr/0002-stdlib-net-http-core.md)
  - [0003 Docker by default](docs/adr/0003-docker-by-default.md)
  - [0004 Hexagonal application layout](docs/adr/0004-hexagonal-application-layout.md)
  - [0005 Environment configuration, fail fast](docs/adr/0005-environment-configuration-fail-fast.md)
  - [0006 Zero-dependency core](docs/adr/0006-zero-dependency-core.md)
  - [0007 PostgreSQL adapter with pgx, in the main module](docs/adr/0007-postgresql-adapter-with-pgx.md)
  - [0008 SQL migrations embedded in the application binary](docs/adr/0008-sql-migrations-embedded-in-the-binary.md)
  - [0009 Multiple databases: PostgreSQL, MySQL, MariaDB and SQLite](docs/adr/0009-multiple-databases.md)
