<p align="center">
  <img src="internal/scaffold/templates/assets/forge-go-logo.png" alt="Forge Go — Go Framework" width="280">
</p>

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
flows, recovery and MFA primitives, security auditing, generated sign-in
pages and a `setup` command for the first owner. Phase 4 adds
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
with configurable concurrency. Phase 5 adds dependency-free observability:
traces with W3C propagation, bounded-cardinality metrics for Prometheus and
OTLP, trace-correlated logs, and database and job instrumentation
([docs/OBSERVABILITY.md](docs/OBSERVABILITY.md)). OpenAPI, performance work,
and further security hardening remain planned. See [docs/ROADMAP.md](docs/ROADMAP.md) for the full status.

## Packages

Core packages import only the standard library and each other; a test
(`architecture_test.go`) enforces this
([ADR 0007](docs/adr/0007-postgresql-adapter-with-pgx.md),
[ADR 0009](docs/adr/0009-multiple-databases.md)).

| Package      | Purpose |
| ------------ | ------- |
| `forge`      | `forge.Main` entrypoint, `App` composition, shutdown hooks, binary subcommands, health routes |
| `auth`       | Opaque bearer-session tokens, current-session resolver middleware, principals and deny-by-default permissions; browser sign-in, sign-out, MFA and password-reset flows (`auth.Browser`) and sign-in redirects for HTML pages |
| `agent`      | Provider-neutral allowlisted action pipeline with policy, validation, principal permissions, explicit approval and mandatory journaling |
| `tenancy`    | Fail-closed tenant context; used by authentication and tenant-aware database transactions |
| `config`     | Environment configuration with fail-fast validation; database selection by URL scheme; `config.Secret` for redacted values |
| `router`     | Route registry on `net/http.ServeMux` (Go 1.22 patterns), frozen after the first request; JSON `400`/`404`/`405`; conventional `Resources` routes |
| `httpserver` | HTTP server with timeouts, body limit, security headers, request IDs, panic recovery, graceful shutdown, and composable application middleware |
| `web`        | JSON responses, the single error format, strict JSON and form decoding, safe redirects, content negotiation, flash messages, method override, request ID and request-scoped logger |
| `csrf`       | CSRF protection for browser requests: double-submit token in forms and headers, cross-origin rejection |
| `views`      | Concurrent-safe server-rendered HTML with layouts, partials, CSRF and flash helpers, contextual escaping and buffered rendering |
| `pagination` | Keyset pagination over UUIDv7 with opaque cursors |
| `events`     | Versioned event envelope with bounded JSON payloads |
| `jobs`       | In-process scheduler with bounded concurrency, fixed-rate, daily, and numeric 5-field cron schedules |
| `ratelimit`  | Rails-style `rate_limit`: fixed-window limiter, per-route 429/`Retry-After` middleware, bounded in-memory store; shared stores in `ratelimit/postgres`, `/mysql`, `/sqlite` |
| `cache`      | Solid Cache equivalent: `Cache` with `Fetch` stampede protection, bounded LRU+TTL memory cache, typed JSON helpers; shared stores in `cache/postgres`, `/mysql`, `/sqlite` |
| `storage`    | Active Storage equivalent for local disk: `Storage` port, HMAC-signed expiring links, safe serving handler, strict multipart upload; `storage/disk` adapter |
| `telemetry`  | Tracing with W3C trace context, metrics with a cardinality cap, Prometheus text and OTLP/HTTP JSON export, trace-correlated logs |
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
| `ratelimit/{postgres,mysql,sqlite}`, `cache/{postgres,mysql,sqlite}` | Shared rate limit buckets and application cache in the database, with embedded migrations |
| `postgres/postgrestest`, `mysql/mysqltest`, `sqlite/sqlitetest` | A fresh database per test |

An application links only the adapter it imports. The `forge` CLI lives in
`cmd/forge`; the deploy proxy, forge-proxy, in `cmd/forge-proxy` (its
`go.yaml.in/yaml/v3` and `golang.org/x/crypto` dependencies are not linked
into applications, [ADR 0030](docs/adr/0030-forge-deploy.md)).

