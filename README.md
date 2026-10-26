<p align="center">
  <img src="internal/scaffold/templates/assets/forge-go-logo.png" alt="Forge Go — Go Framework" width="220">
</p>

# Forge

Forge is a Go framework for building HTTP services on PostgreSQL, MySQL,
MariaDB, or SQLite with secure defaults, a conventional application layout,
and a production container from the first commit. The core packages use only
the Go standard library; each database adapter adds its driver.

New to Forge? [Getting started](docs/GETTING_STARTED.md) builds a small blog
from `forge new` to deploy: scaffolds, sign-in, live updates, translations,
email, jobs, OpenAPI and tests.

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
([docs/OBSERVABILITY.md](docs/OBSERVABILITY.md)). Phase 6 starts with an
OpenAPI 3.1 document generated from the routes' Go types
([docs/OPENAPI.md](docs/OPENAPI.md)). Phase 10 starts with continuous
fuzzing, SBOMs and signed release images
([docs/SUPPLY_CHAIN.md](docs/SUPPLY_CHAIN.md)). Performance work and further
security hardening remain planned. See [docs/ROADMAP.md](docs/ROADMAP.md) for the full status.

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
| `credentials` | Encrypted per-environment credentials (`config/credentials/<env>.json.enc`, AES-256-GCM, key from `FORGE_MASTER_KEY` or a git-ignored key file), typed `Store`, fail-fast required credentials; `forge credentials edit` |
| `encryption` | Attribute encryption with key rotation: randomized AES-256-GCM bound to table, column and record id, key rings, opt-in HMAC blind index, `Value[T]` for `database/sql`, resumable batch re-encryption |
| `router`     | Route registry on `net/http.ServeMux` (Go 1.22 patterns), frozen after the first request; JSON `400`/`404`/`405`; conventional `Resources` routes |
| `openapi`    | OpenAPI 3.1 from described routes: `Describe`, `Typed` handlers, schemas reflected from Go types (UUIDv7, `date-time`, nullable, `$ref` components), deterministic JSON, contract validation of responses |
| `httpserver` | HTTP server with timeouts, body limit, security headers, request IDs, panic recovery, graceful shutdown, and composable application middleware |
| `web`        | JSON responses, the single error format, strict JSON and form decoding, safe redirects, content negotiation, flash messages, method override, request ID and request-scoped logger |
| `csrf`       | CSRF protection for browser requests: double-submit token in forms and headers, cross-origin rejection |
| `views`      | Concurrent-safe server-rendered HTML with layouts, partials, CSRF, flash and translation helpers, contextual escaping and buffered rendering |
| `i18n`       | Rails I18n: JSON translations in `config/locales`, `%{name}` interpolation, CLDR plural rules, per-request locale (path, parameter, cookie, `Accept-Language`), localized numbers and dates, `forge i18n check` |
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

Requires Go 1.27.3+ and Docker with BuildKit and Compose.

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
forge db prepare                # create the database if missing, then migrate (and seed a new one)
forge dev                       # rebuilds, restarts and reloads the browser when files change
```

With SQLite, `forge db prepare` and `forge dev` use `storage/development.db`
directly.

### Database roles (PostgreSQL)

A PostgreSQL application serves as a runtime role that row-level security
applies to and that cannot change the schema, and migrates as a separate role
that owns it. `docker/postgres/init.sql` creates both in the Compose database
(`app_runtime` and `app_migrator`, development-only passwords), and
`.env.development` points `FORGE_DATABASE_URL` and
`FORGE_MIGRATION_DATABASE_URL` at them:

```sh
docker compose up -d postgres     # first start: init.sql creates app_migrator, app_runtime and the database
forge migrate                     # runs as app_migrator (FORGE_MIGRATION_DATABASE_URL)
forge dev                         # serves as app_runtime (FORGE_DATABASE_URL)
forge test                        # repository and sign-in tests run as a fresh runtime role
docker compose down --volumes     # once, for a volume created before the roles existed
```

In production, provision the same two roles once (SQL in
[Database roles](docs/DATABASE.md#database-roles-postgresql)), migrate with
the migration role and serve with the runtime role only. In staging and
production the application refuses to start when the role it logs in as, or
acts as, is a superuser or has `BYPASSRLS` or `CREATEROLE`, or when the URL
switches it to another role.

```sh
docker run --rm -e FORGE_ENV=production \
  -e FORGE_DATABASE_URL="$RUNTIME_DATABASE_URL" \
  -e FORGE_MIGRATION_DATABASE_URL="$MIGRATION_DATABASE_URL" myapp migrate
docker run -e FORGE_ENV=production -e FORGE_DATABASE_URL="$RUNTIME_DATABASE_URL" myapp
```

With `forge deploy`, name the migration credential under `migrate.env` in
`config/deploy.yml` and keep it out of `env`: only the one-off migrate
container receives it ([DEPLOYMENT.md](docs/DEPLOYMENT.md#database-roles)):

```yaml
env:
  secret: [FORGE_DATABASE_URL]                    # every container
migrate:
  env:
    secret: [FORGE_MIGRATION_DATABASE_URL]        # the migrate step only
```

```sh
# .forge/secrets: FORGE_MIGRATION_DATABASE_URL=$FORGE_MIGRATION_DATABASE_URL
export FORGE_DATABASE_URL="$RUNTIME_DATABASE_URL" FORGE_MIGRATION_DATABASE_URL="$MIGRATION_DATABASE_URL"
forge deploy --dry-run     # prints the upload, run and rm commands for the migrate env file; values stay hidden, nothing runs
forge deploy
```

### Development loop

```sh
forge dev                        # http://127.0.0.1:8080, reloads on save
forge dev --poll-interval 1s     # scan less often (large trees, slow disks)
forge dev --no-reload            # build and run once, no watching

