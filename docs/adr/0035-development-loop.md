# ADR 0035: Development loop and database lifecycle

- **Status:** Accepted
- **Date:** 2026-10-20

## Context

Rails' developer loop is `bin/dev` (code reloads on save, the browser follows)
and `rails db:create|drop|reset|setup|prepare|schema:dump|schema:load` plus
`rails dbconsole`. Forge had `forge dev` (build once, run) and
`forge migrate|rollback|seed`, which delegate to the application binary. Three
requirements pull against each other:

- the loop must be fast and must not wreck a running app when the code is
  momentarily broken;
- the reload machinery must never exist in a production binary, and must not
  weaken the strict page Content-Security-Policy views send (ADR 0025);
- database commands that destroy data are the most dangerous thing the CLI can
  do, and Rails' protection (`ProtectedEnvironmentError`) is a flag away.

## Decision

### `forge dev`: watch, rebuild, restart

- A polling watcher (standard library, one goroutine, stopped with the context)
  scans the tree every `--poll-interval` (default 500 ms) and reports one
  batch after the tree has been quiet for `--debounce` (default 300 ms). It
  watches Go sources (not `_test.go`), `go.mod`/`go.sum`, templates, assets,
  migrations and the two env files; it never enters dot-directories, `tmp`,
  `vendor`, `node_modules`, `bin`, `storage`, `log`, `coverage` or `dist`, and
  refuses trees beyond 50 000 files. Polling instead of inotify keeps the
  dependency list unchanged and behaves the same on every file system, including
  bind mounts.
- A supervisor builds into a fresh binary. **The old process keeps serving
  until a build succeeds**; only then it gets SIGTERM, is awaited (up to 35 s,
  then killed) and the new binary starts. A failed build prints the compiler
  output, tells the browser, and leaves the running application alone. If the
  tree changes while a build runs, the stale build is discarded. An env-file
  change restarts without rebuilding (the environment is read at start).
  A crashed application is not restarted in a loop; it waits for the next
  change. The application and `go build` each run in their own process group
  (Unix), and the group is signalled as a whole: SIGTERM then, after the grace
  period or once the leader has exited, SIGKILL, so helpers an application
  started do not outlive a restart and a cancelled build leaves no compiler
  running. On other platforms only the process forge started is signalled. Scan
  failures after startup (the tree vanished, or grew past the bound) are logged
  once per run of failures.
- `--no-reload` is the old behavior: build once, run, forward interrupts.

### Browser reload: a development-only proxy

The reload code is not in the application. With `FORGE_ENV=development` and
`FORGE_HTTP_TRANSPORT=plain`, **the forge CLI itself listens on
`FORGE_HTTP_ADDR`** and runs the application on a private `127.0.0.1` port
(`FORGE_HTTP_ADDR` overridden in the child only). The proxy:

- forwards every request unchanged (same path bytes, same `Host`; unclean paths
  are not redirected), asking the application for uncompressed responses;
- injects `<script src="/__forge/reload.js" nonce="...">` before the last
  `</body>` of `text/html` responses only (an ASCII case-insensitive search on
  the original bytes, so Latin-1 and other non-ASCII pages are not corrupted);
  JSON, Turbo streams, fragments without `</body>`, compressed, attachment,
  HEAD, 304 and over-4 MiB responses of known length are not touched; the page
  gets `Cache-Control: no-store`. A response of known length is buffered; a
  chunked or streamed one is passed on as it arrives, holding back only its
  last 4 KiB, where the closing tag is, until the body ends;
- serves the script and a Server-Sent Events stream under `/__forge/`, which
  the application never sees; at most 32 open streams;
- on a successful restart (after the new process accepts connections)
  broadcasts `reload`; on a failed build broadcasts `build-error`, which the
  script shows as an overlay (built with `textContent`, styled through the
  CSSOM, so it works under a strict CSP);
- while there is no application, answers with a small wait page that connects to
  the stream and reloads itself. Compiler output appears on that page, in the
  overlay and in the stream only for requests whose `Host` is a loopback name
  and only when the proxy listens on loopback, so a DNS-rebound page cannot read
  it.

`devloop.NewProxy` returns an error unless the environment is `development`; in
any other environment, over TLS or `trusted-proxy`, or with an invalid
configuration, `forge dev` says why reload is off and only restarts.

