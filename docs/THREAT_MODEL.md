# Threat Model

Scope: the Forge framework, the `forge` CLI, and the applications and
container images that `forge new` generates, including Phase 3 identity work
through password login, login throttling, opaque sessions, browser-cookie
sessions, CSRF, recovery, security audit, and PostgreSQL tenancy enforcement.
Method: STRIDE per component. Items
marked **Planned** are not implemented.

STRIDE key: **S**poofing, **T**ampering, **R**epudiation, **I**nformation
disclosure, **D**enial of service, **E**levation of privilege.

## Assets

- Availability of the application process and its database.
- Integrity of request handling (correct routing, no path confusion).
- Confidentiality and integrity of data stored in the database (a server,
  or a SQLite file on the application host).
- Confidentiality of configuration values, database credentials, TLS keys,
  password hashes, active session credentials, recovery credentials, and
  internal errors.
- Integrity of the database schema and its migration history.
- Integrity of the build: source, dependencies, and container image.
- The developer's filesystem when running the CLI.

## Trust boundaries

1. Internet client -> TLS proxy / load balancer (in `trusted-proxy` mode).
2. Proxy (or client, in `tls`/`plain` mode) -> application HTTP listener.
3. Deployment environment (env vars, mounted files) -> application process.
4. Application process -> database server (network connection), or ->
   SQLite file on a local volume.
5. Migration run (`<app> migrate`) -> database.
6. Container runtime -> container.
7. Upstream code (Go modules, base images) -> build.
8. Developer shell -> `forge` CLI -> filesystem, `go` subprocesses, the
   application binary, and the development database.
9. Test process -> the test servers in `FORGE_TEST_POSTGRES_URL`,
   `FORGE_TEST_MYSQL_URL`, or `FORGE_TEST_DATABASE_URL`.

## Assumptions

- **Trusted proxy.** With `FORGE_HTTP_TRANSPORT=trusted-proxy` (the generated
  image default), the application port is reachable only through a proxy
  that terminates TLS. If clients can reach the port directly, traffic is
  plain HTTP and HSTS gives no protection. Forge cannot verify this; it is an
  operator responsibility.
- Forge does not interpret `X-Forwarded-*` headers, so a client cannot spoof
  its address or scheme through them; equally, the real client IP is not
  available to handlers.
- The environment and mounted files are controlled by the operator.
- The database server (or the host and volume holding a SQLite file) and its
  administrators are trusted. Forge protects
  the connection and the application's use of it, not the server itself.
- Application handlers and the SQL they contain are outside the framework's
  control.

## HTTP edge

| Threat | STRIDE | Mitigation |
| ------ | ------ | ---------- |
| Oversized request bodies exhaust memory | D | `Content-Length` above `FORGE_HTTP_MAX_BODY_BYTES` gets `413`; all bodies wrapped in `http.MaxBytesReader` (default 1 MiB). |
| Oversized headers | D | `MaxHeaderBytes` (default 1 MiB). |
| Slowloris / slow reads and writes | D | `ReadHeaderTimeout` 5s, `ReadTimeout` 15s, `WriteTimeout` 30s, `IdleTimeout` 60s by default; all must be positive. |
| Path traversal and path confusion | T, I | Requests with `.`/`..` segments, backslashes, `%2F`, or `%5C` are rejected with `400` before routing. Route registration rejects the same. |
| Late route registration or conflicts altering routing | T | Router freezes on first request; conflicting patterns return an error at registration. |
| Malformed or unexpected JSON input | T | `web.DecodeJSON` requires `application/json`, rejects unknown fields and any data after the first value, and maps oversized bodies to `413`. Other body read failures return a generic `400 unreadable_body`, so network addresses in the underlying error are not echoed. |
| Response header injection | T | Framework header values are constants or generated UUIDs; incoming `X-Request-ID` is ignored. `net/http` neutralises CR/LF in header values when writing. |
| Clickjacking, MIME sniffing, referrer leakage | I, T | `X-Frame-Options: DENY`, CSP `frame-ancestors 'none'`, `nosniff`, `Referrer-Policy: no-referrer`. |
| Protocol downgrade | T, I | HSTS in `tls` and `trusted-proxy` modes; `production` rejects `plain`. |
| Handler panic crashes process or leaks stack | D, I | Recovery middleware logs the stack server-side and returns a generic JSON `500`. |
| Error logs flooded by disconnecting clients | D, R | `web.Error` logs client cancellations at debug level; real `5xx` failures are logged at error level with their cause. |
| Internal error details returned to clients | I | `web.Error` serializes only the `code` and `message` of a `*fault.Error`; any other error becomes a generic `500 internal_error`. Causes and metadata are never serialized. `fault.Error.Error()` omits the cause. Readiness responses expose check names and pass/fail only. Router `400`/`404`/`405` use the same format. |
| Untraceable requests | R | UUIDv7 `X-Request-ID` on every response and in every error body; the request-scoped logger (`web.Logger`) adds `request_id` to log lines. Trace correlation is **Planned** (Phase 5). |
| Readiness endpoint abused to trigger expensive checks | D | Partial: each check runs with the request context and a 2-second timeout; the generated `database` check is a single ping through the pool. Results are not cached. Keep checks cheap. |