forge db setup                   # create + migrate + seed
forge db prepare                 # setup if the database is missing, else migrate (safe to repeat)
forge db reset                   # drop + setup: a clean development database
forge db status                  # applied / pending migrations
forge db rollback -steps 1       # your newest migration; never a Forge framework table
forge db schema:dump             # write db/schema.sql (pg_dump, mysqldump or sqlite3 on PATH)
forge db schema:load             # load db/schema.sql into an existing, empty database (CI, tests)
forge db console                 # psql, mysql or sqlite3 on the application's database
forge db console -- -c 'select count(*) from forge_schema_migrations'
```

`forge dev` watches Go sources, templates, assets, migrations and
`.env.development`/`.env.local`. The old process keeps serving until the new
build succeeds, then gets `SIGTERM` and is replaced; build errors are printed
and shown over the page, and the running app is not stopped. With
`FORGE_ENV=development` (the default under `forge dev`) the forge process
listens on `FORGE_HTTP_ADDR` as a development-only proxy, runs the application
on a private loopback port and injects the reload script into HTML pages; the
page's Content-Security-Policy gets a per-response nonce only if it would block
that one script. Nothing of this is in your application binary
([ADR 0035](docs/adr/0035-development-loop.md), [Database](docs/DATABASE.md#database-lifecycle-forge-db)).

`drop`, `reset`, `schema:load` and `rollback` are refused unless the database is
local and `FORGE_ENV` is `development` or `test`: in production and staging, and
for any server that is not on this machine, you must pass `--force-production`
and either type the database name at the prompt or pass `--yes` with
`FORGE_ALLOW_DESTRUCTIVE=true` in the process environment:

```sh
FORGE_ALLOW_DESTRUCTIVE=true forge db drop --force-production --yes
```

`FORGE_MIGRATION_DATABASE_URL` names the migration role (on PostgreSQL, the
schema owner) for `create`, `drop`, `schema:*`, `console`, `migrate` and
`rollback`; the database itself always comes from `FORGE_DATABASE_URL`.

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
| `/mfa` | two-factor settings of the signed-in account: turn on, replace the authenticator, new backup codes, turn off |
| `/passwords/new` | forgot password (only when reset emails are configured, below) |
| `/passwords/edit?token=...` | choose a new password from the emailed link |

Two-factor authentication: `forge new` puts a development
`FORGE_AUTH_MFA_KEY` in `.env.local`, so signed in, open
`http://localhost:8080/mfa`, enter your password, add the key to an
authenticator app (the `otpauth://` link or the Base32 text) and confirm with
the password and its 6-digit code; the page shows 10 backup codes once and
signs you out everywhere. Every later change asks for the password and a
current code (or a backup code), throttled and audited. For production,
create a key with
`openssl rand -base64 32 | tr '+/' '-_' | tr -d '='` and keep it in a secret
manager ([docs/MFA.md](docs/MFA.md), [ADR 0045](docs/adr/0045-generated-mfa-settings-pages.md)).

