// Package migrate is Forge's database-independent migration engine: SQL
// files embedded in the application binary, applied in version order under a
// cross-process lock, recorded with checksums, and refused whenever the
// database and the build disagree (ADR 0008). Adapters supply a Dialect.
package migrate

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// NoTransactionDirective, as the first line of a migration file, runs that
// file outside a transaction. PostgreSQL requires this for statements such as
// CREATE INDEX CONCURRENTLY. Keep such files to a single statement: a failure
// part-way cannot be rolled back.
const NoTransactionDirective = "-- forge:no-transaction"

// migrationFileName matches <14-digit UTC timestamp>_<snake_name>.<up|down>.sql.
var migrationFileName = regexp.MustCompile(`^(\d{14})_([a-z0-9_]+)\.(up|down)\.sql$`)

// Script is one direction of a migration.
type Script struct {
	SQL  string
	NoTx bool
}

// Migration is a versioned schema change loaded from SQL files.
type Migration struct {
	Version int64
	Name    string
	Up      Script
	// Down is empty for irreversible migrations; rolling them back fails.
	Down Script
	// Checksum is the SHA-256 of the up script. Editing an applied migration
	// changes it, and the migrator then refuses to run.
	Checksum string
}

// MigrationState is the status of one migration.
type MigrationState string

const (
	StateApplied MigrationState = "applied"
	StatePending MigrationState = "pending"
	// StateModified means the file changed after the migration was applied.
	StateModified MigrationState = "modified"
	// StateUnknown means the database has a migration this build lacks;
	// migrate and rollback refuse to run.
	StateUnknown MigrationState = "unknown"
	// StateDisabled means the database has a framework migration whose set
	// this build does not compose (its feature was turned off). It is
	// tolerated: it stays applied, and composing the set again verifies it.
	StateDisabled MigrationState = "disabled"
)

type MigrationStatus struct {
	Version   int64
	Name      string
	State     MigrationState
	AppliedAt time.Time
}

// LoadMigrations reads migrations from the root of fsys. Files other than
// *.sql are ignored; *.sql files that do not follow the naming convention are
// an error, so a typo cannot silently skip a migration.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	byVersion := make(map[int64]*Migration)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		match := migrationFileName.FindStringSubmatch(entry.Name())
		if match == nil {
			return nil, fmt.Errorf("migration %s: name must be <YYYYMMDDHHMMSS>_<snake_case_name>.<up|down>.sql", entry.Name())
		}
		version, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", entry.Name(), err)
		}
		content, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		script := parseScript(string(content))
		if !hasStatements(script.SQL) {
			// Catches freshly generated files: applying one would record it
			// as done before the SQL was written.
			return nil, fmt.Errorf("migration %s has no SQL statements", entry.Name())
		}

		migration := byVersion[version]
		if migration == nil {
			migration = &Migration{Version: version, Name: match[2]}
			byVersion[version] = migration
		}
		if migration.Name != match[2] {
			return nil, fmt.Errorf("migration version %d is used by both %q and %q", version, migration.Name, match[2])
		}
		target := &migration.Up
		if match[3] == "down" {
			target = &migration.Down
		}
		if target.SQL != "" {
			return nil, fmt.Errorf("migration %s is defined twice", entry.Name())
		}
		*target = script
	}

	migrations := make([]Migration, 0, len(byVersion))
	for _, migration := range byVersion {
		if migration.Up.SQL == "" {
			return nil, fmt.Errorf("migration %d_%s has a down script but no up script", migration.Version, migration.Name)
		}
		sum := sha256.Sum256([]byte(migration.Up.SQL))
		migration.Checksum = hex.EncodeToString(sum[:])
		migrations = append(migrations, *migration)
	}
	slices.SortFunc(migrations, func(a, b Migration) int { return cmp.Compare(a.Version, b.Version) })
	return migrations, nil
}

// LoadMigrationSets loads and merges several independent migration filesystems.
// Every set is validated with LoadMigrations first. Versions must be globally
// unique across all sets so framework and application migrations cannot
// silently shadow or reorder each other, and a framework migration's version
// is reserved for it (*ReservedVersionError).
func LoadMigrationSets(sets ...fs.FS) ([]Migration, error) {
	if len(sets) == 0 {
		return nil, nil
	}
	merged := make([]Migration, 0)
	seen := make(map[int64]Migration)
	for index, set := range sets {
		if set == nil {
			return nil, fmt.Errorf("migration set %d is nil", index+1)
		}
		loaded, err := LoadMigrations(set)
		if err != nil {
			return nil, fmt.Errorf("migration set %d: %w", index+1, err)
		}
		for _, migration := range loaded {
			if existing, ok := seen[migration.Version]; ok {
				return nil, fmt.Errorf(
					"migration version %d is used by both %q and %q across migration sets",
					migration.Version, existing.Name, migration.Name,
				)
			}
			seen[migration.Version] = migration
			merged = append(merged, migration)
		}
	}
	if err := checkReservedVersions(merged); err != nil {
		return nil, err
	}
	slices.SortFunc(merged, func(a, b Migration) int { return cmp.Compare(a.Version, b.Version) })
	return merged, nil
}

// hasStatements reports whether sql contains anything besides whitespace and
// "--" line comments.
func hasStatements(sql string) bool {
	for line := range strings.Lines(sql) {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			return true
		}
	}
	return false
}

func parseScript(content string) Script {
	firstLine, _, _ := strings.Cut(strings.TrimLeft(content, " \t\r\n"), "\n")
	return Script{SQL: content, NoTx: strings.TrimSpace(firstLine) == NoTransactionDirective}
}