## Configuration

| Threat | STRIDE | Mitigation |
| ------ | ------ | ---------- |
| Insecure production configuration | T, I | `production` with `plain` HTTP transport fails at startup; `tls` without cert/key fails; non-positive timeouts or limits fail; a production PostgreSQL URL without `sslmode=require`, `verify-ca`, or `verify-full`, or a MySQL URL without `tls=true`, fails unless `FORGE_DATABASE_ALLOW_PLAINTEXT=true`. Unknown URL schemes fail. |
| Unknown or misspelled values silently ignored | T | Unknown `FORGE_ENV` and `FORGE_HTTP_TRANSPORT` values fail validation; malformed numbers, durations, and booleans fail. |
| Secrets leaked through error messages | I | Malformed values are reported by variable name and expected format only. URL parse errors from `config` and from the adapters' `Open` are not wrapped, because they quote the URL. |
| Database credentials leaked through logs or output | I | `FORGE_DATABASE_URL` is a `config.Secret`: `[REDACTED]` through `fmt`, `slog`, and JSON/text marshaling; only `Reveal()` returns it. `forge doctor` drops the password and every query parameter except `sslmode`, so passwords in the user info or in `?password=`/`?sslpassword=` are not shown. pgx connection errors name the user and database, not the password. |
| TLS private key exposure | I | Key is read from a file path, not an environment variable; mount read-only. |

## Database