**CSP.** The proxy edits the page's policy per response, only as far as the
injected tag needs: a fresh nonce is added to `script-src-elem`/`script-src`
(or a `script-src-elem` is added when only `default-src` applies) unless script
elements already allow `'self'` (and no `'strict-dynamic'`), and `'self'` is
added to `connect-src` for the event stream when it is not already allowed.
Forge's default view policy (`script-src 'self'; connect-src 'self'`) passes
unchanged. A nonce switches `'unsafe-inline'` off, so a policy that relies on
`'unsafe-inline'` (with no nonce, hash or `'strict-dynamic'`) gets `'self'`
instead of a nonce and its inline scripts keep working in development. No
`unsafe-inline`/`unsafe-eval` is ever added; every policy header on the response
is handled. `Report-Only` policies are left alone, and so are policies set in a
`<meta http-equiv>` tag, which the proxy does not parse: if one blocks the
script the browser says so in the console and the page does not reload itself
(send the policy as a header, which is what Forge's views do).

### `forge db`

`create`, `drop`, `reset`, `setup` (create + migrate + seed), `prepare` (setup
when the database is missing, else migrate), `migrate`, `rollback`, `status`,
`seed`, `schema:dump`, `schema:load`, `console`. `forge migrate|rollback|seed`
remain and mean the same as their `db` forms. Migrations, rollback and seed
still run the application binary's own commands, built once per invocation.

- **create/drop** need a server connection that is not the application's
  database: PostgreSQL connects to the `postgres` (then `template1`)
  maintenance database, MySQL/MariaDB to `information_schema`, through the
  adapter's own URL and TLS rules; SQLite creates (`O_EXCL`, 0600) or removes
  the file and its `-wal`, `-shm`, `-journal` and `-migrate.lock` companions.
  `FORGE_MIGRATION_DATABASE_URL`, when set, supplies a privileged role for these
  and for dump/load/console; it is validated like `FORGE_DATABASE_URL`, must
  select the same adapter, and its database is ignored: the target always comes
  from `FORGE_DATABASE_URL`. Database names are limited to
  `[A-Za-z0-9_][A-Za-z0-9_$-]{0,62}`; `postgres`, `template0`, `template1`,
  `mysql`, `information_schema`, `performance_schema` and `sys` are never
  created or dropped. PostgreSQL drops use `WITH (FORCE)` (13+) so a running
  development server cannot block them.
- **schema:dump** writes `db/schema.sql` with `pg_dump --schema-only --no-owner
  --no-privileges`, `mysqldump --no-data --skip-add-drop-table` or `sqlite3
  .schema`, from `PATH`; a missing tool is a clear error. Output is
  deterministic: `Dumped by/from` comments, the random `\restrict` tokens and
  `AUTO_INCREMENT` counters are removed. The applied migrations
  (`version, name, checksum`) are appended as `INSERT`s into
  `forge_schema_migrations` with `applied_at = 0`, so a loaded database knows
  its history and `migrate` has nothing to do; the PostgreSQL insert is
  schema-qualified because `pg_dump` ends with an empty `search_path`. RLS
  policies are part of the dump.
- **schema:load** runs the file with `psql --single-transaction`,
  `mysql` or `sqlite3` (inside `BEGIN`/`COMMIT`) into an existing database and
  fails rather than replace existing tables. It is for bootstrapping test and
  CI databases.
- **console** runs `psql`, `mysql` or `sqlite3`. Arguments after `--` go to the
  client.
- **Credentials never travel in arguments.** PostgreSQL tools get `PGHOST`,
  `PGUSER`, `PGPASSWORD`, `PGSSL*`... in the child's environment, and every
  inherited `PG*` variable (service file, options) is dropped; MySQL tools read a
  `0600` option file in a private directory (`--defaults-file`, first argument,
  so `~/.my.cnf` and `/etc/my.cnf` are not read; removed on exit)
  with `protocol=tcp` and the TLS option spelled for MySQL or MariaDB;
  SQLite gets an absolute path.

### Protection of destructive commands

`drop`, `reset`, `schema:load` and `rollback` are refused unless **both**:
`--force-production` is given, and either a person types the database name at a
terminal, or `--yes` is given **and** `FORGE_ALLOW_DESTRUCTIVE=true` is set in
the process environment (an env file cannot grant it; any other value is
refused). The protection applies to everything except a *local* database in
the `development` or `test` environment: it covers production and staging, and
any database whose server is not on this machine (a loopback address,
`localhost`, a Unix socket path, or a SQLite file are local; the privileged
URL's server counts too). So a real server is guarded by its address even when
`FORGE_ENV` is unset (the CLI then defaults it to `development`) or wrong. Rails
guards only its production environment; Forge's guard is by evidence about the
target. Because the target is resolved through the normal configuration, an
invalid production configuration refuses too. `schema:load` and `console` also
refuse the reserved system databases, as `create` and `drop` do. `console`
runs the client outside forge's cancellable context, so Ctrl-C reaches the
client (it cancels a query) instead of killing the session; forge forwards
hang-up, quit and terminate to the client and removes the option file in every
case.

## Consequences

- The dev port is held by the forge process, not the application; applications
  that bind it themselves under `forge dev` use `--no-reload`.
- Applications behind `forge dev` see `127.0.0.1` as the peer (the original is in
  `X-Forwarded-For`); client-IP rate limits and audit sources are development
  values.
- Compiled output, templates and migrations are embedded, so every change except
  env files rebuilds; there is no partial hot swap.
- Reload and build-error delivery is best effort over SSE; the terminal is the
  source of truth.
- `schema:dump` needs a client at least as new as the server for PostgreSQL.
- `schema.sql` is a convenience for fresh databases; migrations stay the history
  of record, and the loaded `applied_at` is 0.
