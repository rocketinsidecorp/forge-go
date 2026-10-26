# Multi-tenancy

Forge tenancy is fail-closed. `tenancy.WithContext` accepts only a UUIDv7
tenant and `tenancy.Require` returns `tenant_required` when no tenant was
resolved. There is no process-wide, environment, or default tenant fallback.

The authentication middleware installs the tenant returned by the current
session resolver. Applications must never resolve a tenant from an unsigned
client header.

## PostgreSQL row-level security

`postgres.DB.InTenantTx` and `InTenantTxWith` require a tenant in the context
before acquiring a connection. On every transaction attempt they execute:

```sql
SELECT set_config('forge.tenant_id', $1, true);
```

The third argument makes the setting transaction-local, so it is cleared at
commit/rollback and cannot leak through the connection pool. Serialization
retries reinstall it before application SQL runs.

Tables opt into RLS explicitly in reviewed migrations:

```sql
ALTER TABLE tasks ENABLE ROW LEVEL SECURITY;
ALTER TABLE tasks FORCE ROW LEVEL SECURITY;

CREATE POLICY tasks_tenant_isolation ON tasks
USING (tenant_id = NULLIF(current_setting('forge.tenant_id', true), '')::uuid)
WITH CHECK (tenant_id = NULLIF(current_setting('forge.tenant_id', true), '')::uuid);
```

This is the predicate `forge generate resource --tenant-scoped` emits. It
compares `uuid` values, so the `(tenant_id, id)` index stays usable (a
`tenant_id::text` cast would defeat it), and `NULLIF` turns an unset or empty
setting into `NULL`, which matches no row: the policy fails closed.

The serving database role must not be a superuser or have `BYPASSRLS`, because
those roles bypass policies. This is enforced, not only advised: the generated
bootstrap opens its pool with `postgres.RequireRowSecurity`, and in `staging`
and `production` the application refuses to start when the role it logs in
as or acts as is a superuser, has `BYPASSRLS`, `CREATEROLE` or `REPLICATION`,
is a member of a superuser, `BYPASSRLS` or `REPLICATION` role or of
`pg_execute_server_program`, `pg_write_server_files` or `pg_read_server_files`,
or owns (or can `SET ROLE` to the owner of) a table with row-level security,
or when the URL switches the session to another role. Row-level security protects against a query that forgets its
tenant predicate; it does not stop SQL injected into the serving process,
which can set `forge.tenant_id` itself. Generated applications serve as
a runtime role (`FORGE_DATABASE_URL`, `app_runtime` in development) that owns
nothing and holds only data privileges, and migrate as a separate role that
owns the schema (`FORGE_MIGRATION_DATABASE_URL`, `app_migrator`); see
[Database roles](DATABASE.md#database-roles-postgresql). Repository methods for
tenant-owned data run inside `InTenantTx`; direct pool queries do not receive a
tenant setting and RLS denies them when policies are written fail-closed.

The PostgreSQL integration test creates a non-superuser role and proves that
two tenant contexts see only their rows, cross-tenant inserts fail, a missing
tenant fails before a transaction, and the setting does not remain in the
pool. `postgrestest.NewRLS` gives tests the deployment's two roles; the
framework's tests and every generated tenant-scoped resource use it to prove,
as the runtime role, that a statement without a tenant predicate sees only
the transaction's tenant, a session without a tenant sees nothing, a row
cannot be written for another tenant, and the role cannot change the schema
(`CREATE`, `ALTER ... DISABLE ROW LEVEL SECURITY`, `TRUNCATE`, `DROP` all fail
with `42501`).

## MySQL, MariaDB, and SQLite

These adapters have no equivalent database-level RLS guarantee in Forge.
Repositories must include `tenant_id` in every key and predicate and accept a
tenant explicitly or call `tenancy.Require(ctx)`. This application-layer rule
is weaker than PostgreSQL RLS; the framework does not claim otherwise. A
future adapter API may make scoped repositories easier, but it must not imply
database enforcement that the database does not provide.
