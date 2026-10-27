# Multi-tenancy

Forge tenancy is fail-closed. `tenancy.WithContext` accepts only a UUIDv7
tenant and `tenancy.Require` returns `tenant_required` when no tenant was
resolved. There is no process-wide, environment, or default tenant fallback.

The authentication middleware installs the tenant returned by the current
session resolver. Applications must never resolve a tenant from an unsigned
client header.

## Organizations are opt-in

Generated applications show tenants to their users as organizations, and
only when created with `forge new --tenants` (PostgreSQL unless `-d` is given;
[ADR 0050](adr/0050-opt-in-authentication-and-organizations.md)): generated
tables are `--org-scoped` (an `org_id` column) or `--global`, sign-in asks
for the organization, and `forge setup --org SLUG` creates the first one.
The framework keeps its own names: `TenantContext`, `forge_tenants`,
`forge.tenant_id`; generated SQL binds `org_id` to the tenant.

An application created with `--auth` alone has plain tables and one
organization. Identity still runs the same tenant-scoped code: `forge setup
--email` creates a tenant with the hidden slug `default`
(`auth.DefaultTenantSlug`), and the login and recovery services built with
`auth.WithDefaultTenant` and `auth.WithRecoveryDefaultTenant` use it when the
form names none. That is a sign-in default only: a session still resolves to
its membership's tenant, and nothing installs a tenant without one.
`forge generate tenancy` converts such an application ([GENERATORS.md](GENERATORS.md#authentication-and-organizations)).

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

This is the predicate `forge generate resource --org-scoped` emits, on its
`org_id` column under a policy named `<table>_org`. It compares `uuid` values,
so the `(org_id, id)` index stays usable (a `::text` cast would defeat it), and `NULLIF` turns an unset or empty
setting into `NULL`, which matches no row: the policy fails closed.

The serving database role must not be a superuser or have `BYPASSRLS`, because
those roles bypass policies. This is enforced, not only advised:
`postgres.Open` checks every pool it opens, in generated and hand-written
applications alike, and in `staging` and `production` (every environment
except `development` and `test`) the application refuses to start when the
role it logs in
as or acts as is a superuser, has `BYPASSRLS`, `CREATEROLE` or `REPLICATION`,
is a member of a superuser, `BYPASSRLS` or `REPLICATION` role or of
`pg_execute_server_program`, `pg_write_server_files` or `pg_read_server_files`,
or owns (or can `SET ROLE` to the owner of) a table with row-level security,
or holds `TRUNCATE`, `REFERENCES` (also per column) or `TRIGGER` on one
(none of them is subject to row-level security; `TRUNCATE` empties every
tenant's rows), or can use a partition without row-level security of a
partitioned table with it,
or when the URL switches the session to another role. The only opt-out is
`postgres.AllowRowSecurityBypass()`, passed in code to a pool that must use
such a role on purpose and never serves tenants (see [Database
roles](DATABASE.md#database-roles-postgresql)). Row-level security protects against a query that forgets its
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