Release note: the MFA service and store contracts changed (a step-up proof
bound to the account is required by every change of an existing factor, and
a step-up backup code is used up with the change); custom stores and direct
callers port as [docs/MFA.md](docs/MFA.md#upgrading) describes.

Release note: pages now embed the CSRF token masked with a fresh pad on every
render (BREACH), so it no longer equals the `__Host-forge_csrf` cookie. Two
tests of applications generated earlier compare them byte for byte and fail:
`TestLayoutRendersFlashAndCSRF` (`app/views/views_test.go`) and
`TestBrowserPipelineRequiresCSRFToken` (`app/bootstrap/bootstrap_test.go`).
Replace the comparison with `csrf.Match(cookie, embedded)`, as
[docs/VIEWS.md](docs/VIEWS.md#upgrading-tests-that-compare-the-embedded-token-with-the-cookie)
shows:

```go
if !csrf.Match(cookies[0].Value, form.Body.String()) { // was: cookies[0].Value != form.Body.String()
	t.Fatalf("form body %q does not carry the CSRF cookie's token", form.Body.String())
}
```

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

Logins are ASCII email addresses (`setup` refuses others) and the reset page
answers at once, no sooner than 100 ms: the email is issued and sent a moment
later by a bounded queue the application owns. After upgrading an existing
application, run `forge migrate` (the login throttle gains a counter) and list
accounts whose stored login is not ASCII, which can no longer sign in
([docs/AUTHENTICATION.md](docs/AUTHENTICATION.md#canonical-logins)):

```sh
forge migrate
psql "$FORGE_DATABASE_URL" -c "SELECT id, email FROM forge_users WHERE email_normalized !~ '^[!-~]+\$' OR email_normalized <> lower(email_normalized)"
```

Sessions end after two hours without a request (and at the latest 24 hours
after sign-in). Set another idle limit, from `5m` to `720h`, with
`FORGE_AUTH_SESSION_IDLE_TIMEOUT`. Existing applications run `forge migrate`
first: sessions gain their last use, and those open during the upgrade count
as used then ([docs/AUTHENTICATION.md](docs/AUTHENTICATION.md#session-idle-timeout)):

```sh
forge migrate
echo 'FORGE_AUTH_SESSION_IDLE_TIMEOUT=30m' >> .env.local
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

These pages are the only way to sign in to a new application. The JSON
authentication APIs (bearer `POST /auth/login` and `/auth/logout`, the cookie
login for browser scripts, MFA completion, JSON password recovery and
`GET /v1/auth/session`) are opt-in, since each is another place a password is
entered:

```sh
forge new myapi --api-auth      # the pages plus the JSON authentication APIs
curl -s -X POST http://127.0.0.1:8080/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"your password","tenant":"acme"}'
```

An application created without the flag adds them later: in one that has
the pages, `--api-auth` writes nothing and prints only the JSON API wiring to
paste into `identityRoutes`:

```sh
forge generate authentication --api-auth   # the pages exist: prints the JSON API wiring only
```

Applications generated earlier mount the JSON routes; remove those no API
client uses (`forge generate authentication` lists them,
[docs/AUTHENTICATION.md](docs/AUTHENTICATION.md#json-authentication-apis-opt-in)).
An application created before the sign-in pages existed gets them with:

```sh
forge generate authentication --dry-run   # preview files and the wiring to paste
forge generate authentication             # app/adapters/htmlauth, app/views/{sessions,passwords}
forge generate authentication --api-auth  # the same, and the wiring of the JSON authentication APIs
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

### Generators

Every generator refuses to overwrite a file, writes nothing when it refuses,
takes `--dry-run`, never edits `app/bootstrap/bootstrap.go` (it prints the
lines to paste) and never makes anything public by accident. Fields are
`name:type` with `type` one of `string`, `text`, `int`, `bool`, `time`, `uuid`
(a trailing `?` makes one optional; quote it in shells that expand `?`). The
full reference is [docs/GENERATORS.md](docs/GENERATORS.md).

```sh
# Model: migration, domain entity, repository port and use cases, persistence adapter, tests; no HTTP
forge generate model post title:string 'body:text?' published:bool --tenant-scoped --dry-run
forge generate model post title:string 'body:text?' published:bool --tenant-scoped
forge migrate && forge test

# Controller: handlers, routes, views and tests for read-only actions, no persistence
forge generate controller pages home about_us --public     # GET /pages/home, GET /pages/about_us
forge generate controller reports monthly                  # guarded: needs the reports:read permission
forge generate controller status_checks index --api        # JSON handlers under /v1
forge routes

# Mailer: typed methods, text and HTML templates, browser previews, tests
forge generate mailer user welcome:name,url password_reset:name,reset_url
FORGE_MAIL_URL=file:tmp/mails FORGE_MAIL_FROM='App <app@example.com>' FORGE_MAIL_PREVIEW=true forge dev
#   http://localhost:8080/_mail/user/   (after pasting the printed wiring)

# Job: a typed handler for the persistent queue with its enqueue function and tests
forge generate job send_welcome user_id:uuid --queue
forge generate job cleanup                                 # the recurring in-process stub, unchanged

# Everything above, then the same arguments with destroy to take it away again
forge destroy controller pages home about_us --dry-run
forge destroy controller pages home about_us
forge destroy model post title:string 'body:text?' published:bool --tenant-scoped --migrations
```

`forge destroy GENERATOR ARGUMENTS...` (the arguments the generator was run
with) removes exactly the files the generator creates, and only while each is
unchanged: a file you edited is listed, nothing is removed, and `--force`
overrides. The same goes for files a bigger generation still needs: a model
inside a resource or scaffold, a view a controller renders. Migrations are kept unless `--migrations` (run `forge rollback`
first if they ran), shared files such as the views base are never removed, and
the wiring to take out of `bootstrap.go` is printed.

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
  compose.observability.yaml   # opt-in overlay: collector, Prometheus, Tempo, Loki, Grafana
  observability/               # their configuration, Grafana data sources and dashboard
  .gitignore                   # ignores .env, .env.local, .env.*.local (and /storage/ for SQLite)
  README.md
  .github/workflows/ci.yml     # gofmt, vet, test -race (with a database service), govulncheck, docker build
  .github/dependabot.yml       # weekly Go module, Actions, Docker and Compose updates
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

## Translations (i18n)

Rails I18n with JSON files: every generated page reads its text from
`config/locales` (`en.json`, `pt-BR.json`, `posts.en.json`...), each request
gets a locale (the `locale` cookie, `Accept-Language`, then the default), and
`forge i18n check` lists what each locale lacks.

```sh
forge new shop --locale pt-BR                       # en.json + pt-BR.json; pt-BR is the default
cd shop && forge dev
curl -H 'Accept-Language: en' http://127.0.0.1:8080/        # Welcome to shop
curl -H 'Accept-Language: pt-BR' http://127.0.0.1:8080/     # Bem-vindo ao shop
forge generate scaffold post title:string --global  # adds config/locales/posts.en.json and posts.pt-BR.json (English text)
forge i18n check                                    # nothing missing: translate posts.pt-BR.json (values only; keep the "pt-BR" key)
FORGE_I18N_DEFAULT_LOCALE=en forge dev              # change the default without a rebuild
```

To let the URL choose the locale, set `PathPrefix` where
`useBrowserPipeline` (`app/bootstrap/bootstrap.go`) builds the middleware:
`translator.Middleware(i18n.NegotiationOptions{PathPrefix: true})`.

```sh
curl http://127.0.0.1:8080/en/      # Welcome to shop: GET / in English, whatever Accept-Language says
curl http://127.0.0.1:8080/pt-BR/   # Bem-vindo ao shop
```

The locale segment is removed before any application middleware runs
(request IDs, telemetry and the panic log see the original path), so a path
guard sees `/admin` for `/pt-BR/admin` wherever it is registered (`App.Use`
or `forge.WithHTTPMiddleware`, before or after the i18n middleware).

In templates: `{{t "posts.index.title"}}`, `{{t "users.inbox" "count" .Unread}}`
(CLDR plural forms), `{{l .CreatedAt "short"}}`, `{{number .Total 2}}`,
`<html lang="{{locale}}">`; in Go: `i18n.T(r.Context(), "posts.flash.created")`.
Keys ending in `_html` are trusted markup with escaped arguments; everything
else is escaped by `html/template`. Missing translations show as
`translation missing: LOCALE.KEY` in development and test and fall back to the
default locale in production. See [docs/I18N.md](docs/I18N.md) and
[ADR 0042](docs/adr/0042-internationalization.md).

## Credentials and attribute encryption

Rails' `credentials:edit` and Active Record Encryption, with the sharp edges
removed. `forge new` already created `config/credentials/development.json.enc`
(committed) and `development.key` (git-ignored, mode 0600).

```sh
forge credentials edit                                   # edit development secrets in $EDITOR
forge credentials show                                   # structure only; values redacted
forge credentials init --environment production          # new key + encrypted file
forge credentials edit --environment production
forge credentials show --environment production --reveal # needs an interactive terminal
git add config/credentials/production.json.enc           # the key file is ignored

# deploy: the server needs only the key
docker run --rm -p 8080:8080 -e FORGE_ENV=production \
  -e FORGE_MASTER_KEY="$(cat config/credentials/production.key)" myapp

# rotate the key, keeping the old one readable while deployments catch up
forge credentials rotate-key --environment production --keep-old --print-key
```

Read secrets in the composition root, failing at startup if one is missing:

```go
creds, err := credentials.Open(string(cfg.Environment), credentials.Options{
	Required: []string{"stripe.api_key"}, // enforced outside development and test
})
stripeKey, err := creds.Secret("stripe", "api_key") // config.Secret: prints as [REDACTED]
```

Encrypt a column (the ciphertext is bound to table, column and row, so it cannot
be moved to another row; `ssn` is a `TEXT` column):

```go
ring, _ := creds.KeyRing("encryption")        // created by `init`
cipher, _ := encryption.NewCipher(ring)
ssn := encryption.NewValue[string](cipher, "people", "ssn").ForRecord(id.String())
ssn.Set("123-45-6789")
_, err = db.Exec(ctx, `INSERT INTO people (id, ssn) VALUES ($1, $2)`, id, ssn)
```

Key rotation, blind indexes for exact-match search and batch re-encryption are
in [docs/ENCRYPTION.md](docs/ENCRYPTION.md); the credentials workflow, CI and
rotation are in [docs/CREDENTIALS.md](docs/CREDENTIALS.md).

To rotate a data key, make the new key active, re-encrypt each column and
remove the old key only when a count of rows not on the active key is 0
([docs/ENCRYPTION.md](docs/ENCRYPTION.md#rotating-keys)). PostgreSQL tenant
tables (row-level security) rotate once per tenant; an unscoped run on them is
refused:

```go
rewriter, err := postgres.EncryptedColumn(db, encryption.SQLColumn{Table: "people", Column: "ssn"},
	postgres.WithTenant(tenant))
if err != nil {
	return err
}
report, err := cipher.Reencrypt(ctx, rewriter, encryption.RotateOptions{})
```

```sql
-- as a role that sees every row (superuser or BYPASSRLS); stale must be 0
SELECT count(CASE WHEN ssn IS NOT NULL
                   AND substr(ssn, 1, length('fg1.2026-10.')) <> 'fg1.2026-10.' THEN 1 END) AS stale,
       count(*) AS total
  FROM people;
```

## OpenAPI

JSON routes declare their request, response and error types next to the
route, and Forge builds an OpenAPI 3.1 document from the Go types: accurate
by construction for `openapi.Typed` handlers, checked by contract tests for
the rest. Generated applications, resources and `--api` controllers are
described out of the box, and so are the `auth` package's JSON routes
(`/auth/login`, logout, MFA completion, password recovery and the cookie
routes) in an application that registers them (`forge new --api-auth`).

```sh
forge new shop && cd shop && forge dev
curl http://127.0.0.1:8080/openapi.json             # served in development and test
curl -s http://127.0.0.1:8080/openapi.json | grep '"/auth/'   # --api-auth apps: the sign-in, recovery and cookie routes
forge openapi                                       # print it (runs Configure, never listens)
forge openapi --output openapi.json                 # write it to commit and diff
forge openapi --check openapi.json                  # exit 1 when the committed file is stale
FORGE_OPENAPI_ENABLED=false forge dev               # off; staging and production serve it only with =true
```

```go
app.Handle("GET /v1/status", openapi.Typed(http.StatusOK,
	openapi.Operation{Summary: "Service status", Tags: []string{"status"}},
	func(*http.Request, openapi.NoBody) (statusResponse, error) { return statusResponse{Status: "ok"}, nil }))

app.Handle("POST /v1/posts", openapi.Describe(create, openapi.Operation{
	Summary: "Create a post", Permission: "posts:write",
	Request:   openapi.TypeOf[createPost](),
	Responses: []openapi.Response{openapi.JSON[post](http.StatusCreated)},
	Errors:    []int{http.StatusConflict},
}))
```

In tests, `spec.CheckResponses(app.Handler(), report)` validates every
response against the document. Details, type mapping and tags:
[docs/OPENAPI.md](docs/OPENAPI.md), [ADR 0043](docs/adr/0043-openapi-from-typed-routes.md).

## CLI

| Command | Description |
| ------- | ----------- |
| `forge new NAME [--database DB] [--locale LOCALE] [--api-auth] [--module PATH] [--dir DIR] [--framework-path DIR] [--skip-deps] [--skip-database]` | Create an application. Refuses to write into a non-empty directory. |
| `forge dev [NAME] [--no-reload] [--poll-interval D] [--debounce D]` | Build and run `./cmd/<name>`; on a change to Go sources, templates, assets, migrations or `.env.*` files rebuild, restart gracefully and reload the browser (development only). A failed build leaves the running app alone. `--no-reload` builds and runs once |
| `forge test [ARGS]` | `go test` with the development environment loaded; `ARGS` default to `./...` |
| `forge build [NAME]` | `go build -trimpath` into `bin/<name>` |
| `forge db COMMAND` | Database lifecycle like Rails' `db:*`: `create`, `drop`, `reset`, `setup`, `prepare`, `migrate`, `rollback`, `status`, `seed`, `schema:dump`, `schema:load`, `console`; see [Development loop](#development-loop) |
| `forge migrate [status]` | Build the application and run its `migrate` (or `migrate status`) command (same as `forge db migrate`) |
| `forge rollback [-steps N]` | Build the application and run its `rollback` command (default 1 step): only the application's own migrations, never Forge's framework migrations |
| `forge setup --tenant SLUG --email EMAIL [--organization NAME] [--password-stdin]` | PostgreSQL: create the first organization, tenant and owner account; the password is prompted for (or piped, or `FORGE_SETUP_PASSWORD`), never an argument; refuses once a tenant exists |
| `forge seed` | Run the application's `seed` command: `db.Seed` in one transaction |
| `forge generate migration NAME [--dry-run]` | Create or preview `db/migrations/<UTC timestamp>_NAME.{up,down}.sql` |
| `forge generate view [DIR/]NAME [--dry-run]` | Create or preview a server-rendered page, `app/views/DIR/NAME.gohtml` (DIR defaults to `pages`), in the layout `forge new` generates |
| `forge generate job NAME [--dry-run]` | Create or preview a cancellation-aware recurring job stub and tests; registration and schedule remain explicit |
| `forge generate model NAME field:type... (--tenant-scoped \| --global) [--plural P] [--dry-run]` | Create or preview a model: migration, domain, application (repository port, use cases) and persistence layers with tests; the model and persistence files of `generate resource`, without the HTTP layer |
| `forge generate controller NAME ACTION... [--api] [--public] [--dry-run]` | Create or preview HTTP handlers for read-only actions, their routes, views (HTML) and tests, without persistence; `--api` generates JSON handlers; guarded unless `--public` |
| `forge generate mailer NAME METHOD[:field,field]... [--dry-run]` | Create or preview a mailer: typed methods, text and HTML templates, previews and tests, on the `mail` package |
| `forge generate job NAME [field:type...] --queue [--dry-run]` | Create or preview a typed handler for the persistent job queue, its enqueue function and tests |
| `forge generate resource NAME field:type... (--tenant-scoped \| --global) [--plural P] [--dry-run]` | Create or preview a complete resource: migration, domain, application, persistence and HTTP layers with tests, for the application's database. Never public: see below |
| `forge generate scaffold NAME field:type... (--tenant-scoped \| --global) [--plural P] [--broadcast] [--dry-run]` | Everything `generate resource` creates plus server-rendered HTML CRUD pages, forms and their tests; `--broadcast` adds live updates over Server-Sent Events. Never public: see below |
| `forge generate authentication [--api-auth] [--dry-run]` | PostgreSQL: add the sign-in, sign-out, two-factor and password-reset pages `forge new` generates to an application that lacks them, and print their wiring (with `--api-auth`, also the JSON authentication APIs'); other databases are refused (identity persistence is PostgreSQL-only) |
| `forge destroy GENERATOR ARGUMENTS... [--force] [--migrations] [--dry-run]` | Remove what a generator created (`model`, `resource`, `scaffold`, `controller`, `mailer`, `job`, `view`): the same arguments as the generate command; a file that differs from what the generator writes, or that a bigger generation still needs, is never removed without `--force`, migrations are kept without `--migrations` |
| `forge deploy [init\|setup\|rollback VERSION\|prune\|details\|lock\|maintenance] [--dry-run] [--json]` | Deploy the image to your servers over SSH with zero-downtime switches through forge-proxy, Kamal-style: see [Deploy](#deploy) |
| `forge logs [-f] [--role R] [--host H] [--since 1h] [--grep REGEX] [--lines N]` | Stream container logs from every host, prefixed `host\|` |
| `forge app exec [--role R] [--host H] [--interactive] [--reuse] CMD...`, `forge app shell [--image IMAGE]` | Run a command or a shell next to the deployed application |
| `forge credentials init \| edit \| show \| rotate-key [--environment E]` | Encrypted credentials: create a key and skeleton, edit in `$EDITOR` (plaintext shredded on every exit path), show redacted (`--reveal` on a terminal only), rotate the key. See [docs/CREDENTIALS.md](docs/CREDENTIALS.md) |
| `forge i18n check [--json]` | Read-only: list the translations each locale in `config/locales` lacks (keys of the other locales and literal keys used by `t` and `i18n.T`), missing CLDR plural forms and unused keys; exit 1 when anything is missing. See [Translations](#translations-i18n) |
| `forge doctor [--json]` | Load and validate configuration; JSON mode emits versioned, secret-safe diagnostics with remediation codes |
| `forge inspect --json` | Read-only, versioned inventory of commands, migrations, static routes, modules and database adapter |
| `forge routes` | Read-only table of the routes found in the source (including `Resources` routes) and Forge's health routes |
| `forge openapi [--output FILE \| --check FILE]` | Build the application and run its `openapi` command with the development environment: print or write the OpenAPI 3.1 document of the described routes, list undescribed ones on stderr; `--check` exits 1 when FILE is out of date. See [OpenAPI](#openapi) |
| `forge help COMMAND`, `forge COMMAND --help` (or `-h`) | Print one command's usage, flags, examples and notes, and exit 0; for example `forge help new` or `forge generate --help` |
| `forge help --json` | Emit the versioned command catalog with per-command flags, examples, environment inputs, exit codes and side effects |
| `forge uuid [--count N]` | Print N UUIDv7 values (1-1000) |
| `forge version` | Print CLI version, commit, and build date |

`NAME` for `dev` and `build` is needed only if `cmd/` has several commands.
`migrate`, `rollback`, `setup`, `seed` and `db` require exactly one.

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
- `--locale LOCALE` sets the default locale of the generated pages: `en`
  (default), `es` or `pt-BR`. `config/locales/en.json` is always generated,
  plus `LOCALE.json`.
- `--api-auth` also mounts the JSON authentication APIs (bearer and cookie
  login and logout, MFA completion, JSON password recovery,
  `GET /v1/auth/session`) beside the HTML sign-in pages; PostgreSQL only.
  Without it the pages are the only way to sign in.
- `--framework-path DIR` is for framework contributors: it adds a `replace`
  directive pointing at a local checkout, then runs `go mod tidy` and
  `go mod vendor` so the Docker build context is self-contained. In such an
  application (any with a `vendor/` directory) every `forge generate ...` that
  writes files ends with `go mod vendor` as well, so what it generated, which
  may import framework packages `vendor/` did not hold, still builds; a
  `--dry-run` does not. Without it,
  `forge new` runs `go get github.com/rocketinsidecorp/forge-go@<cli version or
  latest>` followed by `go mod tidy`. The CLI version is the release stamped at
  build time or, for a CLI installed from the module proxy with
  `go install github.com/rocketinsidecorp/forge-go/cmd/forge@VERSION`, the
  module (pseudo-)version recorded in its build info (`forge version` reports
  it too). A CLI built from a checkout (`go build`, or `go install` run in it)
  resolves `latest`, even when Go stamps a VCS pseudo-version, because that
  commit may not be published.

`forge generate migration NAME` requires `NAME` to match
`^[a-z][a-z0-9_]{0,99}$` and `db/migrations` to exist. If a migration with the
same second already exists, the version is moved forward one second. It never
overwrites a file.

`forge generate job NAME [--dry-run]` creates `app/jobs/NAME.go` and its test
file. The generated stub returns an explicit not-implemented error until it
is replaced, so registering it cannot silently report successful work. It does
not edit `bootstrap.JobsConfigure`: choose a schedule and register the handler
there explicitly. `NAME` must be lowercase snake_case with each segment
starting with a letter. The generated shutdown hook calls
`scheduler.StopContext(ctx)`, which cancels running jobs and waits for them only
within the shutdown budget (`FORGE_HTTP_SHUTDOWN_TIMEOUT`); a job that ignores
its context keeps running and the hook returns the deadline error. Scheduler
failures are reported but are not retried automatically; jobs that run on
multiple replicas must be idempotent or use a PostgreSQL advisory lock when only
one replica may perform a schedule tick.

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
# Rotating it without breaking open pages: the old key verifies for 24 hours (up to 3 keys, comma-separated)
export FORGE_STREAMS_PREVIOUS_KEYS=$OLD_STREAMS_KEY
forge generate scaffold post title:string body:text --tenant-scoped --broadcast
forge migrate
forge test                                              # includes an end-to-end event stream test on PostgreSQL
```

Release note: the generated `htmlstreams.NewSigner` now takes the previous
keys as a third argument (`FORGE_STREAMS_PREVIOUS_KEYS`). `streams.go` is
written once, so an application generated earlier must update it before the
newly printed wiring compiles: see
[docs/VIEWS.md](docs/VIEWS.md#rotating-the-stream-signing-key).

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
forge deploy lock acquire --message 'db maintenance'   # hold the deploy lock (status | acquire | release [--force])
forge deploy maintenance on            # forge-proxy serves its 503 page; `off` restores the application
forge deploy maintenance off
ssh deploy@host docker exec forge-proxy /usr/local/bin/forge-proxy status   # forge-proxy: current targets per host
```

A deploy migrates before any switch, keeps the previous containers running
until every host passed its health gate, and routes all hosts back if one
fails. Exit codes: `2` invalid arguments (nothing ran), `3` lock held, `4`
unhealthy (reverted), `5` migration failed, `6` hook failed, `7` not fully
reverted.

forge-proxy bounds what one client can make it hold: request headers up to
`-max-header-bytes` (64 KiB; `431` above it) and `-max-connections` open
connections per listener (10000). They are flags of `forge-proxy run`, with
those defaults in every deploy:

```sh
go run ./cmd/forge-proxy run -h   # in a forge-go checkout: lists -max-header-bytes, -max-connections and the rest
```

On PostgreSQL with separate roles, the schema owner's credential goes to the
migrate step alone (`migrate.env`, see
[Database roles](docs/DEPLOYMENT.md#database-roles)):

```sh
$EDITOR config/deploy.yml .forge/secrets   # uncomment migrate.env and the FORGE_MIGRATION_DATABASE_URL line
forge deploy --dry-run                     # prints the migrate env upload, run and rm commands; no value is printed, nothing runs
```

Release note: `retain: 1` in `config/deploy.yml` is now refused (`retain`
counts the running version, so 1 would prune the previous one and leave
nothing to roll back to); use 2 to 100, default 5. The deploy lock also needs
`~/.forge` on the hosts to be on a file system with hard links.

Client addresses: behind forge-proxy the proxied role trusts `X-Forwarded-For`
only from the `forge` Docker network (`forge deploy` sets
`FORGE_HTTP_TRUSTED_PROXIES` to its subnets), so sign-in and reset throttles,
`ratelimit.ByClientIP` and audit count each client, not the proxy. Behind any
other proxy or load balancer, list its networks yourself (empty, the default,
trusts nobody; `/0` and any IPv4 prefix shorter than `/8` or IPv6 prefix
shorter than `/16` are refused):

```sh
export FORGE_HTTP_TRUSTED_PROXIES=10.0.0.0/24,fd00:1::/64   # proxy CIDRs or addresses
```

```yaml
# config/deploy.yml: override the automatic value ("" trusts nobody)
env:
  clear:
    FORGE_HTTP_TRUSTED_PROXIES: "172.18.0.0/16"
```

See [ADR 0037](docs/adr/0037-trusted-proxies-and-client-ip.md).

### Development environment files

`forge dev`, `forge test`, `forge db`, `forge migrate`, `forge rollback`, and
`forge doctor` read `.env.development` and then `.env.local` from the current
directory; a later file overrides an earlier one, and variables already set in
the process environment always win. `dev`, `test`, `db`, `migrate`, and `rollback`
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
| `openapi [--output FILE \| --check FILE]` | Run `Configure` without listening and print or write the OpenAPI document (what `forge openapi` runs) |
| `help` | List the available commands |
| `migrate [status]`, `rollback [-steps N]` | Added by `<adapter>.Commands` (generated applications with a database); `rollback` reverts application migrations only |
| `seed` | Added by `<adapter>.SeedCommand(db.Seed)`: the application's idempotent seed data in one transaction |
| `setup --tenant SLUG --email EMAIL [--organization NAME] [--password-stdin]` | Added by `authpostgres.SetupCommand()` (PostgreSQL): the first tenant and owner, once |
| `jobs status`, `jobs dead [-queue Q] [-kind K] [-limit N]`, `jobs show ID`, `jobs retry ID`, `jobs discard ID`, `jobs prune [-older D]` | Added by `queuepostgres.Commands()` (`queuemysql`, `queuesqlite` for the other databases) once the persistent job queue is wired ([JOBS](docs/JOBS.md)): inspect and repair the queue; payloads are never printed |

The `jobs` commands ship inside the production image, so operators need no
extra tooling:

```sh
docker compose run --rm app jobs status                # jobs per queue and state
docker compose run --rm app jobs dead                  # dead jobs; also -queue Q, -kind K, -limit N
docker compose run --rm app jobs show JOB_ID           # one job's metadata
docker compose run --rm app jobs retry JOB_ID          # move a dead job back to pending
docker compose run --rm app jobs discard JOB_ID        # delete a dead job permanently
docker compose run --rm app jobs prune -older 24h      # delete finished jobs
```

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

Rate limit one route (`rate_limit to: 10, within: 3.minutes, only: :create`),
per client address:

```go
limiter, _ := ratelimit.NewMemory(ratelimit.MemoryOptionsFromConfig(app.Config().RateLimit))
create, err := ratelimit.Middleware(limiter, ratelimit.ByClientIP, ratelimit.Limit{N: 10, Per: 3 * time.Minute})
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
app migrate          # creates forge_rate_limits and forge_cache_entries
app rollback         # reverts your newest migration, never these framework tables
app migrate status   # a framework set you stopped composing shows as "disabled"
```

`rollback` refuses (exit status 1) when only framework migrations are left, and
removing a framework set from `CommandsFromSets` later keeps `migrate` working;
reverting a framework migration is the explicit `Migrator.DownFramework` API
([ADR 0039](docs/adr/0039-framework-migration-ownership.md)).

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

# Logs too: every record, with trace_id and span_id, also goes to the collector
# (stdout keeps the same lines; same FORGE_TELEMETRY_OTLP_* settings)
FORGE_TELEMETRY_LOGS_EXPORTER=otlp FORGE_TELEMETRY_OTLP_ENDPOINT=http://127.0.0.1:4318 forge dev

# In a container: bind to all interfaces and confirm it (never publish the port)
FORGE_TELEMETRY_METRICS_ADDR=0.0.0.0:9464 FORGE_TELEMETRY_METRICS_PUBLIC=true forge dev

# Spans in the development log, or no telemetry at all
FORGE_TELEMETRY_EXPORTER=log forge dev
FORGE_TELEMETRY_ENABLED=false forge dev
```

If your standard-output logger redacts with `slog.HandlerOptions.ReplaceAttr`,
build it with `telemetry.NewOutputHandler` so the log exporter applies the same
redaction (otherwise the collector receives what stdout hides):

```go
logger := slog.New(telemetry.NewOutputHandler(os.Stdout, telemetry.LogFormatJSON,
	&slog.HandlerOptions{ReplaceAttr: redact}))
app, err := forge.New(forge.WithLogger(logger))
```

Pass `app.Telemetry()` to adapters to trace database statements and jobs and to
export their connection pool (in use, idle, limit, waits):
`postgres.Open(ctx, cfg, postgres.WithTelemetry(app.Telemetry()))` (generated
applications already do). Go runtime metrics (goroutines, heap, GC) are always
recorded. Collector and Prometheus configuration, the
metric catalogue, cardinality rules and what is never recorded:
[docs/OBSERVABILITY.md](docs/OBSERVABILITY.md).

### Grafana, Prometheus, Tempo and Loki, preconfigured

Every generated application includes an opt-in observability stack
(`compose.observability.yaml` and `observability/`): an OpenTelemetry
Collector, Prometheus, Tempo (traces), Loki (logs) and Grafana with the data
sources, trace-to-log links and a starter dashboard (requests, database and
connection pool, Go runtime, jobs, logs, traces) provisioned; the app's logs
reach Loki over OTLP. Everything
stays on your machine and every port is bound to `127.0.0.1`
([ADR 0038](docs/adr/0038-local-observability-stack.md)).

```bash
forge new myapp && cd myapp
export GRAFANA_ADMIN_PASSWORD="$(openssl rand -base64 18)"   # required; or put it in .env
docker compose -f compose.yaml -f compose.observability.yaml up --build
curl http://127.0.0.1:8080/v1/status
# Grafana http://127.0.0.1:3000 (user admin): dashboard "Forge application",
# Explore > Tempo (traces), Explore > Loki (logs); Prometheus http://127.0.0.1:9090
# compose interpolates the whole file, so GRAFANA_ADMIN_PASSWORD must be set for down too
docker compose -f compose.yaml -f compose.observability.yaml down      # add -v to delete the data
```

The default `docker compose up` does not start it. See
[docs/OBSERVABILITY.md](docs/OBSERVABILITY.md#local-stack-grafana-prometheus-tempo-and-loki)
for how logs reach Loki and what the dashboard shows.

## Supply chain: fuzzing, SBOM, signed images

Every fuzz target runs on each pull request and nightly for longer; SBOMs
describe the binaries and images; release images are signed keyless with
Sigstore cosign from `v*` tags only; actions and images are pinned by SHA and
digest ([docs/SUPPLY_CHAIN.md](docs/SUPPLY_CHAIN.md),
[ADR 0044](docs/adr/0044-continuous-fuzzing-sbom-signed-images.md)).

```sh
# fuzz locally: every target 10s (what CI runs), one package longer, one target
scripts/fuzz.sh
scripts/fuzz.sh -t 2m ./i18n
go test -run '^$' -fuzz '^FuzzDatabaseURL$' -fuzztime 1m ./config
scripts/fuzz.sh -l                       # list the targets
# a failing input lands in <package>/testdata/fuzz/<target>/: fix the bug, commit the input

# verify a released image (cosign v3+), then deploy it by digest
cosign verify ghcr.io/rocketinsidecorp/forge-proxy:v0.1.0 \
  --certificate-identity-regexp '^https://github\.com/rocketinsidecorp/forge-go/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
docker buildx imagetools inspect ghcr.io/rocketinsidecorp/forge-proxy:v0.1.0 --format '{{ .Manifest.Digest }}'

# read the SBOMs: an image's (SPDX attestation), the binaries' (CycloneDX)
docker buildx imagetools inspect ghcr.io/rocketinsidecorp/forge-proxy:v0.1.0 \
  --format '{{ json (index .SBOM "linux/amd64").SPDX }}' | jq -r '.packages[] | "\(.name) \(.versionInfo)"'
scripts/sbom.sh dist/sbom                # builds forge and forge-proxy; Syft runs in Docker
jq -r '.components[] | "\(.name) \(.version)"' dist/sbom/forge.cdx.json
```

Applications from `forge new` keep an SBOM of their image as the `sbom`
artifact of every CI run, and get an opt-in `.github/workflows/release.yml`
(enable it with `gh variable set FORGE_RELEASE_IMAGES --body true`, then push
a `v*` tag); their README shows how to verify the result.

## Benchmarks

Micro-benchmarks of the hot paths (router, middleware chain, JSON and HTML
rendering, CSRF, session cookie, password hashing, UUIDv7, i18n, attribute
encryption, rate limiting) and an end-to-end HTTP benchmark of a generated
application on PostgreSQL, with a load generator written with the standard
library only. Methodology, scenarios, caveats and a dated results table:
[docs/BENCHMARKS.md](docs/BENCHMARKS.md).

```sh
# micro-benchmarks, one package at a time so they do not compete for CPU
go test -p 1 -run '^$' -bench . -benchmem -count 3 ./...
go test -run '^$' -bench 'RouterMatch|MiddlewareChain' -benchmem ./router .   # a subset

# end to end: builds a generated app against this checkout, runs it on PostgreSQL,
# prints requests/s and p50/p95/p99 of /health, welcome, index, JSON show, create
export FORGE_TEST_POSTGRES_URL='postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable'
scripts/bench.sh                                  # 16 workers, 10s per scenario, 3 runs
BENCH_DURATION=3s BENCH_RUNS=1 scripts/bench.sh   # a quick check
scripts/bench.sh -format json                     # the same figures as JSON
```

CI runs every benchmark once (`go test -run '^$' -bench . -benchtime=1x ./...`)
so they keep compiling and running; it never compares numbers.

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

Coverage: `scripts/coverage.sh` measures statement coverage (`go test
-coverpkg=./...`, with the database tests when the servers above are set),
prints the gross total and the gated total (production code, without the
contract test kits) with a per-package table, and fails when the gated total is
below `.coverage-floor` ([docs/TESTING.md](docs/TESTING.md#coverage)).

CI (`.github/workflows/ci.yml`):

- `quality`: gofmt, vet, golangci-lint, race tests against PostgreSQL 18
  and MySQL 8.4 services and a real sshd, benchmarks, and govulncheck;
- `coverage`: `scripts/coverage.sh` against PostgreSQL 18 and 16 and MariaDB,
  gated by `.coverage-floor`;
- `fuzz`: every fuzz target for 10 seconds (`scripts/fuzz.sh`);
- `sbom`: CycloneDX SBOMs of the `forge` and `forge-proxy` binaries;
- `mariadb`: the `mysql` adapter's tests against MariaDB 11.8;
- `docker`: for each of PostgreSQL, MySQL, MariaDB, and SQLite, generates an
  application, builds its image, and smoke-tests its Compose stack with
  `docker compose up --wait` (the PostgreSQL run also builds the example
  image and an SBOM of the generated one).

Other workflows: `fuzz.yml` fuzzes each target for 10 minutes every night,
`scorecard.yml` runs OpenSSF Scorecard weekly, and `release.yml` publishes
signed images for `v*` tags only.

## Documentation

- [Getting started](docs/GETTING_STARTED.md)
- [Architecture](docs/ARCHITECTURE.md)
- [Authentication and authorization](docs/AUTHENTICATION.md)
- [AI agent guide and current authorization boundaries](docs/AI_AGENT_GUIDE.md)
- [Database](docs/DATABASE.md)
- [Server-rendered HTML](docs/VIEWS.md)
- [Generators and `forge destroy`](docs/GENERATORS.md)
- [Internationalization (i18n)](docs/I18N.md)
- [OpenAPI from typed routes](docs/OPENAPI.md)
- [Rate limiting](docs/RATE_LIMITING.md), [caching](docs/CACHING.md) and [file storage](docs/STORAGE.md)
- [Deployment](docs/DEPLOYMENT.md)
- [Observability](docs/OBSERVABILITY.md)
- [Credentials](docs/CREDENTIALS.md) and [attribute encryption](docs/ENCRYPTION.md)
- [Roadmap](docs/ROADMAP.md)
- [Rails parity (Forge-Go and Forge-Rust)](docs/RAILS_PARITY.md)
- [Threat model](docs/THREAT_MODEL.md)
- [Supply chain: fuzzing, SBOM, signed images](docs/SUPPLY_CHAIN.md)
- [Testing and coverage](docs/TESTING.md)
- [Benchmarks](docs/BENCHMARKS.md)
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
  - [0035 Development loop and database lifecycle](docs/adr/0035-development-loop.md)
  - [0031 Rate limiting](docs/adr/0031-rate-limiting.md)
  - [0032 Application cache](docs/adr/0032-cache.md)
  - [0033 File storage](docs/adr/0033-file-storage.md)
  - [0036 Encrypted credentials and attribute encryption](docs/adr/0036-encrypted-credentials-and-attribute-encryption.md)
  - [0037 Trusted proxies and the client address](docs/adr/0037-trusted-proxies-and-client-ip.md)
  - [0038 Local observability stack](docs/adr/0038-local-observability-stack.md)
  - [0039 Framework migration ownership in the application's migration table](docs/adr/0039-framework-migration-ownership.md)
  - [0040 OTLP log export, Go runtime and connection pool metrics](docs/adr/0040-otlp-log-export-runtime-and-pool-metrics.md)
  - [0041 Model, controller, mailer and queue job generators, and `forge destroy`](docs/adr/0041-generators-and-destroy.md)
  - [0042 Internationalization (i18n)](docs/adr/0042-internationalization.md)
  - [0043 OpenAPI from typed routes](docs/adr/0043-openapi-from-typed-routes.md)
  - [0044 Continuous fuzzing, SBOMs and signed images](docs/adr/0044-continuous-fuzzing-sbom-signed-images.md)
  - [0045 Generated two-factor settings pages with step-up](docs/adr/0045-generated-mfa-settings-pages.md)