| Threat | STRIDE | Mitigation |
| ------ | ------ | ---------- |
| Eavesdropping or tampering on the database connection | I, T, S | PostgreSQL in `production` requires `sslmode=require`, `verify-ca`, or `verify-full`; only `verify-full` authenticates the server's host name, and `require` protects against passive eavesdropping only. MySQL/MariaDB in `production` requires `tls=true`, which verifies the certificate; `skip-verify` and `preferred` are rejected. `FORGE_DATABASE_ALLOW_PLAINTEXT=true` is an explicit opt-out for trusted private networks. SQLite has no network connection. |
| Driver options injected through the connection URL | T, E | MySQL: the driver configuration is built field by field, never as a DSN string, and every URL parameter except `tls` is rejected, so a URL cannot enable `multiStatements`, `allowAllFiles` (`LOAD DATA LOCAL INFILE`), cleartext passwords, or client-side interpolation. SQLite: the URL accepts no parameters; the path is percent-encoded into a `file:` URI, so `?` or `#` in a file name cannot add pragmas; `:memory:` and `file:` paths are rejected. |
| Stacked-query injection | T, E | MySQL multi-statement execution is enabled only on the migrator's dedicated pool, never on the application pool. MySQL uses server-side prepared statements. |
| Silent data corruption on MySQL/MariaDB | T | Every connection sets a strict `sql_mode` (`STRICT_ALL_TABLES`, `NO_ZERO_IN_DATE`, `NO_ZERO_DATE`, `ERROR_FOR_DIVISION_BY_ZERO`, `ONLY_FULL_GROUP_BY`, `NO_ENGINE_SUBSTITUTION`), `utf8mb4`, and UTC; a connection whose setup fails is discarded. |
| SQLite file read or corrupted by other users or statements | I, T | The database file is created `0600` and its directory `0750` (the `-wal`/`-shm` files inherit the file's mode); in the image the directory belongs to `nonroot`. Defensive mode blocks corruption through `writable_schema`; foreign keys are enforced; double-quoted strings are identifiers only. |
| SQL injection | T, I, E | Repositories pass values as query arguments (`$1` or `?`), which the drivers send separately from the SQL. Forge's own SQL is constant; the MySQL session setup contains only constants and numbers; test helpers quote generated database names (`pgx.Identifier`, or a name checked against `^[a-z0-9_]{1,64}$` for MySQL). Application code that concatenates input into SQL is not prevented. |
| SQL details, constraint names, or values leaked to clients | I | Every adapter's `Translate` maps errors through `sqldb.Translate` to generic messages with stable codes; the constraint name goes to `Metadata`, which `web.Error` never serializes. Only client-caused data errors (check violation, value too long, numeric out of range, invalid datetime) become `400`; other data errors and untranslated database errors become a generic `500`. |
| Slow queries or connection exhaustion | D | A server-side statement timeout (default 30s, minimum 1ms so it can never become `0` = unlimited) on every pooled connection: all statements on PostgreSQL and MariaDB, only `SELECT` on MySQL, none on SQLite (context cancellation interrupts statements there); pool capped by `FORGE_DATABASE_MAX_CONNS`; connect timeout (5s); startup ping so a bad configuration fails before serving. PostgreSQL advisory lock waits are bounded by the statement timeout. |
| Database unreachable at runtime | D | Readiness returns `503` so the orchestrator stops routing traffic; translated errors return `503 database_unavailable` with `Retryable` set. |
| Retried transactions repeating side effects | T | Retries happen only with `Attempts > 1` and only for SQLSTATE `40001`/`40P01`. Transaction functions must not have external side effects (documented rule, not enforced). |
| Cross-process races on jobs | T | PostgreSQL only: transaction-scoped advisory locks (`AdvisoryXactLock`, `TryAdvisoryXactLock`) are released at commit or rollback and cannot leak. |

## Migrations

| Threat | STRIDE | Mitigation |
| ------ | ------ | ---------- |
| Migration files edited after being applied | T | SHA-256 checksum of each up script is recorded; a mismatch stops `migrate` and `rollback`. Down scripts are deliberately not checksummed, so a broken rollback can be fixed. |
| Older release deploying over a newer schema | T | The migrator refuses to run when the database has a version the binary does not contain. |
| Misnamed or empty files silently skipped | T | Any `*.sql` file that does not match the naming convention, or contains no SQL, is an error. |
| Unreviewed SQL executed | T | Migrations are plain SQL embedded at build time (`go:embed`); the SQL in review is the SQL executed. There is no runtime migration source. |
| Concurrent migrators corrupting state | T, D | A cross-process lock on a dedicated session serializes `migrate` and `rollback` (`migrate status` is read-only and takes no lock): a PostgreSQL session advisory lock, a MySQL/MariaDB `GET_LOCK` named per database, or an operating-system lock on the SQLite sidecar file `<database>-migrate.lock` (created `0600`). Server locks are released when the session closes; the SQLite file lock is released by the kernel when the process exits, so a crashed migrator leaves no stale lock. The SQLite lock covers one host only and is not reliable on network file systems. Locks are polled, never waited for inside the database. |
| Half-applied migration | T | Each migration and its bookkeeping row run in one transaction; on PostgreSQL and SQLite a failure leaves nothing behind. Not prevented on MySQL/MariaDB, where DDL commits implicitly: a failed migration can leave earlier statements applied (it is not recorded). Files marked `-- forge:no-transaction` can leave partial state on any database. |
| Irreversible or wrong rollback | T | A rollback checks that every step has a down script before reverting any, and reverts the most recently applied migrations (by `applied_at`), which stays correct after out-of-order application. |
| Long locks during migrations block traffic | D | Not prevented: migration sessions lift the statement timeout, and a SQLite migration holds the write lock for its whole transaction. Guidance: `SET LOCAL lock_timeout`, `CREATE INDEX CONCURRENTLY`, expand/contract ([DATABASE.md](DATABASE.md#zero-downtime-changes)). |
| Migration history tampered with by a database user | T | Not prevented: anyone who can write `forge_schema_migrations` can alter it. Checksums detect accidental edits of files, not a malicious database user. |

## Development and test environment

| Threat | STRIDE | Mitigation |
| ------ | ------ | ---------- |
| Secrets committed or baked into images through env files | I | Generated `.gitignore` ignores `.env`, `.env.local`, and `.env.*.local`; `.dockerignore` excludes `.env` and all `.env.*`, so no env file reaches the image. The committed `.env.development` holds only local development values. |
| Env file contents leaked in error output | I | The parser reports the line number only, never the line. |
| Development database exposed on the network | I, T | Compose publishes the database server and the application on `127.0.0.1` only. |
| Tests touching real data or each other | T, I | `postgrestest` and `mysqltest` create a separate database per test and register its drop immediately after creating it, so it is removed even if the test fails during setup; they never touch existing databases. `sqlitetest` uses a new file in the test's temporary directory. Test server URLs should still point at dedicated servers; the MySQL test user needs to create databases (the generated applications use `root` on the local Compose server). |
| Database tests silently not running in CI | R | `FORGE_TEST_REQUIRE_DATABASE=true` turns a missing test database into a failure; the framework and generated CI set it. |

## Container

| Threat | STRIDE | Mitigation |
| ------ | ------ | ---------- |
| Code execution leads to root in container | E | Runs as `nonroot` in `gcr.io/distroless/static-debian12:nonroot`. |
| Post-exploitation tooling | E | No shell, package manager, or `curl` in the image; health check and migrations use the application binary itself. |
| Build path or host info leaked in binary | I | `-trimpath`, `CGO_ENABLED=0` static build. |
| Secrets baked into image | I | Only non-secret defaults are set with `ENV`; for server databases `FORGE_DATABASE_URL` is not set in the image and must be provided at runtime (the SQLite image sets a file path, which is not a secret). `.dockerignore` excludes env files and local `storage`. |
| Health probe weakens TLS | S | In `tls` mode `healthcheck` skips certificate verification, but only for a request to `127.0.0.1` that sends no credentials and reads only the status code. Accepted. |
| Container killed mid-request, or shutdown hung by stuck requests | D | Graceful shutdown on `SIGTERM` within one `FORGE_HTTP_SHUTDOWN_TIMEOUT` budget: HTTP drain, then shutdown hooks. At the drain deadline, remaining connections are closed so their request contexts are cancelled; the adapters' `Shutdown` stops waiting for held connections when the budget ends. |

## Supply chain

| Threat | STRIDE | Mitigation |
| ------ | ------ | ---------- |
| Compromised or vulnerable dependency | T, E | Core packages have no third-party modules, enforced by `architecture_test.go` ([ADR 0006](adr/0006-zero-dependency-core.md), [ADR 0007](adr/0007-postgresql-adapter-with-pgx.md)). The adapters add their drivers (pgx v5, go-sql-driver/mysql, modernc.org/sqlite) and their transitive modules; an application links only the adapter it imports. The SQLite driver is pure Go, so no C toolchain or cgo enters the build. `govulncheck ./...` runs in framework and generated CI. |
| Outdated dependencies | T, E | Framework and generated repositories include Dependabot configuration for Go modules, GitHub Actions, and Docker, checked weekly. |
| Mutable base image tags and CI action tags | T | CI uses `permissions: contents: read`. **Planned**: pin base images by digest and actions by commit SHA (Phase 10). |
| Unverifiable build provenance | R, T | **Planned**: SBOM and signed images (Phase 10). |

## CLI

| Threat | STRIDE | Mitigation |
| ------ | ------ | ---------- |
| `forge new` overwrites existing work | T | Refuses to write into a non-empty directory. A directory created by `forge new` is removed again if generation fails. |
| `forge generate` overwrites application files | T | Migration and view files are created with `O_EXCL`; generated names are allow-listed and existing files are refused. |
| Malicious or malformed application name | T | Name must match `^[a-z][a-z0-9-]{0,62}$`; module path is validated and may not contain `..`. Templates fail on missing keys. |
| Shell injection through arguments | E | Subprocesses (`go get`, `go mod edit`, `go mod tidy`, `go mod vendor`, `go build`, `go test`, and the application binary run by `forge dev`, `forge migrate`, and `forge rollback`) are executed directly via `os/exec`, never through a shell. |
| Wrong code vendored via `--framework-path` | T | The path's `go.mod` must declare `github.com/rocketinsidecorp/forge-go`. |
| Generated files readable by other local users | I | Files are written `0600`, directories `0750`. |
| Resource exhaustion via `forge uuid` | D | `--count` limited to 1-1000. |

## Identity and tenancy

| Threat | STRIDE | Mitigation |
| ------ | ------ | ---------- |
| Session token stolen from persistence | S, E | Tokens contain 256 random bits; persistence stores only the SHA-256 digest. `Token` exposes the plaintext only through explicit `Reveal`. |
| Browser session token read by JavaScript | I, E | Cookie sessions use the fixed `__Host-forge_session` cookie with `Secure`, `HttpOnly`, `Path=/`, no `Domain`, and `SameSite=Lax`. Cookie login never returns the session token in its body. |
| Cross-site request forgery | T, E | Unsafe cookie-authenticated requests require the fixed `__Host-forge_csrf` 256-bit token to match exactly one `X-CSRF-Token` header in constant time. Cookie logout includes the CSRF check by default. Safe methods bypass the check and therefore must not mutate state. |
| Login CSRF | S, T | Cookie login requires exactly one HTTPS `Origin` whose host equals `Request.Host`. The bearer login surface remains available for non-browser clients. |
| Password guessing and spraying | S, D | Login is throttled independently by hashed tenant+account and source keys. The default process-local limiter is bounded; PostgreSQL deployments can share counters across replicas. Unknown accounts take the same throttle path and perform a dummy PBKDF2 verification. |
| Stale or forged role claims | S, E | Tokens carry no claims. The resolver contract requires current user, membership, expiration and permissions on every request. Bearer and cookie transports are explicit and do not fall back to one another; query and tenant headers are not authentication fallbacks. |
| Missing authorization check | E | `auth.Require` denies without a principal and denies a principal without the exact permission. Permissions have no wildcard semantics. Application route coverage remains the application's responsibility. |
| Tenant omitted or forged | E, I | `tenancy.Require` has no default. Authentication installs the tenant returned by the server-side resolver; client tenant headers are ignored. |
| Tenant leaks through a pooled PostgreSQL connection | I, E | `InTenantTx` sets `forge.tenant_id` transaction-locally on every retry. Integration tests prove RLS read/write isolation and that the setting is cleared. The serving role must not be superuser or `BYPASSRLS`. |
| False database-isolation assurance | I, E | Forge explicitly makes no RLS-equivalent claim for MySQL, MariaDB or SQLite; their repositories must filter every operation by tenant. |
| Secrets accidentally persisted in audit records | I | `auth.SecurityEvent` has a closed field set with identifiers and SHA-256 digests only; there is no arbitrary metadata/payload map for passwords, bearer tokens, reset links, TOTP seeds, challenges, or backup codes. |
| Audit history lost when identities are deleted | R | PostgreSQL audit actor/subject/membership identifiers intentionally have no foreign keys, so history survives identity cleanup. |
| Audit records tampered with by a database owner | T, R | Not prevented cryptographically. Forge exposes append-only application APIs, but a database owner can still alter/delete rows; external immutable shipping/signing remains a hardening concern. |
| Agent action identity spoofing or tenant mismatch | S, E | `agent.Engine` accepts no caller-supplied principal; it requires the authenticated principal and matching tenant established in context by auth middleware. PostgreSQL journal completion is scoped to execution, tenant, actor, and reserved status. |
| Agent action partial side effect hidden as a definitive failure | T, R | Executor errors and post-execution audit/output uncertainty are persisted as `unresolved` where possible, indexed for reconciliation, and returned with `MayHaveExecuted`; tenant-scoped operator decisions are stored separately, once per execution, without rewriting the original row or retrying the action. |

## Future components (Planned)

| Area | Planned mitigation |
| ---- | ------------------ |
| Identity completion (Phase 3) | MFA, generated application wiring, remaining identity-flow audit instrumentation, and a decided tenancy-enforcement strategy for MySQL, MariaDB, and SQLite. |
| AI execution (Phase 7) | Provider integrations and generated actions; auditable policy/version and permission evidence; first-party journal adapters for MySQL, MariaDB, and SQLite. Policy and approval callbacks remain application trust points and cannot be proven human/restrictive by the engine. |

## Residual risks

- Direct network exposure of a `trusted-proxy` deployment.
- `sslmode=require` or `verify-ca` without host name verification, or
  `FORGE_DATABASE_ALLOW_PLAINTEXT=true` outside a trusted network.
- MySQL/MariaDB TLS can only verify against the system certificate pool; a
  server with a private CA can only be reached in production with
  `tls=skip-verify` (unverified) plus `FORGE_DATABASE_ALLOW_PLAINTEXT=true`.
- On MySQL, writes and DDL have no server-side timeout; bound them with
  context deadlines.
- SQLite: whoever can read the volume can read the database; there is no
  database-level authentication.
- The same `FORGE_DATABASE_URL` is used for serving and migrating by default,
  so the application role usually holds DDL privileges. Run `migrate` with a
  separate, more privileged URL and give the serving role only the
  privileges it needs.
- Handler code that ignores the body limit's error, logs secrets, builds SQL
  from input, or builds responses from untrusted input.
- Long-running or lock-heavy migrations, and partial state from
  `no-transaction` migrations.
- TLS certificate rotation requires a restart.
- Default CSP blocks HTML rendering; applications that relax it take on XSS
  risk. XSS can act with a browser session and read its CSRF token even though
  the HttpOnly session cookie itself is not readable by JavaScript.