## Quickstart

Requires Go 1.27.1+ and Docker with BuildKit and Compose.

```sh
go install github.com/rocketinsidecorp/forge-go/cmd/forge@latest
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

### Sign in (PostgreSQL)

PostgreSQL applications come with Rails-style sign-in, sign-out, two-factor
and password-reset pages. End to end, from a new application to a signed-in
owner using a scaffold:

```sh
forge new myapp && cd myapp
docker compose up -d postgres
forge migrate                   # identity tables and the application's migrations
forge setup --tenant acme --email you@example.com --organization "Acme Inc"
                                # prompts twice for the owner password (12+ characters)
forge seed                      # optional: loads db/seeds.go (idempotent)
forge dev
```

Open `http://localhost:8080/session/new` and sign in with organization `acme`,
your email and password (use `localhost`: the `__Host-` cookies need a secure
context, which browsers grant to `http://localhost`). Then add pages and use
them as the owner:

```sh
forge generate scaffold post title:string body:text --tenant-scoped
# paste the printed wiring into app/bootstrap/bootstrap.go
forge migrate                   # also grants posts:read and posts:write to the owner role
forge dev                       # http://localhost:8080/posts (signed-out visitors go to /session/new and back)
```

| URL | Page |
| --- | ---- |
| `/session/new` | sign in (organization, email, password) |
| `/session/mfa` | authenticator or backup code, when `FORGE_AUTH_MFA_KEY` is set |
| `/passwords/new` | forgot password (only when reset emails are configured, below) |
| `/passwords/edit?token=...` | choose a new password from the emailed link |

Password-reset emails are off until outgoing mail is configured, and so are
the reset pages and the "Forgot your password?" link. To try them locally,
add to `.env.local` (or uncomment the same lines in `.env.development`),
restart `forge dev`, request a reset at `http://localhost:8080/passwords/new`
and open the link in the newest `tmp/mail/*.eml`:

```sh
cat >> .env.local <<'ENV'
FORGE_MAIL_URL=file:tmp/mail
FORGE_MAIL_FROM="myapp <no-reply@example.com>"
FORGE_AUTH_PASSWORD_RESET_URL=http://localhost:8080/passwords/edit
ENV
```

In production set `FORGE_MAIL_URL=smtps://user:password@smtp.example.com:465`
(or `smtp://...:587` for STARTTLS), `FORGE_MAIL_FROM` and
`FORGE_AUTH_PASSWORD_RESET_URL=https://app.example.com/passwords/edit`; see
[docs/MAIL.md](docs/MAIL.md).

Sign out with a `DELETE /session` form: include
`{{template "sessions/_sign_out" .}}` in a page. `forge setup` refuses once a
tenant exists. The same commands exist in the application binary, for
containers and production (the password is never an argument):

```sh
docker compose run --rm app setup --tenant acme --email you@example.com
printf '%s\n' "$OWNER_PASSWORD" | myapp setup --tenant acme --email you@example.com --password-stdin
myapp seed
```

An application created before the sign-in pages existed gets them with:

```sh
forge generate authentication --dry-run   # preview files and the wiring to paste
forge generate authentication             # app/adapters/htmlauth, app/views/{sessions,passwords}
```

