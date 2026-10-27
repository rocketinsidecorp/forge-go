# ADR 0009: Multiple databases: PostgreSQL, MySQL, MariaDB and SQLite

- **Status:** Accepted (amends [ADR 0007](0007-postgresql-adapter-with-pgx.md) and [ADR 0008](0008-sql-migrations-embedded-in-the-binary.md))
- **Date:** 2026-09-26
- **Amended by:** [ADR 0050](0050-opt-in-authentication-and-organizations.md)
  (2026-10-26: SQLite is the default; `--tenants` selects PostgreSQL unless
  `-d` is given)

## Context

Forge started PostgreSQL-only. Teams choose a database for reasons outside
the framework (existing infrastructure, managed offerings, the operational
simplicity of SQLite for small services), and Rails offers the same choice
at `rails new --database`. Forge is SQL-first: applications write each
database's own SQL, so the framework cannot hide dialects and should not
try. What it can share is everything around the SQL: configuration,
connection setup with secure defaults, transactions, error translation,
migrations, test databases, health checks and the generated Docker setup.

## Decision

- **Supported databases:** PostgreSQL 18 (the default until ADR 0050 made
  it SQLite), MySQL 8.4, MariaDB 11.8 and SQLite. `forge new --database postgresql|mysql|mariadb|sqlite|none`
  (`-d`; `--skip-database` is `none`) generates the adapter wiring, compose
  service, CI service container and development settings for the choice.
- **Selection by URL scheme.** `FORGE_DATABASE_URL` starting with
  `postgres://`, `mysql://` (MySQL and MariaDB) or `sqlite:` selects the
  adapter; `config` validates each form and its production rules
  (PostgreSQL `sslmode=require|verify-ca|verify-full`, MySQL `tls=true`,
  both overridable only with `FORGE_DATABASE_ALLOW_PLAINTEXT=true`). An
  adapter refuses a URL of another adapter.
- **Shared core, stdlib-only:**
  - `sqldb`: one mapping from database-independent error kinds to public
    faults (every database yields the same codes and statuses),
    transaction retries, bounded pool shutdown, and a `database/sql`
    wrapper with a sqlc-compatible `DBTX`.
  - `migrate`: the migration engine moved out of `postgres`, on
    `database/sql`, with a small `Dialect` interface (placeholders, session
    preparation, non-blocking lock, table lookup). The bookkeeping table
    uses portable types; `applied_at` is Unix microseconds written by the
    migrator.
  - `dbtest`: test-server discovery (`FORGE_TEST_<ADAPTER>_URL`, falling
    back to `FORGE_TEST_DATABASE_URL` when its scheme matches) shared by
    `postgrestest`, `mysqltest` and `sqlitetest`.
- **Adapters:**
  - `postgres` keeps pgx natively (its `DB`, `DBTX` and `InTx` are
    unchanged); only migrations go through `database/sql`, via pgx's
    `stdlib` on a dedicated connection.
  - `mysql` uses `github.com/go-sql-driver/mysql`, with strict SQL mode,
    UTC and utf8mb4 forced, and multi-statement execution enabled only on
    the migrator's dedicated pool (never on the application pool, where it
    would enable stacked-query injection).
  - `sqlite` uses `modernc.org/sqlite`, a pure-Go driver, so images keep
    `CGO_ENABLED=0` and the distroless static base. Foreign keys, WAL,
    `busy_timeout` and immediate transactions are enforced on every
    connection.
- Every adapter has the same surface: `Open`, `Translate`, `NewMigrator`,
  `Commands`, and a `*test` package with `New`, `NewMigrated`, `Config`,
  `NewWithConfig`.

## Consequences

- Choosing a database is a generator flag; switching later means porting
  SQL and migrations, as in Rails with raw SQL.
- Behaviour that differs by database is explicit and documented:
  - MySQL and MariaDB commit DDL implicitly, so a failed migration can
    leave part of its changes applied.
  - MySQL's statement timeout (`max_execution_time`) bounds only `SELECT`;
    MariaDB's `max_statement_time` bounds every statement; SQLite has no
    server-side timeout and relies on context cancellation.
  - SQLite serves one application instance on one host.
  - UUIDv7 values are stored as `uuid` (PostgreSQL), `CHAR(36)` (MySQL,
    MariaDB) or `TEXT` (SQLite); the canonical lowercase text form sorts
    in creation order everywhere.
- `go.mod` gains the MySQL and SQLite drivers. Applications only build and
  link the adapter they import; the architecture test keeps the core free
  of all drivers.
- CI tests each adapter against a real server (MariaDB in its own job) and
  smoke-tests a generated application per database in Docker.
