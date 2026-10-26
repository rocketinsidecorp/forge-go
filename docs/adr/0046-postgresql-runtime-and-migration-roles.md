# ADR 0046: PostgreSQL runtime and migration roles, refused RLS bypass

- **Status:** Accepted
- **Date:** 2026-10-25
- **Parity with:** Forge-Rust ADR 0011 (separate PostgreSQL migration/runtime
  roles and transaction-local RLS context)

## Context

Generated tenant-scoped tables run behind `FORCE ROW LEVEL SECURITY` policies
on `forge.tenant_id` ([ADR 0010](0010-opaque-sessions-and-fail-closed-tenancy.md),
[ADR 0021](0021-resource-generator.md)). Generated PostgreSQL applications
nevertheless served, migrated and tested as the Compose image's
`POSTGRES_USER`, a superuser, which bypasses every policy. The rule that the
serving role must not be a superuser or `BYPASSRLS` existed only in prose, and
the generated RLS test ran as a probe role inside a superuser's transaction
rather than as the role the application serves with (audit findings S-1 and
S-2). Forge-Rust already generates separate `app_migrator` and `app_runtime`
roles.

## Decision

1. **Two roles.** A PostgreSQL application serves as a runtime role
   (`FORGE_DATABASE_URL`): `NOSUPERUSER NOBYPASSRLS NOCREATEROLE
   NOREPLICATION`, owns
   nothing, holds only `SELECT, INSERT, UPDATE, DELETE` on tables and
   `USAGE, SELECT` on sequences, and may not create temporary tables. It migrates as a migration role
   (`FORGE_MIGRATION_DATABASE_URL`) that owns the database and its schema.
   The runtime role's grants come from the migration role's default
   privileges.
2. **`migrate` and `rollback` use the migration role.** The application's
   commands connect with `FORGE_MIGRATION_DATABASE_URL` when it is set
   (`config.Config.MigrationDatabase`), validated like `FORGE_DATABASE_URL`;
   its database is ignored, as `forge db` already did, so migrations always
   change the database the application serves. Unset, they use
   `FORGE_DATABASE_URL`, so MySQL, MariaDB, SQLite and existing applications
   keep working.
