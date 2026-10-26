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
	{Version: 20261026120000, Name: "job_schedules", Owner: "jobs"},
}

// supersededScript pins, for a framework migration edited while it was
// unreleased (no version tag included it), the checksum of the up script
// Forge now ships (current) and of the texts it shipped before (old). A
// database row recorded under an old checksum is accepted only while the
// build carries exactly the current script: a build whose text differs from
// both is still a modified migration.
type supersededScript struct {
	current string
	old     []string
}

// supersededChecksums lists the framework migrations edited before any
// release, by version. A database that applied an earlier text is still
// accepted: Forge wrote both, and the edit changed nothing a database that
// already ran the old text needs. Never add a released migration here:
// released history only grows (add a migration).
var supersededChecksums = map[int64]supersededScript{
	// The identity tables declared DEFAULT uuidv7(), which PostgreSQL 16 and
	// 17 lack; the application has always been able to supply the IDs, and
	// now always does (forge-go QG9). A database migrated before keeps the
	// unused defaults.
	20260927090000: {
		current: "da89667ae1aec4fdb3cea32b30eca71646ba278faf613ff5ac664f77b2d4e83c",
		old:     []string{"97b2e99f3157ead576f950a2dbb031e3d7df41d559afd884fe9e69023481efde"},
	},
	20260927142000: {
		current: "d5c8b501d651dfa7621c42982ef03573d45e65d81cbc841ce9dfdec060da1687",
		old:     []string{"943bc72bacf06d89a307c50835d79fab308c8051802af6bf60e3b8777a55b917"},
	},
	20260927200000: {
		current: "93d700f21f7fb368c47ddb390a5a13fdc57927e3817e7f419060fc4b7ceee661",
		old:     []string{"d0d4b601e34a2f6d9bf2275a63d97eea1c3300aba116f2171bbaaba4321622a0"},
	},
	20260927203000: {
		current: "952786be96ffc614a0a4b283832a592138bf0dc80b68e1771c5d6cf47059ed4c",
		old:     []string{"af1ae8e3bdfc8cadb3a4ffb8b2ccc01d551a3b9473cf0e9052e96bdb3a43b443"},
	},
	20260928004500: {
		current: "779fe82ac020a94db9a3e73c49837bf88cc5059888035b82aa8ed839f5ea22e8",
		old:     []string{"b0e756ab61753decd2b31205375ca607fab8faa1beab1f88fc7bb3940696f79e"},
	},
}

// checksumMatches reports whether a database row recorded with checksum
// applied migration: the current script, or, for a framework migration
// recorded under its own name whose build carries exactly the pinned
// current script, an earlier text in supersededChecksums.
func checksumMatches(migration Migration, recordedName, checksum string) bool {
	if checksum == migration.Checksum {
		return true
	}
	framework, reserved := frameworkMigration(migration.Version)
	if !reserved || framework.Name != migration.Name || recordedName != framework.Name {
		return false
	}
	superseded, ok := supersededChecksums[migration.Version]
	return ok && migration.Checksum == superseded.current && slices.Contains(superseded.old, checksum)
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
