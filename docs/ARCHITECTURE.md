# Architecture

This document describes the Forge framework foundations and the layout it
prescribes for generated applications. Planned work is
labelled as such; see [ROADMAP.md](ROADMAP.md).

## Principles

- **Standard library core.** Core packages depend only on the Go standard
  library and each other ([ADR 0006](adr/0006-zero-dependency-core.md)).
  HTTP is plain `net/http` ([ADR 0002](adr/0002-stdlib-net-http-core.md)).
  Third-party dependencies (database drivers) are confined to adapter
  packages ([ADR 0007](adr/0007-postgresql-adapter-with-pgx.md),
  [ADR 0009](adr/0009-multiple-databases.md)).
- **SQL-first persistence.** Explicit SQL for the chosen database
  (PostgreSQL, MySQL, MariaDB, or SQLite), no ORM ([DATABASE.md](DATABASE.md)).
- **Secure by default.** Timeouts, size limits, security headers, production
  transport requirements, and database TLS in production are on without
  configuration.
- **Fail fast.** Invalid configuration stops the process at startup
  ([ADR 0005](adr/0005-environment-configuration-fail-fast.md)); an
  unreachable database stops `Configure`.
- **Production container from day one** ([ADR 0003](adr/0003-docker-by-default.md)).
- **No speculative scaffolding.** Directories and abstractions appear when
  there is code to put in them ([ADR 0004](adr/0004-hexagonal-application-layout.md)).

## Framework packages

```text
Core (standard library only)
forge (root)     App composition, forge.Main / forge.Execute, commands, shutdown hooks, health routes
├── auth         Opaque sessions, principal context, bearer middleware, authorization
├── agent        Provider-neutral allowlisted action policy and execution pipeline
├── tenancy      Fail-closed tenant context
├── config       Config struct, defaults, environment loading, validation, Secret
├── router       Route registry on http.ServeMux
├── httpserver   http.Server, middleware chain, run/shutdown lifecycle
├── web          JSON responses, error format, strict JSON and form decoding, redirects, flash, method override, request context values
├── csrf         CSRF protection for browser requests (double-submit token, cross-origin rejection)
├── views        Server-rendered HTML: layouts, partials, request-aware helpers
├── pagination   UUIDv7 keyset pagination and cursors
├── events       Versioned application-event envelope with bounded JSON payloads
├── ratelimit    Fixed-window rate limiting: bounded in-memory limiter, HTTP middleware, shared-store interface
├── cache        Application cache: bounded LRU+TTL memory store, Fetch stampede protection, typed JSON helpers
├── storage      File storage port, HMAC-signed links, safe serving handler, multipart upload; storage/disk adapter
├── health       Readiness check registry
├── uuid         UUIDv7 type and generator
├── fault        Typed application errors
├── sqldb        Error kinds and translation, retries, bounded close, database/sql wrapper
├── migrate      Migration engine, Dialect interface, migrate/rollback commands
└── dbtest       Test-server discovery and helpers for the *test packages

Adapters (may use third-party modules)
postgres               PostgreSQL: pgx v5 pool, transactions, translation, advisory locks, migration dialect
outbox/postgres        Transactional outbox persistence, leases, and dispatcher
agent/postgres         Tenant-RLS-protected execution journal for registered agent actions
ratelimit/{postgres,mysql,sqlite}  Shared rate limit buckets in the database
cache/{postgres,mysql,sqlite}      Shared application cache (Solid Cache equivalent)
postgres/postgrestest  Per-test PostgreSQL databases
mysql                  MySQL and MariaDB: go-sql-driver/mysql pool, translation, migration dialect
mysql/mysqltest        Per-test MySQL/MariaDB databases
sqlite                 SQLite: modernc.org/sqlite pool, translation, migration dialect and file lock
sqlite/sqlitetest      Per-test SQLite files

Internal
internal/cli       forge CLI commands (cmd/forge is a thin main)
internal/scaffold  Templates and file generation for `forge new`
internal/version   Build metadata set via -ldflags
```

Dependency direction (imports within the module):

