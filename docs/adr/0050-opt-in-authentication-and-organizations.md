# ADR 0050: Opt-in authentication and organizations, SQLite by default

- **Status:** Accepted
- **Date:** 2026-10-26 (owner decisions D30, D31, D33, D34, D35, D36)
- **Amends:** [ADR 0009](0009-multiple-databases.md) (the default database),
  [ADR 0017](0017-tenancy-enforcement-mysql-sqlite.md) (Forge identity on
  MySQL, MariaDB and SQLite), [ADR 0026](0026-authentication-pages-and-setup.md)
  (pages and `setup` only with `--auth`; `setup --org`)
- **Parity with:** Forge-Rust's `forge new --auth`, `--tenants`,
  `forge generate authentication` and `forge generate tenancy` (tracked as a
  Forge-Rust follow-up, after Forge-Go reaches 100%)

## Context

`forge new APP` generated a PostgreSQL application with Forge identity
(sign-in, two-factor and password-reset pages, the identity migrations and
`setup`) and multi-tenant tables: every generated table needed
`--tenant-scoped` or `--global`, and `forge setup` needed `--tenant`. A
developer who wanted a CRUD app had to run a PostgreSQL server and learn
tenancy first, and identity persisted only in PostgreSQL, so `-d sqlite`
applications had no sign-in at all. Rails starts from SQLite and a plain
monolith, and adds authentication with a generator.

The owner decided on 2026-10-26:

- "SQLite por padrão, PostgreSQL automático no --tenants" (SQLite by
  default, PostgreSQL automatically with `--tenants`);
- tenancy only when selected, otherwise a normal monolith;
- a multi-tenant application's tenant is an organization (`org`, `org_id`).

## Decision

- **Database (D35).** `forge new APP` uses SQLite. `--tenants` selects
  PostgreSQL (row-level security) unless `-d` is given; `-d
  postgresql|mysql|mariadb|sqlite3|none` keeps working with every mode.
  This replaces "PostgreSQL 18 (default)" in ADR 0009.
- **Authentication is opt-in (D31).** `forge new APP` generates no identity:
  a welcome page and plain tables. `--auth` adds it; `--api-auth` and
  `--tenants` imply `--auth`; `--api` cannot be combined with any of them, and
  an API application composes no identity schema. `forge generate
  authentication` later adds exactly what `--auth` generates and wires it
  with no manual edit.
- **Wiring without edits.** Generator-owned, checksummed
  `app/bootstrap/app_gen.go` declares `const Organizations`,
  `MigrationSets()` (identity, job schedules, `db/migrations`) and
  `Commands()` (migrate, rollback, seed and, with identity, `setup`);
  `cmd/NAME/main.go` passes `bootstrap.Commands()` to `forge.Main`. The
  routes seam (`routes_gen.go`) starts with a `forge:identity repository`
  block calling `identityRoutes` in `app/bootstrap/identity.go` (yours to
  change). A hand-edited `app_gen.go` is refused, as `routes_gen.go` is.
- **Identity on every database (D30).** `auth/sqlite` and `auth/mysql`
  (MySQL and MariaDB) implement the same interfaces as `auth/postgres`,
  through one shared implementation (`auth/internal/sqlidentity`) with
  per-dialect SQL. Identifiers are UUIDv7 values generated in Go; the
  identity migration has the same version on every backend. Without RLS,
  tenant isolation is the mandatory `TenantContext` predicate in every
  statement and fails closed. `auth/authtest` is one contract suite (17
  behaviours: provisioning, permissions, cross-tenant roles, revocation,
  credential version, idle timeout, compare-and-swap, throttling, recovery,
  MFA, audit, argument validation, a closed database) run on all three
  backends. Argon2id and the timing floor are unchanged.
- **Organizations are opt-in (D33, D34).** Without `--tenants` generated
  tables have no scope column, `--org-scoped` (alias `--tenant-scoped`) is
  refused with "create the app with --tenants", sign-in asks for email and
  password only, and `forge setup --email` creates one hidden default
  organization (`auth.DefaultTenantSlug`, `default`) that every user belongs
  to, so identity keeps one code path (`auth.WithDefaultTenant`). With
  `--tenants` the user-facing tenant is an organization: `org_id` columns,
  `--org-scoped`, `forge setup --org SLUG`, an "Organization" sign-in field,
  `Org`/`OrgID` names in generated code. Framework internals keep their names
  (`TenantContext`, `forge_tenants`, `forge.tenant_id`). There is no
  `--tenant-name`.
- **Converting (D36).** `forge generate tenancy [--org SLUG]` turns an
  `--auth` application into one with organizations: one migration adds
  `org_id` to every generated table, backfills it with the default
  organization, adds the index and `NOT NULL` (on SQLite a non-empty check
  through triggers, since SQLite cannot add `NOT NULL` to a column), and on
  PostgreSQL enables and forces row-level security; it renames the default
  organization to `SLUG`, rewrites every generated file as `--org-scoped`
  would write it (create migrations record their generator command, so the
  generator re-renders them) and flips `Organizations` in `app_gen.go`. The
  down migration reverses the schema and the slug, and refuses (a `CHECK`
  on a guard table) while any row belongs to another organization than the
  first; the generated code is restored with version control. It refuses applications without identity, applications that already
  have organizations and generated files edited by hand.

## Consequences

- The README quickstarts are `forge new shop --auth` (SQLite, no Docker) and
  `forge new saas --tenants` (PostgreSQL); CI runs both, plus MySQL `--auth`
  (MySQL 8.4 and MariaDB) and `forge new site`, over HTTP.
- Applications generated before `app_gen.go` existed keep working; `forge
  generate authentication` and `tenancy` refuse them with the reason.
- `--public` no longer needs `--global` in an application without
  organizations; with organizations it still refuses `--org-scoped`.
- On MySQL and MariaDB DDL is not transactional, so a tenancy migration that
  fails half-way (a full disk, a lock timeout) leaves the tables it already
  changed, as any MySQL migration with DDL does, and needs a manual fix.
- `forge destroy` of a resource converted by `forge generate tenancy` takes
  `--org-scoped`, as the converted files are what that flag generates.
