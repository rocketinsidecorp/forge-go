# ADR 0039: Framework migration ownership in the application's migration table

- **Status:** Accepted
- **Date:** 2026-10-20
- **Refines:** [ADR 0008](0008-sql-migrations-embedded-in-the-binary.md)
- **Issue:** rocketinsidecorp/forge-go#13 (Forge-Rust: ADR 0030, rocketinsidecorp/forge-rust#10)

## Context

Several Forge packages ship SQL migrations that an application composes with
its own through `<adapter>.CommandsFromSets` (or `migrate.LoadMigrationSets`):
the identity schema (`auth/postgres`), the outbox (`outbox/postgres`), the
agent journal (`agent/postgres`), the job queue, the rate limiter and the
cache (`queue`, `ratelimit`, `cache`, one set per database). They are recorded
in the application's `forge_schema_migrations` table next to the
application's migrations.

The engine treated the merged history as one:

1. `Migrator.Down`, behind `<app> rollback`, `forge rollback` and
   `forge db rollback`, reverted the most recently applied migration of any
   set. A framework set is usually applied after the application's first
   migrations (its versions are newer, or it was added later), so the first
   rollback dropped the identity tables, or, for the queue, rate-limit and
   cache sets, which have no down script, failed as "irreversible" and left
   the application unable to roll back its own migration.
2. Removing a framework set from `CommandsFromSets` (turning a feature off)
   made every `migrate` and `rollback` fail: the applied version was
   "unknown to this build".

## Decision

Keep the shared table and decide ownership by version, with the same rules as
Forge-Rust's ADR 0030.

- **Registry.** `migrate.FrameworkMigrations()` lists every migration a Forge
  package ships for the application's table: version, name and owning package.
  The sets are plain `fs.FS` values with no identity of their own, so the list
  is explicit; `TestFrameworkMigrationsMatchShippedFiles` walks the repository
  and fails when a shipped migration is not listed under its name and owner,
  or a listed one is not shipped. Released entries never change, and released
  migration files are never edited.
- **Application rollback.** `Migrator.Down` (and so every `rollback` command)
  reverts only application migrations: the most recently applied versions not
  in the registry, even when a framework migration was applied later. With
  more steps than applied application migrations it reverts those there are.
  When only framework migrations remain it reverts nothing and returns
  `*migrate.FrameworkMigrationOnlyError`, naming the newest one; the command
  exits with status 1. With nothing applied it still does nothing.
- **Explicit framework revert.** `Migrator.DownFramework(ctx, steps)` reverts
  the most recently applied framework migrations the migrator contains and
  never an application migration. No command exposes it: dropping a framework
  table is a deliberate operator or test action. The queue, rate-limit, cache
  and outbox migrations have no down script, so it refuses them as
  irreversible before reverting anything.
- **Disabled sets.** An applied registered version that the build does not
  contain is tolerated: its table and row stay, `migrate status` reports it as
  `disabled`, and composing the set again finds it applied and verifies its
  checksum. Every other unknown version still fails closed.
- **Reserved versions.** A migration that uses a registered version under
  another name is refused with `*migrate.ReservedVersionError` by
  `LoadMigrationSets` (before any connection opens) and by `migrate.New`. A
  database row recorded under a registered version with another name is
  refused the same way by `migrate` and `rollback`: ownership is never decided
  for the wrong migration. Checksums of every applied migration the build
  contains are verified as before.

### Rejected: a separate bookkeeping table per framework package

It would need a transition that moves rows already recorded in
`forge_schema_migrations` on PostgreSQL, MySQL/MariaDB and SQLite, a new
registration API replacing the documented `CommandsFromSets` usage, and the
version list anyway for databases whose feature was turned off before the
transition ran. The registry fixes both defects with no data migration and no
API removal.

### Rejected: tagging each set with its owner at composition time

`CommandsFromSets(app, authpostgres.Migrations())` cannot tell which
filesystem is the application's, and a disabled set is by definition not
passed at all, so ownership must be known without it.

## Consequences

- An application rollback can no longer drop a framework table or be blocked
  by an irreversible framework migration, whatever the version order.
- Turning a framework feature off no longer breaks `migrate`; the unused table
  stays until an operator drops it (or composes the set again and calls
  `DownFramework`).
- A new framework migration must be added to the registry in the same change;
  the repository test enforces it.
- `migrate/migratetest.RunFrameworkOwnership` is the shared suite every
  adapter runs (SQLite and PostgreSQL in CI, MySQL/MariaDB when a server is
  configured).
- `migrate status` has a new state, `disabled`.