| Package                 | Imports |
| ----------------------- | ------- |
| `forge`                 | `config`, `health`, `httpserver`, `router`, `web` |
| `auth`                  | `csrf`, `fault`, `tenancy`, `uuid`, `web` |
| `csrf`                  | `fault`, `web` |
| `views`                 | `csrf`, `uuid`, `web` |
| `tenancy`               | `fault`, `uuid` |
| `httpserver`            | `config`, `fault`, `uuid`, `web` |
| `router`                | `fault`, `web` |
| `pagination`            | `fault`, `uuid` |
| `events`                | `uuid` |
| `web`                   | `fault`, `uuid` |
| `sqldb`                 | `config`, `fault` |
| `migrate`               | `forge`, `config` |
| `dbtest`                | `config`, `migrate`, `uuid` |
| `ratelimit`             | `config`, `fault`, `web` |
| `cache`                 | `config`, `fault`, `web` |
| `storage`               | `config`, `fault`, `uuid`, `web` |
| `storage/disk`          | `config`, `storage` |
| `ratelimit/postgres`, `cache/postgres` | `ratelimit` or `cache`, `postgres`, pgx v5 |
| `ratelimit/mysql`, `cache/mysql` | `ratelimit` or `cache`, `mysql`, `sqldb` |
| `ratelimit/sqlite`, `cache/sqlite` | `ratelimit` or `cache`, `sqlite`, `sqldb` |
| `config`, `fault`, `health`, `uuid` | standard library only |
| `postgres`              | `forge`, `config`, `migrate`, `sqldb`, `tenancy`, pgx v5 |
| `outbox/postgres`       | `events`, `postgres` |
| `mysql`                 | `forge`, `config`, `migrate`, `sqldb`, go-sql-driver/mysql |
| `sqlite`                | `forge`, `config`, `migrate`, `sqldb`, modernc.org/sqlite |
| `postgrestest`, `mysqltest`, `sqlitetest` | their adapter, `config`, `dbtest`, `migrate` (and `sqldb` or pgx) |

Adapters depend on the core; the core never imports an adapter. The core
declares the extension points (`forge.Command`, `App.OnShutdown`,
`App.Readiness`). `architecture_test.go` runs `go list -deps` on every core
package and fails if any dependency is outside the standard library and the
core. An application builds and links only the adapter it imports, so a
SQLite application does not link pgx or the MySQL driver.

### config

`config.Load()` starts from `config.Default()`, overlays `FORGE_*`
environment variables, and calls `Validate()`. Validation rejects unknown
environments and transports, `production` with `plain` transport, `tls`
without certificate and key files, an empty address, non-positive HTTP
timeouts or size limits, database pool settings out of range, a database
statement timeout below `1ms`, and a database URL with an unknown scheme or a
malformed `postgres://`, `mysql://`, or `sqlite:` form. In `production`, a
PostgreSQL URL must carry `sslmode=require|verify-ca|verify-full` and a MySQL
URL `tls=true`, unless `FORGE_DATABASE_ALLOW_PLAINTEXT=true`.
`Database.Adapter()` reports the adapter the scheme selects, and
`Database.SQLitePath()` the SQLite file. Malformed durations, integers, and
booleans are reported by variable name without echoing the value, and URL
parse errors are not wrapped because they quote the URL.

