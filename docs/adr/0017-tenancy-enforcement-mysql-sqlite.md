# ADR 0017: Tenancy enforcement for MySQL, MariaDB and SQLite

- **Status:** Accepted
- **Date:** 2026-10-03
- **Amended by:** [ADR 0050](0050-opt-in-authentication-and-organizations.md)
  (Forge identity on MySQL, MariaDB and SQLite, under the same mandatory
  predicates)

## Context

PostgreSQL enforces tenancy through Row Level Security (RLS) policies that read a
transaction-local `forge.tenant_id` setting installed by `InTenantTx`
(`SET LOCAL forge.tenant_id = $1` via `set_config('forge.tenant_id', $1, true)`).
This setting is scoped to the transaction and cannot leak through the connection
pool. The threat model documents this at line 171.

The threat model (line 172) explicitly states:

> Forge explicitly makes no RLS-equivalent claim for MySQL, MariaDB or SQLite;
> their repositories must filter every operation by tenant.

MySQL, MariaDB and SQLite do not have a feature equivalent to PostgreSQL's RLS
with session/transaction-local settings that policies can reference. The three
databases differ in capabilities:

| Database      | RLS equivalent | Transaction-local variables | Notes                                       |
|---------------|----------------|----------------------------|---------------------------------------------|
| PostgreSQL    | Yes (policies) | `SET LOCAL` / `set_config` | Used by Forge                               |
| MySQL 8.4     | No             | Session only (`SET`)       | No transaction-local; no policy engine      |
| MariaDB 11.8  | No             | Session only (`SET`)       | No transaction-local; no policy engine      |
| SQLite        | No             | N/A (embedded)             | Single file, single process, no server      |

Three implementation approaches were considered for MySQL/MariaDB:

1. **Views with `SECURITY BARRIER` (PostgreSQL term) / `DEFINER` views**
   - MySQL supports views with `DEFINER` and `SQL SECURITY DEFINER/INVOKER`.
   - Views can embed `WHERE tenant_id = @tenant_id` using a session variable.
   - *Problem:* MySQL session variables (`@tenant_id`) are session-scoped, not
     transaction-scoped. They leak across transactions on a pooled connection
     unless explicitly cleared. A `DEFERRED`/`AFTER` trigger could clear them,
     but MySQL does not support transaction-level triggers.
   - *Problem:* `SQL SECURITY DEFINER` views run with the definer's privileges,
     which expands the trust boundary and complicates least-privilege roles.
   - *Verdict:* Rejected — session variables are unsafe with pooling; no
     transaction-scoped cleanup mechanism.

2. **Triggers (BEFORE INSERT/UPDATE/DELETE) that enforce tenant_id**
   - A `BEFORE INSERT` trigger could set `NEW.tenant_id = @tenant_id`.
   - A `BEFORE UPDATE/DELETE` trigger could reject rows where
     `OLD.tenant_id != @tenant_id`.
   - *Problem:* Same session-variable leakage as views. Triggers cannot see a
     transaction-local value; they only see session variables.
   - *Problem:* Triggers add hidden logic, make debugging harder, and are
     difficult to test in isolation. They also run with the trigger
     definer's privileges.
   - *Verdict:* Rejected — same session-variable leakage; opaque enforcement.

3. **Application-layer filtering in repositories (explicit `WHERE tenant_id = ?`)**
   - Every repository method that reads or writes tenant-scoped data includes
     `tenant_id` in the `WHERE` clause (for reads) or as a bound parameter
     (for writes).
   - The tenant comes from `tenancy.Require(ctx)` — the same fail-closed
     context API used by PostgreSQL repositories.
   - *Advantage:* Explicit, testable, no hidden database logic, works identically
     on MySQL and MariaDB, no privilege escalation, no pooling hazard.
   - *Trade-off:* Discipline required — a missed `WHERE` clause is a data leak.
     Mitigated by code review, integration tests that assert isolation, and
     eventually a linter rule.

For SQLite the analysis is simpler: SQLite is an embedded database serving one
application instance on one host (ADR 0009). There is no connection pool across
processes, no concurrent writers from different tenants, and no server-side
session. The only tenancy risk is a bug in the application that omits a
`tenant_id` filter. The same application-layer filtering approach applies.

## Decision

