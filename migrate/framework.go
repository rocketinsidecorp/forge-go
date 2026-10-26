package migrate

import (
	"cmp"
	"fmt"
	"slices"
)

// FrameworkMigration identifies a migration that a Forge package ships for
// the application's own forge_schema_migrations table, composed through
// CommandsFromSets or LoadMigrationSets (ADR 0039).
//
// Its version is reserved: an application migration with that version and
// another name is refused. An applied framework migration is never reverted
// by the application rollback (Migrator.Down), only by Migrator.DownFramework,
// and it is tolerated when the application no longer composes its set.
type FrameworkMigration struct {
	// Version is the same on every database backend.
	Version int64
	// Name is the file name part after the version (identity for
	// 20260927090000_identity.up.sql).
	Name string
	// Owner is the module-relative Forge package that ships it; for a
	// feature with one adapter per database it is the feature package
	// (ratelimit for ratelimit/postgres, ratelimit/mysql, ratelimit/sqlite).
	Owner string
}

// frameworkMigrations is every migration a Forge package ships. Released
// entries never change; TestFrameworkMigrationsMatchShippedFiles keeps the
// list and the migration files in the repository in sync.
var frameworkMigrations = []FrameworkMigration{
	{Version: 20260927090000, Name: "identity", Owner: "auth/postgres"},
	{Version: 20260927124500, Name: "login_throttle", Owner: "auth/postgres"},
	{Version: 20260927142000, Name: "password_recovery", Owner: "auth/postgres"},
	{Version: 20260927200000, Name: "totp_mfa", Owner: "auth/postgres"},
	{Version: 20260927203000, Name: "mfa_backup_codes", Owner: "auth/postgres"},
	{Version: 20260928004500, Name: "security_audit", Owner: "auth/postgres"},
	{Version: 20261020120000, Name: "login_throttle_pair_scope", Owner: "auth/postgres"},
	{Version: 20261026090000, Name: "session_idle_timeout", Owner: "auth/postgres"},
	{Version: 20261003200000, Name: "outbox", Owner: "outbox/postgres"},
	{Version: 20261003210000, Name: "dispatch", Owner: "outbox/postgres"},
	{Version: 20261003220000, Name: "dispatch_indexes", Owner: "outbox/postgres"},
	{Version: 20261003230000, Name: "dispatch_diagnostics_index", Owner: "outbox/postgres"},
	{Version: 20261003240000, Name: "dispatch_pending_age_index", Owner: "outbox/postgres"},
	{Version: 20261004230000, Name: "agent_executions", Owner: "agent/postgres"},
	{Version: 20261014000000, Name: "jobs", Owner: "queue"},
	{Version: 20261019000000, Name: "rate_limits", Owner: "ratelimit"},
	{Version: 20261019000100, Name: "cache", Owner: "cache"},
}

// FrameworkMigrations returns every registered framework migration in
// version order. The result is a copy.
func FrameworkMigrations() []FrameworkMigration {
	list := slices.Clone(frameworkMigrations)
	slices.SortFunc(list, func(a, b FrameworkMigration) int { return cmp.Compare(a.Version, b.Version) })
	return list
}

// frameworkMigration returns the framework migration registered for version.
func frameworkMigration(version int64) (FrameworkMigration, bool) {
	for _, migration := range frameworkMigrations {
		if migration.Version == version {
			return migration, true
		}
	}
	return FrameworkMigration{}, false
}

// FrameworkMigrationOnlyError is returned by Migrator.Down when every applied
// migration belongs to Forge: the application rollback has nothing of the
// application's own to revert and never reverts a framework migration.
type FrameworkMigrationOnlyError struct {
	// Newest is the most recently applied framework migration.
	Newest FrameworkMigration
}

func (e *FrameworkMigrationOnlyError) Error() string {
	return fmt.Sprintf("nothing to roll back: every applied migration belongs to Forge (newest %d_%s, from the %s package), "+
		"and rollback never reverts framework migrations; revert one deliberately with migrate.Migrator.DownFramework",
		e.Newest.Version, e.Newest.Name, e.Newest.Owner)
}

// ReservedVersionError is returned when a migration, in the build or recorded
// in the database, uses a framework migration's version under another name.
// Ownership is decided by version, so it would otherwise be decided for the
// wrong migration.
type ReservedVersionError struct {
	Version int64
	// Name is the conflicting migration's name.
	Name string
	// Reserved is the framework migration that owns Version.
	Reserved FrameworkMigration
}

func (e *ReservedVersionError) Error() string {
	return fmt.Sprintf("migration %d_%s uses version %d, which is reserved for the %s migration of the Forge %s package; "+
		"give the application migration another version", e.Version, e.Name, e.Version, e.Reserved.Name, e.Reserved.Owner)
}

// checkReservedVersions refuses a migration that reuses a framework version
// under another name.
func checkReservedVersions(migrations []Migration) error {
	for _, migration := range migrations {
		if reserved, ok := frameworkMigration(migration.Version); ok && migration.Name != reserved.Name {
			return &ReservedVersionError{Version: migration.Version, Name: migration.Name, Reserved: reserved}
		}
	}
	return nil
}