`config.Secret` holds `FORGE_DATABASE_URL`. It prints `[REDACTED]` through
`fmt` (`String`, `GoString`), `log/slog` (`LogValue`), and text/JSON
marshaling (`MarshalText`); `Reveal()` returns the value. The full variable
list is in [DEPLOYMENT.md](DEPLOYMENT.md#environment-variables).

### router

- Patterns must be `METHOD /path` using Go 1.22 `ServeMux` syntax
  (`GET /users/{id}`). Methods must be uppercase.
- Registration rejects relative paths, backslashes, and `.`/`..` segments.
  Conflicting patterns return an error instead of panicking.
- The router freezes on the first served request (an atomic flag, written
  only once); later registration returns `router.ErrFrozen`.
- At request time, paths containing encoded slashes (`%2F`), encoded
  backslashes (`%5C`), literal backslashes, or `.`/`..` segments receive a
  JSON `400` (`invalid_path`) before routing.
- Requests that match no route receive a JSON `404` (`route_not_found`);
  requests whose path matches but method does not receive a JSON `405`
  (`method_not_allowed`) with the `Allow` header. `ServeMux` decides between
  the two; its own redirects to a canonical path (for example `/tree` to
  `/tree/` for a `/tree/` pattern) are passed through unchanged. As a result,
  matched requests are looked up in the mux twice.
- `Routes()` returns the registered routes sorted by path and method.
- `Resources(path, handler, middleware...)` registers the conventional
  index, new, create, show, edit, update (`PATCH` and `PUT`) and destroy
  routes for the methods the handler implements; `ResourceRoutes(path)`
  lists them. `forge routes` and `forge inspect` expand `Resources` calls
  with a literal path.

### httpserver

Wraps the router in a fixed security/lifecycle chain, outermost first:

1. **Security headers** - `X-Content-Type-Options: nosniff`,
   `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`,
   `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'`, and
   `Strict-Transport-Security: max-age=31536000; includeSubDomains` when the
   transport is `tls` or `trusted-proxy`. The CSP targets JSON APIs; pages
   rendered by a `views.Load` renderer replace it with
   `views.DefaultContentSecurityPolicy`, and other HTML handlers set their
   own.
2. **Request ID** - generates a UUIDv7, sets `X-Request-ID` on the response,
   and stores the ID and a logger carrying `request_id` in the request
   context (`web.RequestID`, `web.Logger`). Incoming `X-Request-ID` headers
   are not trusted or reused. If ID generation fails, the request is answered
   with a JSON `500`.
3. **Panic recovery** - logs method, path, panic value, and stack with the
   request logger; responds `500` with the JSON error body
   (`internal_error`). `http.ErrAbortHandler` is re-panicked.
4. **Body limit** - rejects a declared `Content-Length` above
   `FORGE_HTTP_MAX_BODY_BYTES` with a JSON `413` (`body_too_large`), and wraps
   the body in `http.MaxBytesReader` for undeclared or chunked bodies.
5. **Application middleware** - zero or more optional handlers registered
   with `forge.WithHTTPMiddleware`. They run in declaration order, outermost
   first among custom middleware, after the body limit and inside panic
   recovery. They receive the framework request ID and may add request-scoped
   context for tracing, metrics, or application-specific behavior.
6. **`App.Use` middleware** - added from `Configure`, in call order, still
   before routing. Generated applications install `web.MethodOverride`,
   `csrf.Protection.Handler` and `web.Flash` here. `Use` fails once the
   application has served a request.
7. **Router.**

Because security headers and the request ID come first, every response,
including `413`, recovered panics, and router errors, carries them. Every
error the framework itself produces uses the JSON error format.

The `http.Server` is configured with `ReadHeaderTimeout`, `ReadTimeout`,
`WriteTimeout`, `IdleTimeout`, and `MaxHeaderBytes` from configuration. In
`tls` mode it calls `ListenAndServeTLS` with the configured files; otherwise
it serves plain HTTP. `Run(ctx)` blocks until the context is cancelled, then
calls `Shutdown` bounded by `FORGE_HTTP_SHUTDOWN_TIMEOUT`. If requests are
still running at that deadline, it closes all connections, which cancels
those requests' contexts so they release resources such as database
connections.

`trusted-proxy` mode means "TLS is terminated upstream". It enables HSTS and
satisfies the production transport rule. It does not rewrite the client
address or scheme from `X-Forwarded-*` headers.

### web

The HTTP adapter toolkit for handlers:

- `web.JSON(w, status, value)` writes a JSON response.
- `web.Error(w, r, err)` writes the only error format Forge produces:

  ```json
  {"error":{"code":"task_not_found","message":"task not found","request_id":"01..."}}
  ```

  Only the `Code` and `Message` of the first `*fault.Error` in the chain
  reach the client, with the status from `fault.Error.Status()`. Any other
  error becomes `500 internal_error`. Responses with status `500` or above
  are logged at error level through the request logger, with the error and,
  for a fault, its cause (`cause` attribute). When the client has gone away
  (the request context is done and the error is `context.Canceled`), the
  failure is logged at debug level instead. Causes and metadata are never
  serialized.
- `web.DecodeJSON(r, dst)` decodes strictly: the content type must be
  `application/json` (`415 unsupported_media_type` otherwise); unknown
  fields, invalid JSON, wrong types, an empty body, and any data after the
  first value (including a stray `}` or `]`) are `400 invalid_json`; an
  oversized body is `413 body_too_large`; any other read failure is
  `400 unreadable_body` with a generic message. Errors are faults, so they can
  be passed to `web.Error`.
- `web.RequestID(ctx)` and `web.Logger(ctx)` return the request ID and the
  request-scoped `*slog.Logger` (never nil; a discarding logger outside a
  request).
- For HTML ([VIEWS.md](VIEWS.md), [ADR 0025](adr/0025-server-rendered-html.md)):
  `web.DecodeForm` binds `form:"..."` tagged struct fields from a bounded
  `application/x-www-form-urlencoded` body (unknown or repeated fields are
  `400 invalid_form`; conversion problems are `web.FormErrors`, `422`);
  `web.Redirect` answers `303` to local paths only; `web.Negotiate` and
  `web.PrefersHTML` read `Accept`; `web.Flash` and `web.SetFlash` carry
  one-request messages in the `__Host-forge_flash` cookie;
  `web.MethodOverride` routes `POST` forms with `_method=PUT|PATCH|DELETE`.

### csrf

`csrf.Protection.Handler` rejects unsafe requests that a browser marks as
cross-origin (`net/http.CrossOriginProtection`) or whose `X-CSRF-Token`
header or `authenticity_token` form field does not match the
`__Host-forge_csrf` cookie, the token cookie login also issues
([ADR 0012](adr/0012-cookie-sessions-and-csrf.md)). Requests with a bearer
token or a JSON body skip the token check. `csrf.Token` issues the cookie
when a page first embeds a token. `csrf.Verified` reports that a request's
token was actually checked; `auth.Browser` (the HTML sign-in, sign-out and
password-reset flows, [ADR 0026](adr/0026-authentication-pages-and-setup.md))
refuses any request for which it is false.

### views

`views.New` parses a flat directory into one template set (unchanged).
`views.Load` parses `layouts/`, `_partial.gohtml` files and pages into one
template set per page and adds the CSRF, flash, method-override, DOM id and
`dict` helpers; `RenderRequest` binds the request-scoped helpers through
pooled clones. Rendering is buffered.

### pagination

Keyset pagination over UUIDv7 primary keys: `WHERE id < $cursor ORDER BY id
DESC LIMIT $n`. `FromQuery` reads `cursor` and `limit` (default `50`, maximum
`200`) and returns `400` faults for invalid values. The cursor is the
base64url (unpadded) encoding of the last UUID; `DecodeCursor` accepts only
the canonical encoding of a UUIDv7 (it re-encodes and compares, because Go's
base64 decoder skips `\r` and `\n` even in strict mode), so each ID has
exactly one valid cursor. Both malleability cases were found by
`FuzzDecodeCursor` and are kept as seeds in `pagination/testdata`. `NewPage(rows, limit, id)` takes up to `limit+1` rows and
uses the extra row only to decide whether to emit `next_cursor`.
`Page[T]` serializes as `{"items":[...],"next_cursor":"..."}`. See
[DATABASE.md](DATABASE.md#repositories-and-dbtx).

### health

`health.Registry` holds named `Check func(context.Context) error` functions.
`Ready` runs them sequentially in name order with the request context and
reports each check's name and pass/fail; error text is not exposed.

| Route               | Behaviour |
| ------------------- | --------- |
| `GET /health/live`  | Always `200 {"status":"live"}` while the process serves HTTP |
| `GET /health/ready` | `200` if all checks pass, else `503`; body `{"healthy":..,"checks":[{"name":..,"healthy":..}]}` |
| `GET /health`       | Same as `/health/ready` |

Each check runs with its own timeout, `health.DefaultCheckTimeout` (2
seconds) for the registry `forge.App` uses (`health.NewWithTimeout` sets
another value on a standalone registry); a check must honour its context.
Checks run one after another, so the endpoint can take up to the number of
checks times the timeout. Generated applications register `database` (a pool
ping).

### uuid

`uuid.UUID` is a 16-byte RFC 9562 UUIDv7. The package-level generator uses
`crypto/rand` and is monotonic within a process: within the same millisecond,
or if the clock moves backwards, it increments the random field instead of
reseeding. `Parse` accepts only version 7, RFC variant values in canonical
36-character form. `UUID` implements `encoding.TextMarshaler`,
`json.Marshaler`, `driver.Valuer` (string), and `sql.Scanner` (string, text
bytes, or 16 raw bytes of a UUIDv7). See
[ADR 0001](adr/0001-uuidv7-default-identifier.md).

### fault

`fault.Error` carries a stable `Code`, a client-safe `Message`, a `Category`
(`invalid`, `unauthorized`, `forbidden`, `not_found`, `conflict`,
`unavailable`, `internal`), an optional `Cause`, `Metadata`, `Retryable`, and
`HTTPStatus`.

- `Error()` returns `code: message` and never includes the cause; `Unwrap`
  exposes the cause to `errors.Is`/`errors.As`.
- `Is` matches another `*fault.Error` with the same `Code`, so a sentinel is
  recognized after `WithCause` copies it.
- `Status()` returns `HTTPStatus` when set, otherwise the category default:
  `400`, `401`, `403`, `404`, `409`, `503`, and `500` for `internal` or an
  unknown category.
- `fault.From(err)` returns the first `*fault.Error` in the chain, or nil.

### Database packages

See [DATABASE.md](DATABASE.md) for usage.

**`sqldb`** (core). `Kind` is a database-independent failure category
(not found, unique, foreign key, exclusion, invalid data, conflict, timeout,
unavailable); each adapter's classifier maps driver errors to a
`Classification{Kind, Constraint}`, and `sqldb.Translate` maps kinds to
faults, so every database yields the same public codes. `Retry` reruns a
transaction on conflicts with exponential backoff; `CloseWithin` bounds a
pool close by a context. `sqldb.DB` wraps `*sql.DB` with the configured pool
limits and provides `DBTX` (sqlc's `database/sql` interface), `InTx`,
`InTxWith`, `Translate`, `Ping`, `Close`, `Shutdown`, and `SQL()`.

**`migrate`** (core). `LoadMigrations` reads and validates the SQL files;
`New(db, dialect, migrations, logger)` returns a `Migrator` that owns its
`*sql.DB` and never reuses sessions; `Up`, `Down`, `Status`, and `Close` run
it. A `Dialect` supplies placeholders, session preparation, a non-blocking
lock, and table lookup. `Commands(fs, opener)` builds the `migrate` and
`rollback` subcommands. The bookkeeping table uses portable types
([ADR 0008](adr/0008-sql-migrations-embedded-in-the-binary.md)).

**`dbtest`** (core). `ServerURL` finds the test server
(`FORGE_TEST_<ADAPTER>_URL`, else `FORGE_TEST_DATABASE_URL` when its scheme
matches) and skips or, with `FORGE_TEST_REQUIRE_DATABASE=true`, fails the
test without one; `DatabaseName` and `Migrate` are shared helpers.

**Adapters.** Each has `Open`, `Translate`, `NewMigrator(cfg, migrations,
logger)`, `Commands(fs)`, and a test package with `New`, `NewMigrated`,
`Config`, and `NewWithConfig`. Each refuses a URL that selects another
adapter.

- `postgres` keeps pgx natively: `Open` returns `*postgres.DB` (a `pgxpool`
  pool with `statement_timeout`, a default `application_name` of `forge`,
  and `timestamptz` scanned as UTC) with its own `DBTX`, `InTx`/`InTxWith`,
  `SQLState`, `ConstraintName`, advisory lock helpers, `Shutdown`, and
  `Pool()`. Its migrator uses a dedicated connection through pgx's `stdlib`.
- `mysql` serves MySQL and MariaDB and returns `*sqldb.DB`. It builds the
  driver configuration field by field, accepts only the `tls` URL parameter,
  and configures every connection (utf8mb4, UTC, strict `sql_mode`, a
  flavor-specific statement timeout). Multi-statement execution is enabled
  only on the migrator's pool; the migration lock is `GET_LOCK`.
- `sqlite` uses the pure-Go modernc.org/sqlite driver (so `CGO_ENABLED=0`
  holds) and returns `*sqldb.DB`. It enforces foreign keys, WAL,
  `busy_timeout`, immediate transactions, defensive mode, and no
  double-quoted strings on every connection; the migration lock is an
  operating-system lock on a sidecar file.

### forge (application runtime)

`forge.New(opts...)` builds an `App` from a validated `config.Config`
(`WithConfig`) and an `*slog.Logger` (`WithLogger`), registers the health
routes, and constructs the HTTP server. `App` exposes `Config`, `Logger`,
`Handle`, `HandleFunc`, `Resources`, `Use`, `Readiness`, `Routes`,
`Handler`, `OnShutdown`, `Close`, and `Run`.

- `OnShutdown(hook)` registers a `func(ctx) error` that releases a resource.
  Hooks run once, in reverse registration order.
- `Run(ctx)` serves until `ctx` is cancelled, drains in-flight requests,
  then runs the hooks. Draining and hooks share one budget,
  `FORGE_HTTP_SHUTDOWN_TIMEOUT`, counted from the cancellation: the hooks'
  context expires at cancellation time plus that timeout, so time spent
  draining is not available to the hooks.
- `Close(ctx)` runs the hooks; use it directly only for an `App` that is
  never run (for example in tests).

`forge.Main(configure, commands...)` is the conventional entrypoint, where
`type Configure func(ctx context.Context, app *forge.App) error` and `ctx` is
cancelled on `SIGINT`/`SIGTERM`:

1. Validates the extra commands: each needs a name and a `Run` function,
   names must be unique, and `serve`, `healthcheck`, and `help` are reserved.
   A violation exits with code `2`.
2. Parses the subcommand (default `serve`). `help`, `-h`, and `--help` list
   the commands and exit `0`. Unknown commands, or arguments to `serve` or
   `healthcheck`, exit `2`.
3. Loads configuration from the environment; invalid configuration is
   reported on stderr and exits `1`.
4. Creates a logger on stdout: JSON in `staging`/`production`, text in
   `development`/`test`.
5. `serve`: builds the `App` and calls `configure`. If `configure` fails, the
   shutdown hooks registered so far run and the process exits `1`. Otherwise
   it runs the server until the signal, then shuts down as above. Exit code
   `1` if building, configuring, serving, or shutdown fails.
6. `healthcheck`: sends `GET /health/live` to `127.0.0.1` on the port from
   `FORGE_HTTP_ADDR` with a 2-second timeout and exits `0` only on `200`. In
   `tls` mode it uses HTTPS and skips certificate verification, because the
   certificate is issued for the public hostname, not the loopback address.
   The probe sends no credentials. It exists because distroless images have
   no shell or `curl`.
7. Extra commands: `forge.Command{Name, Summary, Run}`. `Run(ctx, env, args)`
   receives the remaining arguments and a `CommandEnv` with the loaded
   configuration, the logger, stdout, and stderr. An error is printed as
   `<name>: <error>` and exits `2` if it wraps `forge.ErrUsage` (invalid
   arguments), otherwise `1`. The adapters' `Commands` (built on
   `migrate.Commands`) contribute `migrate` and `rollback` this way and
   report argument errors with `forge.ErrUsage`.

`forge.Execute(ctx, args, lookup, stdout, stderr, configure, commands...) int`
is the same flow with injectable arguments, environment lookup, and output
streams, for tests.

### CLI

`internal/cli` implements the `forge` command (see the
[README](../README.md#cli)). It runs `go` and the application binary directly
through `os/exec`, never through a shell. `forge dev`, `forge migrate`, and
`forge rollback` build `cmd/<name>` into a temporary directory and run the
binary, so migrations in development go through the same code path as in
production. `forge dev`, `forge test`, `forge migrate`, `forge rollback`, and
`forge doctor` load `.env.development` and `.env.local`, with the process
environment taking precedence.

## Generated application layout

`forge new` creates only what the application needs to run:

```text
cmd/<name>/main.go   forge.Main(bootstrap.Configure, <adapter>.Commands(db.Migrations())...)
app/bootstrap/       composition root: database pool, readiness check "database", routes,
                     method override + CSRF + flash middleware
                     (generated with GET /v1/status, assets/application.css and tests)
app/views/           embedded HTML: layouts/application.gohtml, shared/_flash.gohtml,
                     views.New() renderer and tests
db/db.go             embeds db/migrations
db/migrations/       SQL migrations (empty, with .keep)
```

`--database` chooses the adapter (default `postgresql`); `--database none`
(or `--skip-database`) omits `db/` and the database wiring. Per-database
settings (Compose image and service, CI service, development and test URLs,
production TLS rule, migration lock description) are profiles in
`internal/scaffold/database.go`. Templates live in
`internal/scaffold/templates` and are embedded in the CLI binary. Generated Go
files are passed through `gofmt`.

As the application grows, code is placed in hexagonal layers
([ADR 0004](adr/0004-hexagonal-application-layout.md)):

| Directory            | Contents                                                        | May import |
| -------------------- | --------------------------------------------------------------- | ---------- |
| `app/domain`         | Entities, value objects, domain rules and errors                | standard library, `uuid`, `fault` |
| `app/application`    | Use cases and the ports (interfaces) they need                  | `domain`; framework value packages (`uuid`, `fault`, `pagination`) |
| `app/adapters`       | HTTP handlers, repository implementations, other port adapters  | `application`, `domain`, framework |
| `app/infrastructure` | Technical resources: connection pools, clients, telemetry setup | framework, drivers |
| `app/bootstrap`      | Composition root that wires everything into `forge.App`         | everything |

These directories are not generated as empty scaffolding; the layer rules
are conventions and are not enforced by a check. Generated applications open
the database pool directly in `app/bootstrap`.
[`examples/basic-api`](../examples/basic-api) shows the layout with
`app/domain`, `app/application`, `app/adapters/persistence`,
`app/adapters/httpapi`, and `app/bootstrap`.

## Resource generator

`forge generate resource NAME field:type... (--tenant-scoped | --global)`
([ADR 0021](adr/0021-resource-generator.md)) adds one resource to the layers
above without editing existing files. It reads the database adapter from
`FORGE_DATABASE_URL` exactly as `forge inspect` does and writes:

| File | Contents |
| ---- | -------- |
| `db/migrations/<UTC timestamp>_create_<plural>.{up,down}.sql` | The table, in the dialect of the application's database |
| `app/domain/<name>.go` | Entity, `<Name>Input`, `<Name>Changes`, per-field validation, `fault` sentinels with stable codes |
| `app/application/<name>.go` | `<Name>Repository` port and `<Name>Service` (create, get, list, update, delete) |
| `app/adapters/persistence/<name>.go` | Explicit SQL for PostgreSQL, MySQL/MariaDB or SQLite, errors through the adapter's `Translate` |
| `app/adapters/httpapi/<name>.go` | `Register<Plural>(router, service, protect)` and thin handlers |
| `*_test.go` next to each | Domain table tests, an in-memory-fake service test, database integration tests and handler tests |

`forge generate scaffold` (same arguments) additionally writes
`app/adapters/htmlui/<name>.go` (+ test) and `app/views/<plural>/*.gohtml`:
another inbound adapter over the same `<Name>Service`, which depends on
`application` and `domain` and on the framework's `views`, `web` and `router`
packages, never on `persistence` or `httpapi`.

Every exported name carries the resource name (`ProductService`,
`ErrProductNotFound`, `RegisterProducts`), so several resources share the
same packages; the generator refuses to run when a name it would declare is
already declared there. Rules the generated code follows:

- **UUIDv7 identifiers only.** `uuid` on PostgreSQL (`DEFAULT uuidv7()` is a
  fallback for rows inserted by hand), `CHAR(36)` on MySQL and MariaDB, `TEXT`
  in a `STRICT` table on SQLite. Ids come from an injected `uuid.New`. A
  framework test renders every database and scope and fails on `serial`,
  `AUTO_INCREMENT`, `gen_random_uuid()`, `UUID()` or any float or decimal
  type.
- **Explicit scope.** Tenant-scoped repositories take the tenant from
  `tenancy.Require(ctx)` and bind `tenant_id` in every statement. On
  PostgreSQL they also run in `InTenantTx` behind `ENABLE` and `FORCE ROW
  LEVEL SECURITY` policies, and the generated tests prove the policy itself
  with a non-superuser role. MySQL, MariaDB and SQLite have no row-level
  security, so the statement predicates are the only isolation
  ([ADR 0017](adr/0017-tenancy-enforcement-mysql-sqlite.md)).
- **No public routes.** `Register<Plural>` returns an error without a
  `protect` function and asks it for `<plural>:read` or `<plural>:write` per
  route. With Forge identity (PostgreSQL) the migration seeds those
  permissions and the printed wiring uses `auth.RequireAudited` behind the
  authentication middleware (writes to a global resource additionally stay
  behind a deny-all `protectPlatformWrite`, since tenant roles cannot
  authorize changes to rows every tenant reads); elsewhere the printed guard denies every request
  until the application implements it.
- **Optimistic locking and keyset pagination.** `PATCH` and `DELETE` carry the
  version the client read and answer `409` when it is stale; lists are
  newest-first keyset pages. `PATCH` changes only the members it names, and
  `null` clears an optional field.
- **No hidden edits.** The generator never edits `bootstrap.go`; it prints the
  imports and the wiring to add, also with `--dry-run`.

Templates are embedded text templates in `internal/cli/resourcetemplates`,
rendered by `internal/cli/resource*.go` and passed through `gofmt`.

## Architecture decision records

| ADR | Title |
| --- | ----- |
| [0001](adr/0001-uuidv7-default-identifier.md) | UUIDv7 as the default identifier |
| [0002](adr/0002-stdlib-net-http-core.md) | Standard library `net/http` as the HTTP core |
| [0003](adr/0003-docker-by-default.md) | Docker by default |
| [0004](adr/0004-hexagonal-application-layout.md) | Hexagonal application layout |
| [0005](adr/0005-environment-configuration-fail-fast.md) | Environment configuration, fail fast |
| [0006](adr/0006-zero-dependency-core.md) | Zero-dependency core (amended by 0007) |
| [0007](adr/0007-postgresql-adapter-with-pgx.md) | PostgreSQL adapter with pgx, in the main module (amended by 0009) |
| [0008](adr/0008-sql-migrations-embedded-in-the-binary.md) | SQL migrations embedded in the application binary (amended by 0009) |
| [0009](adr/0009-multiple-databases.md) | Multiple databases: PostgreSQL, MySQL, MariaDB and SQLite |
| [0010](adr/0010-opaque-sessions-and-fail-closed-tenancy.md) | Opaque sessions and fail-closed tenancy |
| [0011](adr/0011-login-throttling.md) | Login throttling by account and source |
| [0012](adr/0012-cookie-sessions-and-csrf.md) | Explicit secure-cookie sessions with double-submit CSRF |
| [0017](adr/0017-tenancy-enforcement-mysql-sqlite.md) | Tenancy enforcement for MySQL and SQLite |
| [0018](adr/0018-transactional-outbox-foundation.md) | Transactional outbox foundation |
| [0019](adr/0019-outbox-dispatch.md) | PostgreSQL outbox dispatch |
| [0020](adr/0020-background-jobs-scheduling.md) | Background jobs and scheduling |
| [0021](adr/0021-resource-generator.md) | Resource generator |
| [0025](adr/0025-server-rendered-html.md) | Server-rendered HTML |
| [0026](adr/0026-authentication-pages-and-setup.md) | Authentication pages, sign-in redirects and first-owner setup |

## Not yet implemented

OpenTelemetry, OpenAPI generation, the AI execution subsystem, and
relationships, decimal types, soft delete and audit columns in generated
resources (`forge generate` supports SQL migrations, embedded HTML views, job
stubs and resources). See
[ROADMAP.md](ROADMAP.md).