**MySQL, MariaDB and SQLite will enforce tenancy exclusively at the
application layer, in repository code.** Every repository method that accesses
tenant-scoped tables must include the current tenant's ID (from
`tenancy.Require(ctx)`) as a bound parameter in its `WHERE` clause for reads
and as a column value for writes.

No database-level mechanism (views, triggers, session variables, policies) will
be used for tenancy enforcement on these adapters.

## Consequences

### Positive

- **Uniformity:** The same `tenancy.Require(ctx)` pattern works across all
  adapters. PostgreSQL uses it to drive `InTenantTx`; MySQL/SQLite use it
  directly in repository SQL.
- **Explicit and auditable:** A code reviewer can see the `WHERE tenant_id = ?`
  in every query. No hidden triggers or view definitions to inspect.
- **No pooling hazard:** Session variables are never used, so there is no risk
  of a tenant ID leaking across transactions on a pooled connection.
- **No privilege escalation:** Repositories run with the application's database
  role; no `DEFINER` views or trigger definers with elevated privileges.
- **Testable:** Integration tests can assert that a query for tenant A returns
  zero rows when tenant B's data exists, using the same test pattern as
  PostgreSQL's RLS tests.

### Negative

- **Discipline-dependent:** A missed `tenant_id` filter is a silent data leak.
  This is a human-process risk, not a technical one.
- **Boilerplate:** Every repository method repeats the `tenant_id` parameter.
  Can be reduced with a small helper (see Implementation plan).
- **No defense-in-depth:** If application code is compromised, the database
  offers no second line of defense. Accepted — the threat model already assumes
  application code is outside framework control (Assumption 5).

### Migration / compatibility

- No migration required for existing PostgreSQL users — this ADR only applies
  to MySQL, MariaDB and SQLite adapters.
- When `auth/mysql` and `auth/sqlite` packages are created, they will follow
  this pattern from the start.

## Implementation plan

1. **Add a tenancy helper in each adapter** (or shared in `sqldb`/`tenancy`):
   ```go
   // tenancy.RequireTenantID(ctx) -> (uuid.UUID, error)
   // Returns the tenant ID as UUIDv7 or tenancy.ErrMissing.
   ```
   This avoids repeating `tenant, _ := tenancy.Require(ctx); tenant.ID` in every
   repository method.

2. **Create `auth/mysql/repository.go`** mirroring `auth/postgres/repository.go`
   but with explicit `tenant_id` filters:
   - `LookupPassword`: `JOIN forge_tenants t ON t.id = m.tenant_id WHERE ... AND t.id = ?`
   - `CreateSession`: same join + `WHERE m.id = ? AND t.id = ?`
   - `ResolveSession`: same join + `WHERE s.token_digest = ? AND t.id = ?`
   - All write operations (`ReplacePasswordHash`, `RevokeSession`, etc.) include
     `tenant_id` in the `WHERE` or as a column value.

3. **Create `auth/sqlite/repository.go`** with the same pattern, using `?`
   placeholders and `TEXT` UUID columns.

4. **Add integration tests** for each adapter that:
   - Create two tenants with overlapping data (same email, different tenant).
   - Verify that a query for tenant A never returns tenant B's rows.
   - Verify that a write for tenant A cannot affect tenant B's rows.
   - These tests mirror `postgres/integration_test.go:TestInTenantTxEnforcesRLSAndDoesNotLeakTenant`.

5. **Document the pattern** in the adapter READMEs and in `DATABASE.md` so that
   application developers who write their own repositories follow the same rule.

6. **Future:** Consider a static analysis rule (go vet / golangci-lint) that
   flags queries on tenant-scoped tables without a `tenant_id` bind parameter.

## Alternatives considered

| Alternative                    | Rejected because                                     |
|--------------------------------|------------------------------------------------------|
| MySQL views + session variable | Session variables leak across pooled transactions    |
| MySQL/MariaDB triggers         | Same leakage; opaque; privilege escalation           |
| SQLite `PRAGMA`/hooks          | No tenancy model; single-process anyway              |
| Shared library SQL builder     | Against Forge's SQL-first philosophy (ADR 0009)      |

## References

- [THREAT_MODEL.md lines 171-172](../THREAT_MODEL.md) — tenancy threat statements
- [ADR 0009](0009-multiple-databases.md) — multiple database adapter interface
- `postgres/tx.go` — `InTenantTx` implementation
- `auth/postgres/repository.go` — PostgreSQL repository pattern (joins for RLS)
- `tenancy/tenancy.go` — `Require`, `New` context API