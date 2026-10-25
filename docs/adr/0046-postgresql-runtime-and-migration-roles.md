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
   (`FORGE_DATABASE_URL`): `NOSUPERUSER NOBYPASSRLS`, owns nothing, holds only
   `SELECT, INSERT, UPDATE, DELETE` on tables and `USAGE, SELECT, UPDATE` on
   sequences. It migrates as a migration role
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
   create` or `reset` makes later has the same privileges. Only the `migrate`
   container gets the migration credential.
4. **Production refusal.** `postgres.RequireRowSecurity(environment)` makes
   `Open` fail in production, before serving, when the connected role is a
   superuser, has `BYPASSRLS`, or is a member of such a role
   (`ErrRowSecurityBypass`). The generated bootstrap passes it. There is no
   override variable; removing the option from the bootstrap is a visible,
   reviewable decision.
5. **Tests run as the runtime role.** `postgrestest.NewRLS` creates a
   database owned by a fresh migration role, applies the migrations as it,
   and connects a fresh runtime role. Generated repository, sign-in and
   bootstrap tests use it, and every PostgreSQL resource gets a test that the
   runtime role cannot run DDL; tenant-scoped ones a test that it sees only
   the transaction's tenant and nothing without one.
6. **IDs from the application.** Generated resource tables have no
   `DEFAULT uuidv7()`. The released identity migrations keep it, so the
   migrator refuses, before applying anything, migrations that call
   `uuidv7()` on a server older than PostgreSQL 18, with the server version
   in the error.

## Consequences

- A compromised or injected serving process cannot read across tenants by
  omitting a predicate, nor switch row-level security off.
- Operators manage two credentials; [DATABASE.md](../DATABASE.md#database-roles-postgresql)
  has the SQL. An application that set `FORGE_MIGRATION_DATABASE_URL` to an
  administrator only for `forge db create` now migrates as that role.
- The refusal does not cover a serving role that owns tables (`FORCE` still
  applies to it) and runs only in production, at startup.
- `forge deploy` runs `migrate` with the serving role's env file; a
  migrate-only credential is a follow-up.
- Differences from Forge-Rust, kept deliberately: Forge-Rust requires
  `FORGE_MIGRATION_DATABASE_URL` for migrations and restores `init.sql`'s
  grants after `forge db reset`, where Forge-Go falls back to
  `FORGE_DATABASE_URL` and relies on `template1`; Forge-Rust has no
  production refusal yet (tracked in [RAILS_PARITY.md](../RAILS_PARITY.md#parity-follow-ups)).