3. **Development roles.** `forge new` writes `docker/postgres/init.sql`, run
   once by the Compose image's superuser. It creates `app_migrator`
   (`CREATEDB`, member of `pg_signal_backend` so `forge db drop`/`reset` can
   end the runtime's sessions) and `app_runtime`, sets the schema privileges
   and default privileges in `template1`, then creates the database owned by
   `app_migrator`. Because they are in `template1`, every database `forge db
   create` or `reset` makes later has the same privileges. It revokes
   `TEMPORARY` on the database from `PUBLIC`; database privileges are not
   copied from `template1`, so a database `forge db reset` recreates has the
   default back. Only the `migrate` container gets the migration credential.
4. **Refusal outside development and test.**
   `postgres.RequireRowSecurity(environment)` makes `Open` fail in every
   environment except `development` and `test`, before serving, when the
   login role (`session_user`) or the role in effect (`current_user`) is a
   superuser, has `BYPASSRLS`, `CREATEROLE` (before PostgreSQL 16 it can
   grant itself a `BYPASSRLS` membership) or `REPLICATION` (with
   `wal_level=logical`, logical decoding reads every tenant's changes); is a
   member, directly or through other roles, of a superuser, `BYPASSRLS` or
   `REPLICATION` role (`pg_has_role(..., 'MEMBER')`, whatever the grant's
   `INHERIT` and `SET` options); is a member of `pg_execute_server_program`,
   `pg_write_server_files` or `pg_read_server_files` (paths to superuser, or
   to every tenant's data files); or owns, or is a member of the owner of, a
   table of the database with row-level security enabled
   (`pg_catalog.pg_class.relrowsecurity`), because an owner can run `ALTER
   TABLE ... NO FORCE ROW LEVEL SECURITY` and then read every tenant
   (`ErrRowSecurityBypass`; the `REPLICATION`, predefined-role and ownership rules are Forge-Rust's, adopted
   in [forge-go#63](https://github.com/rocketinsidecorp/forge-go/issues/63)
   after [forge-rust#76](https://github.com/rocketinsidecorp/forge-rust/pull/76)).
   A role that owns only tables without row-level security stays accepted.
   It also fails when the two
   differ: a role setting (`options=-c role=...`, `ALTER ROLE ... SET role`)
   makes the session act as a harmless role while `SET ROLE NONE` restores
   the login role, so the URL must log in as the serving role itself; no
   legitimate serving setup needs the indirection. The generated bootstrap
   passes it. There is no override variable; removing the option from the
   bootstrap is a visible, reviewable decision.
5. **Tests run as the runtime role.** `postgrestest.NewRLS` creates a
   database owned by a fresh migration role, applies the migrations as it,
   and connects a fresh runtime role. Generated repository, sign-in and
   bootstrap tests use it, and every PostgreSQL resource gets a test that the
   runtime role cannot run DDL; tenant-scoped ones a test that it sees only
   the transaction's tenant and nothing without one.
6. **IDs from the application.** Generated resource tables have no
   `DEFAULT uuidv7()`. Amended in forge-go QG9: the identity migrations no
   longer declare it either, so identity runs on PostgreSQL 16 and 17. They
   had not been part of any release (no version tag existed), so they were
   edited in place rather than followed by a new migration, which could not
   have made them apply on PostgreSQL 16 or 17: a `CREATE TABLE` naming a
   missing function fails. A database that applied the earlier text is
   accepted through the earlier checksum, listed per migration in
   `migrate` (`supersededChecksums`) next to the checksum of the script
   Forge ships now, and only while the build carries exactly that script
   (a tampered build is still refused); it keeps its unused defaults. When a
   migration fails with SQLSTATE 42883 naming `uuidv7` (PostgreSQL 17 and
   older), the dialect's `migrate.ScriptErrorExplainer` adds the server
   version and the two ways out (PostgreSQL 18, or a `uuidv7()` defined by
   an earlier migration). Scripts are not scanned in advance: an
   application with its own `uuidv7()` must keep migrating, and released
   migrations cannot change.

## Consequences

- A query that forgets its tenant predicate still sees only the
  transaction's tenant, and the serving role cannot switch row-level
  security off or change the schema. Row-level security is not a defense
  against arbitrary SQL injected into the serving process: the runtime role
  can call `set_config('forge.tenant_id', ...)` itself and then read that
  tenant's rows.
- Operators manage two credentials; [DATABASE.md](../DATABASE.md#database-roles-postgresql)
  has the SQL. An application that set `FORGE_MIGRATION_DATABASE_URL` to an
  administrator only for `forge db create` now migrates as that role.
- A serving role that owns, or can `SET ROLE` to the owner of, a table with
  row-level security is refused (`FORCE` applies to an owner, but an owner
  can switch it off). The check runs at startup only, and not in development
  or test, and reads only the database it is connected to.
- `forge deploy` gives the owner's credential to the one-off migrate
  container alone: `migrate.env` in `config/deploy.yml` is overlaid on the
  migrate role's environment, written to its own `0600` file on the migrate
  host and removed when the step ends
  ([forge-go#57](https://github.com/rocketinsidecorp/forge-go/issues/57)).
  Without it the container gets the serving role's env file and migrating
  fails closed on a runtime role that cannot run DDL. Forge-Rust has no
  `migrate.env` yet
  ([forge-rust#73](https://github.com/rocketinsidecorp/forge-rust/issues/73)).
- Differences from Forge-Rust, kept deliberately: Forge-Rust requires
  `FORGE_MIGRATION_DATABASE_URL` for migrations and restores `init.sql`'s
  grants after `forge db reset`, where Forge-Go falls back to
  `FORGE_DATABASE_URL` and relies on `template1`. Forge-Rust adopted the
  refusal, the runtime-role repository tests and the DDL test in
  [forge-rust#70](https://github.com/rocketinsidecorp/forge-rust/issues/70)
  ([forge-rust#76](https://github.com/rocketinsidecorp/forge-rust/pull/76)).