MySQL, MariaDB and SQLite applications are refused: Forge persists identities
only in PostgreSQL. They still get `db/seeds.go` and `forge seed`. See
[docs/AUTHENTICATION.md](docs/AUTHENTICATION.md#server-rendered-sign-in-pages).

### Tables, pages and jobs

Adding a table:

```sh
forge generate migration create_notes --dry-run # preview exact migration files first
forge generate migration create_notes   # edit db/migrations/<timestamp>_create_notes.{up,down}.sql
forge migrate
forge test
```

Generate a working HTML CRUD (index, show, new, edit, create, update and
destroy with forms, validation errors, flash notices and CSRF-protected forms)
together with the migration, domain, application, persistence and JSON API of
the resource:

```sh
forge generate scaffold post title:string body:text published:bool --global --dry-run
forge generate scaffold post title:string body:text published:bool --global
forge migrate
forge test
```

The command prints the wiring to paste into `app/bootstrap/bootstrap.go`; the
pages are served at `/posts` and are never public (see
[docs/VIEWS.md](docs/VIEWS.md#scaffold-a-crud)). In a PostgreSQL application a
signed-out browser visiting `/posts` is sent to the sign-in page and back; once
signed in as the owner, the pages work.

Generate a single server-rendered page (templates use Go's contextual HTML
escaping and are embedded into the application binary):

```sh
forge generate view welcome --dry-run   # preview files without writing
forge generate view welcome
forge generate job cleanup --dry-run   # preview job and test files
forge generate job cleanup
```

The generator creates `app/views/pages/welcome.gohtml` (or
`app/views/DIR/NAME.gohtml` for `forge generate view DIR/NAME`) and, for an
application that has no views package, the base `forge new` generates:
`app/views/views.go`, `layouts/application.gohtml` and `shared/_flash.gohtml`.
Build the renderer once during application composition with `views.New()`, then
call `renderer.RenderRequest(w, r, "pages/welcome", http.StatusOK, data)` from an
HTTP handler (`data` needs `Title` and `Content`). Execution errors happen before
response headers or body bytes are written.

`forge new myapp` generates:

```text
myapp/
  go.mod
  cmd/myapp/main.go            # forge.Main(bootstrap.Configure, <adapter> migrate/rollback, seed (and setup) commands)
  app/bootstrap/bootstrap.go   # opens the database, readiness check "database", GET /v1/status
  app/bootstrap/bootstrap_test.go
  app/adapters/htmlauth/       # PostgreSQL: sign-in, sign-out, two-factor and password-reset pages
  app/views/                   # layout, flash partial (and, with PostgreSQL, sessions/ and passwords/)
  db/db.go                     # embeds db/migrations into the binary
  db/seeds.go                  # db.Seed: idempotent seed data for `seed`
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
| `forge setup --tenant SLUG --email EMAIL [--organization NAME] [--password-stdin]` | PostgreSQL: create the first organization, tenant and owner account; the password is prompted for (or piped, or `FORGE_SETUP_PASSWORD`), never an argument; refuses once a tenant exists |
| `forge seed` | Run the application's `seed` command: `db.Seed` in one transaction |
| `forge generate migration NAME [--dry-run]` | Create or preview `db/migrations/<UTC timestamp>_NAME.{up,down}.sql` |
| `forge generate view [DIR/]NAME [--dry-run]` | Create or preview a server-rendered page, `app/views/DIR/NAME.gohtml` (DIR defaults to `pages`), in the layout `forge new` generates |
| `forge generate job NAME [--dry-run]` | Create or preview a cancellation-aware recurring job stub and tests; registration and schedule remain explicit |
| `forge generate resource NAME field:type... (--tenant-scoped \| --global) [--plural P] [--dry-run]` | Create or preview a complete resource: migration, domain, application, persistence and HTTP layers with tests, for the application's database. Never public: see below |
| `forge generate scaffold NAME field:type... (--tenant-scoped \| --global) [--plural P] [--broadcast] [--dry-run]` | Everything `generate resource` creates plus server-rendered HTML CRUD pages, forms and their tests; `--broadcast` adds live updates over Server-Sent Events. Never public: see below |
| `forge generate authentication [--dry-run]` | PostgreSQL: add the sign-in, sign-out, two-factor and password-reset pages `forge new` generates to an application that lacks them, and print their wiring; other databases are refused (identity persistence is PostgreSQL-only) |
| `forge deploy [init\|setup\|rollback VERSION\|prune\|details\|lock\|maintenance] [--dry-run] [--json]` | Deploy the image to your servers over SSH with zero-downtime switches through forge-proxy, Kamal-style: see [Deploy](#deploy) |
| `forge logs [-f] [--role R] [--host H] [--since 1h] [--grep REGEX] [--lines N]` | Stream container logs from every host, prefixed `host\|` |
| `forge app exec [--role R] [--host H] [--interactive] [--reuse] CMD...`, `forge app shell [--image IMAGE]` | Run a command or a shell next to the deployed application |
| `forge doctor [--json]` | Load and validate configuration; JSON mode emits versioned, secret-safe diagnostics with remediation codes |
| `forge inspect --json` | Read-only, versioned inventory of commands, migrations, static routes, modules and database adapter |
| `forge routes` | Read-only table of the routes found in the source (including `Resources` routes) and Forge's health routes |
| `forge help --json` | Emit the versioned command catalog with per-command flags, examples, environment inputs, exit codes and side effects |
| `forge uuid [--count N]` | Print N UUIDv7 values (1-1000) |
| `forge version` | Print CLI version, commit, and build date |

`NAME` for `dev` and `build` is needed only if `cmd/` has several commands.
`migrate`, `rollback`, `setup` and `seed` require exactly one.

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
  `forge new` runs `go get github.com/rocketinsidecorp/forge-go@<cli version or
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

`forge generate resource NAME field:type... (--tenant-scoped | --global)
[--plural PLURAL] [--dry-run]` generates a working, tested CRUD resource for
the application's database (read from `FORGE_DATABASE_URL`, as `forge inspect`
does). `NAME` is singular snake_case; the table and routes use the plural
(`category` becomes `categories`; pass `--plural` for irregular words).
Exactly one scope flag is required. A field is `name:type` with `type` one of
`string` (up to 255 characters), `text`, `int` (64-bit), `bool`, `time` (UTC)
or `uuid`; a trailing `?` makes it optional (`NULL`). Quote optional fields in
shells that expand `?`. There is no floating-point or decimal type: store money
as an `int` in minor units.

```sh
forge generate resource product name:string 'summary:text?' price_cents:int --tenant-scoped --dry-run
forge generate resource product name:string 'summary:text?' price_cents:int --tenant-scoped
forge migrate
```

It writes `db/migrations/<timestamp>_create_products.{up,down}.sql` and, for
each of `app/domain`, `app/application`, `app/adapters/persistence` and
`app/adapters/httpapi`, `product.go` with its `product_test.go`. Identifiers are
UUIDv7 (`uuid` on PostgreSQL, `CHAR(36)` on MySQL and MariaDB, `TEXT` in a
`STRICT` table on SQLite). Tenant-scoped resources read the tenant from the
context, bind `tenant_id` in every statement and, on PostgreSQL, run behind
forced row-level security; MySQL, MariaDB and SQLite rely on the statement
predicates alone. Updates and deletes need the version the client read (`409`
when stale), lists are newest-first keyset pages, and `PATCH` changes only the
fields it names (`null` clears an optional one).

The generator refuses to overwrite any file and never edits
`app/bootstrap/bootstrap.go`: it prints the imports and wiring to add.
`RegisterProducts` needs a `protect` function and asks it for `products:read` or
`products:write` per route, so the endpoints are never public by accident. On
PostgreSQL (Forge identity) the migration adds those permissions to
`forge_permissions` and the wiring uses `auth.RequireAudited`; a global resource's
write routes additionally stay behind a deny-all `protectPlatformWrite` you replace
with operator-level authorization (tenant roles must not be able to change rows every
tenant reads); on other databases the
printed guard denies every request until you replace it with your own
authentication and authorization. The generated integration tests need the test
database (`FORGE_TEST_DATABASE_URL`, set by `.env.development`) and fail instead
of skipping when `FORGE_TEST_REQUIRE_DATABASE=true`.

`forge generate scaffold NAME field:type... (--tenant-scoped | --global)
[--plural PLURAL] [--broadcast] [--dry-run]` is `forge generate resource` plus the HTML
pages, the equivalent of `rails g scaffold`. It takes exactly the same
arguments, writes exactly the same resource files, and adds:

- `app/adapters/htmlui/<name>.go` and `<name>_test.go`: thin handlers for
  index, new, create, show, edit, update and destroy, and
  `Register<Plural>(app, service, renderer, protect)`;
- `app/views/<plural>/{index,show,new,edit,_form,not_found}.gohtml`;
- the views base (`app/views/views.go`, `layouts/application.gohtml`,
  `shared/_flash.gohtml`) when the application lacks it.

The pages live at `/<plural>` (the JSON API stays at `/v1/<plural>`). Forms
carry the CSRF token and use `_method` for `PATCH` and `DELETE`; invalid input
re-renders the form with `422` and a message beside each field, success sets a
flash notice and answers `303 See Other`, a missing record shows a `404` page,
and an edit based on a stale version shows the form again with `409` instead
of overwriting the other change. The pages use the same guard as the JSON
routes (`<plural>:read` for index and show, `<plural>:write` for forms and
changes); with Forge identity the printed wiring authenticates the session
cookie rather than a bearer token and sends signed-out browsers to the
sign-in page. On PostgreSQL the migration also grants the new permissions to
the `owner` role `forge setup` creates. See [docs/VIEWS.md](docs/VIEWS.md#scaffold-a-crud).

### Hotwire: JavaScript, Turbo and live updates

Generated applications are full-stack like Rails 8, without Node. Static files
are embedded and served with digested, immutable URLs; Turbo and Stimulus are
vendored by Forge (pinned by SHA-256, licences retained) and loaded through an
import map that the strict Content-Security-Policy allows by hash. Forms and
links work under Turbo Drive with no changes.

```sh
forge new myapp && cd myapp
cat app/javascript/application.js                       # import "@hotwired/turbo"; import "controllers"
cat app/javascript/controllers/hello_controller.js      # a Stimulus controller
curl -s localhost:8080/session/new | grep -E 'importmap|modulepreload|stylesheet'   # with forge dev running (PostgreSQL)

# add a Stimulus controller
cp app/javascript/controllers/hello_controller.js app/javascript/controllers/clipboard_controller.js
#   in app/javascript/controllers/index.js:
#     import ClipboardController from "controllers/clipboard_controller"
#     application.register("clipboard", ClipboardController)
#   in a template: <div data-controller="clipboard"> ... </div>
```

Real-time updates: Turbo Streams pushed to every open page over
Server-Sent Events, with signed, tenant-scoped channel names, and PostgreSQL
`LISTEN/NOTIFY` to reach every replica:

```sh
export FORGE_STREAMS_KEY=$(openssl rand -base64 32)     # required in staging and production
forge generate scaffold post title:string body:text --tenant-scoped --broadcast
forge migrate
forge test                                              # includes an end-to-end event stream test on PostgreSQL
```

Paste the printed wiring into `app/bootstrap/bootstrap.go` (it defines the
stream signer, mounts `GET /streams` behind the session cookie and the
`posts:read` permission, and passes the broker to the pages). Open `/posts` in
two browsers as users of the same organization: creating, editing or deleting a
post in one updates the other without a reload. In your own code:

```go
channel, _ := broadcast.ForTenant(tenant.ID, "posts")
row, _ := renderer.Fragment(nil, "posts/_item", data)      // escaped partial, no request-bound content
stream, _ := turbo.Prepend("posts", row)
_ = broadcast.Publish(ctx, broker, channel, stream)         // and {{turboStreamFrom "posts"}} in the page
```

Details, the multi-replica setup and the delivery guarantees (best effort, at
most once) are in [docs/VIEWS.md](docs/VIEWS.md#assets-javascript-and-hotwire)
and [ADR 0029](docs/adr/0029-hotwire-assets-and-live-updates.md).

### Deploy

`forge deploy` ships the application image to servers that have only SSH and
Docker ([DEPLOYMENT.md](docs/DEPLOYMENT.md#forge-deploy),
[ADR 0030](docs/adr/0030-forge-deploy.md)). Build the proxy image once from
this repository and reference it by digest in `proxy.image`:

```sh
docker build -f cmd/forge-proxy/Dockerfile -t registry.example.com/acme/forge-proxy:0.1.0 .
docker push registry.example.com/acme/forge-proxy:0.1.0
```

Then, in the application:

```sh
forge deploy init                      # config/deploy.yml + .forge/secrets (0600, git-ignored)
$EDITOR config/deploy.yml .forge/secrets
forge deploy setup --dry-run           # every remote command, secrets redacted
forge deploy setup                     # docker check, network, forge-proxy, accessories
forge deploy                           # build, push, migrate, health-gated switch on every host
forge deploy details                   # versions per host and role, proxy target, lock
forge logs -f --grep ERROR             # host|line from every web host
forge app exec migrate status          # one-off container of the deployed image
forge app shell --image busybox:1.37   # debug sidecar (distroless has no shell)
forge deploy rollback 3f2a9c1d07be     # back to a retained version
forge deploy prune                     # drop stopped versions beyond retain
```

A deploy migrates before any switch, keeps the previous containers running
until every host passed its health gate, and routes all hosts back if one
fails. Exit codes: `3` lock held, `4` unhealthy (reverted), `5` migration
failed, `6` hook failed, `7` not fully reverted.

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
| `seed` | Added by `<adapter>.SeedCommand(db.Seed)`: the application's idempotent seed data in one transaction |
| `setup --tenant SLUG --email EMAIL [--organization NAME] [--password-stdin]` | Added by `authpostgres.SetupCommand()` (PostgreSQL): the first tenant and owner, once |

Exit status is `0` on success, `1` on failure (including invalid
configuration), and `2` for an unknown command or invalid arguments.

For agent and script integrations, prefix any Forge CLI command with
`--error-format=json` to emit a versioned, secret-safe error object on stderr
when the command fails. Error messages intentionally omit underlying error
details and input values; `doctor --json` and `inspect --json` keep their
command-specific diagnostic reports on stdout.

## Rate limiting, cache and file storage

Rails 8's `rate_limit`, Solid Cache and Active Storage, on the standard-library
core. Details: [RATE_LIMITING](docs/RATE_LIMITING.md), [CACHING](docs/CACHING.md),
[STORAGE](docs/STORAGE.md).

Configure (all optional; invalid values stop the process at startup):

```sh
export FORGE_RATE_LIMIT_MAX_KEYS=100000        # in-memory limiter buckets
export FORGE_CACHE_MAX_ENTRIES=10000 FORGE_CACHE_DEFAULT_TTL=1h
export FORGE_STORAGE_ROOT=/var/lib/app/storage # enables disk storage
export FORGE_STORAGE_SIGNING_KEY=$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')  # required in production
```

Rate limit one route (`rate_limit to: 10, within: 3.minutes, only: :create`);
behind a proxy key by something authenticated, not the client address:

```go
limiter, _ := ratelimit.NewMemory(ratelimit.MemoryOptionsFromConfig(app.Config().RateLimit))
create, err := ratelimit.Middleware(limiter, ratelimit.ByRemoteAddr, ratelimit.Limit{N: 10, Per: 3 * time.Minute})
err = r.Handle("POST /comments", create(http.HandlerFunc(h.Create))) // 429 + Retry-After over the limit
```

Cache (memory, or shared in the database; stampede-protected `Fetch`):

```go
c, _ := cache.NewMemory(cache.MemoryOptionsFromConfig(cfg.Cache))
price, err := cache.FetchJSON(ctx, c, cache.Key("price", sku), 10*time.Minute, loadPrice)
```

Shared stores for several replicas (PostgreSQL shown; `mysql` and `sqlite` work
the same). Compose their migrations, then run `app migrate`:

```go
forge.Main(bootstrap.Configure, postgres.CommandsFromSets(db.Migrations(),
    ratelimitpostgres.Migrations(), cachepostgres.Migrations())...)
limiter, _ := ratelimitpostgres.NewLimiter(db, ratelimit.StoreOptions{})
c, _ := cachepostgres.NewStore(db, cache.StoreOptions{})
```

```sh
app migrate   # creates forge_rate_limits and forge_cache_entries
```

File storage: uploads with size and type limits, signed expiring links, safe
serving (only raster images inline, everything else a download):

```go
store, _ := disk.NewFromConfig(cfg.Storage)
signer, _ := storage.NewSignerFromConfig(cfg.Storage)
files, _ := storage.NewHandler(store, signer, storage.HandlerOptions{})
_ = r.Handle("GET /files/{key...}", files)

got, err := storage.Receive(w, r, store, storage.UploadOptions{
    Field: "avatar", AllowedTypes: []string{"image/png", "image/jpeg"},
    MaxFileBytes: cfg.Storage.EffectiveMaxUploadBytes(), Prefix: "avatars"})
url, _ := signer.URL("/files", got.Files[0].Key, storage.SignOptions{Inline: true})
```

There is no S3 adapter yet (see the [roadmap](docs/ROADMAP.md)). The upload limit
cannot exceed `FORGE_HTTP_MAX_BODY_BYTES`.
## Observability

Telemetry is on by default and exports nothing: logs carry `trace_id`, and
incoming `traceparent` headers are ignored unless you opt in with
`FORGE_TELEMETRY_TRUST_TRACEPARENT=true` behind a proxy that sanitizes them. Pick an export path with
environment variables:

```bash
# Prometheus metrics on a separate, private listener
FORGE_TELEMETRY_METRICS_ADDR=127.0.0.1:9464 forge dev
curl -s http://127.0.0.1:9464/metrics | grep http_server_request_duration_seconds

# Traces and metrics to an OpenTelemetry Collector over OTLP/HTTP
docker run --rm -p 4318:4318 -v "$PWD/otel-collector.yaml:/etc/otelcol/config.yaml:ro" otel/opentelemetry-collector:0.111.0
FORGE_TELEMETRY_EXPORTER=otlp FORGE_TELEMETRY_OTLP_ENDPOINT=http://127.0.0.1:4318 \
  FORGE_TELEMETRY_SERVICE_NAME=myapp forge dev

# In a container: bind to all interfaces and confirm it (never publish the port)
FORGE_TELEMETRY_METRICS_ADDR=0.0.0.0:9464 FORGE_TELEMETRY_METRICS_PUBLIC=true forge dev

# Spans in the development log, or no telemetry at all
FORGE_TELEMETRY_EXPORTER=log forge dev
FORGE_TELEMETRY_ENABLED=false forge dev
```

Pass `app.Telemetry()` to adapters to trace database statements and jobs:
`postgres.Open(ctx, cfg, postgres.WithTelemetry(app.Telemetry()))` (generated
applications already do). Collector and Prometheus configuration, the
metric catalogue, cardinality rules and what is never recorded:
[docs/OBSERVABILITY.md](docs/OBSERVABILITY.md).

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
- [Server-rendered HTML](docs/VIEWS.md)
- [Rate limiting](docs/RATE_LIMITING.md), [caching](docs/CACHING.md) and [file storage](docs/STORAGE.md)
- [Deployment](docs/DEPLOYMENT.md)
- [Observability](docs/OBSERVABILITY.md)
- [Roadmap](docs/ROADMAP.md)
- [Rails parity (Forge-Go and Forge-Rust)](docs/RAILS_PARITY.md)
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
  - [0030 `forge deploy` and forge-proxy](docs/adr/0030-forge-deploy.md)
  - [0031 Rate limiting](docs/adr/0031-rate-limiting.md)
  - [0032 Application cache](docs/adr/0032-cache.md)
  - [0033 File storage](docs/adr/0033-file-storage.md)
