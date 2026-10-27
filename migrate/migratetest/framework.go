// Package migratetest is the conformance suite for the migration engine's
// framework-migration ownership rules (ADR 0039), run by every database
// adapter against a real database:
//
//	func TestFrameworkMigrationOwnership(t *testing.T) {
//	    migratetest.RunFrameworkOwnership(t, func(t testing.TB) migratetest.Database { ... a fresh, empty database ... })
//	}
package migratetest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rocketinsidecorp/forge-go/migrate"
)

// Database is a fresh, empty database that the test cleans up.
type Database struct {
	// NewMigrator returns a migrator on the database; the suite closes it.
	NewMigrator func(migrations []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error)
	// Exec runs one statement outside the migrator.
	Exec func(ctx context.Context, query string) error
	// TableExists reports whether the table exists.
	TableExists func(ctx context.Context, table string) (bool, error)
}

// NewDatabase returns a fresh, empty database.
type NewDatabase func(t testing.TB) Database

// RunFrameworkOwnership executes the suite.
func RunFrameworkOwnership(t *testing.T, newDatabase NewDatabase) {
	t.Helper()
	tests := []struct {
		name string
		run  func(*testing.T, *suite)
	}{
		{"RollbackRevertsOnlyApplicationMigrations", rollbackRevertsOnlyApplication},
		{"RollbackSkipsAFrameworkSetAddedLater", rollbackSkipsLaterFrameworkSet},
		{"DownFrameworkRevertsOnlyFrameworkMigrations", downFrameworkRevertsOnlyFramework},
		{"DisabledFrameworkSetIsTolerated", disabledFrameworkSetTolerated},
		{"UnknownVersionFailsClosed", unknownVersionFailsClosed},
		{"ReservedVersionIsRefused", reservedVersionRefused},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			test.run(t, &suite{t: t, db: newDatabase(t)})
		})
	}
}

type suite struct {
	t  *testing.T
	db Database
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// frameworkVersion returns the registered version of a framework migration,
// so the fixtures below always use a real reserved version and name.
func frameworkVersion(t *testing.T, owner, name string) int64 {
	t.Helper()
	for _, migration := range migrate.FrameworkMigrations() {
		if migration.Owner == owner && migration.Name == name {
			return migration.Version
		}
	}
	t.Fatalf("framework migration %s %s is not registered", owner, name)
	return 0
}

// Fixture sets. The framework fixtures use registered versions and names with
// test tables: identity is reversible like the real one, cache is
// irreversible like the real one.
func appSet(names ...string) fstest.MapFS {
	all := map[string]string{
		"20260101000000_create_widgets": "widgets", // older than every framework version
		"20261201000000_create_gadgets": "gadgets", // newer than every framework version
		"20260301000000_create_strays":  "strays",
	}
	set := fstest.MapFS{}
	for _, name := range names {
		table, ok := all[name]
		if !ok {
			panic("unknown fixture " + name)
		}
		set[name+".up.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE " + table + " (id BIGINT PRIMARY KEY);\n")}
		set[name+".down.sql"] = &fstest.MapFile{Data: []byte("DROP TABLE " + table + ";\n")}
	}
	return set
}

func identitySet(t *testing.T) fstest.MapFS {
	version := frameworkVersion(t, "auth", "identity")
	return fstest.MapFS{
		fmt.Sprintf("%d_identity.up.sql", version):   {Data: []byte("CREATE TABLE forge_test_identity (id BIGINT PRIMARY KEY);\n")},
		fmt.Sprintf("%d_identity.down.sql", version): {Data: []byte("DROP TABLE forge_test_identity;\n")},
	}
}

func cacheSet(t *testing.T) fstest.MapFS {
	version := frameworkVersion(t, "cache", "cache")
	return fstest.MapFS{
		fmt.Sprintf("%d_cache.up.sql", version): {Data: []byte("CREATE TABLE forge_test_cache (id BIGINT PRIMARY KEY);\n")},
	}
}

func (s *suite) migrator(sets ...fs.FS) *migrate.Migrator {
	s.t.Helper()
	migrations, err := migrate.LoadMigrationSets(sets...)
	if err != nil {
		s.t.Fatalf("load migration sets: %v", err)
	}
	migrator, err := s.db.NewMigrator(migrations, discard)
	if err != nil {
		s.t.Fatalf("open migrator: %v", err)
	}
	s.t.Cleanup(func() { _ = migrator.Close() })
	return migrator
}

func (s *suite) up(migrator *migrate.Migrator, want int) {
	s.t.Helper()
	applied, err := migrator.Up(context.Background())
	if err != nil || len(applied) != want {
		s.t.Fatalf("Up applied %v, want %d: %v", names(applied), want, err)
	}
}

// tables checks which of the fixture tables exist.
func (s *suite) tables(want ...string) {
	s.t.Helper()
	var got []string
	for _, table := range []string{"forge_test_cache", "forge_test_identity", "gadgets", "strays", "widgets"} {
		exists, err := s.db.TableExists(context.Background(), table)
		if err != nil {
			s.t.Fatalf("table %s: %v", table, err)
		}
		if exists {
			got = append(got, table)
		}
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		s.t.Fatalf("tables %v, want %v", got, want)
	}
}

func names(migrations []migrate.Migration) []string {
	var out []string
	for _, migration := range migrations {
		out = append(out, migration.Name)
	}
	return out
}

func states(t *testing.T, migrator *migrate.Migrator) map[string]migrate.MigrationState {
	t.Helper()
	statuses, err := migrator.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	out := make(map[string]migrate.MigrationState, len(statuses))
	for _, status := range statuses {
		out[status.Name] = status.State
	}
	return out
}

func frameworkOnly(t *testing.T, err error, wantName string) {
	t.Helper()
	var only *migrate.FrameworkMigrationOnlyError
	if !errors.As(err, &only) {
		t.Fatalf("want *migrate.FrameworkMigrationOnlyError, got %v", err)
	}
	if only.Newest.Name != wantName || !strings.Contains(err.Error(), "DownFramework") {
		t.Fatalf("newest framework migration %+v, want %s: %v", only.Newest, wantName, err)
	}
}

// The application's versions are older and newer than the framework's, and
// every set is applied in one run: rollback reverts the application
// migrations in reverse order of application, skipping over the framework
// ones (including an irreversible one, which used to block it), then refuses.
func rollbackRevertsOnlyApplication(t *testing.T, s *suite) {
	ctx := context.Background()
	migrator := s.migrator(appSet("20260101000000_create_widgets", "20261201000000_create_gadgets"), identitySet(t), cacheSet(t))
	s.up(migrator, 4)

	reverted, err := migrator.Down(ctx, 10)
	if err != nil || !slices.Equal(names(reverted), []string{"create_gadgets", "create_widgets"}) {
		t.Fatalf("Down reverted %v: %v", names(reverted), err)
	}
	s.tables("forge_test_cache", "forge_test_identity")

	reverted, err = migrator.Down(ctx, 1)
	if len(reverted) != 0 {
		t.Fatalf("Down reverted %v with only framework migrations left", names(reverted))
	}
	frameworkOnly(t, err, "cache")
	s.tables("forge_test_cache", "forge_test_identity")
	got := states(t, migrator)
	if got["identity"] != migrate.StateApplied || got["cache"] != migrate.StateApplied || got["create_widgets"] != migrate.StatePending {
		t.Fatalf("status after rollback: %v", got)
	}

	// A migrator that does not contain the applied framework migrations
	// has none to revert.
	appOnly := s.migrator(appSet("20260101000000_create_widgets"))
	if reverted, err := appOnly.DownFramework(ctx, 1); err != nil || len(reverted) != 0 {
		t.Fatalf("DownFramework without contained framework migrations: %v %v", names(reverted), err)
	}
}

// The issue's case: a framework set composed after the application's
// migrations is applied last, yet rollback reverts the application's.
func rollbackSkipsLaterFrameworkSet(t *testing.T, s *suite) {
	ctx := context.Background()
	if reverted, err := s.migrator(appSet("20261201000000_create_gadgets")).Down(ctx, 1); err != nil || len(reverted) != 0 {
		t.Fatalf("Down on an empty database: %v %v", names(reverted), err)
	}
	s.up(s.migrator(appSet("20261201000000_create_gadgets")), 1)
	migrator := s.migrator(appSet("20261201000000_create_gadgets"), identitySet(t))
	s.up(migrator, 1)

	reverted, err := migrator.Down(ctx, 1)
	if err != nil || !slices.Equal(names(reverted), []string{"create_gadgets"}) {
		t.Fatalf("Down reverted %v: %v", names(reverted), err)
	}
	s.tables("forge_test_identity")
	_, err = migrator.Down(ctx, 1)
	frameworkOnly(t, err, "identity")
}

func downFrameworkRevertsOnlyFramework(t *testing.T, s *suite) {
	ctx := context.Background()
	migrator := s.migrator(appSet("20260101000000_create_widgets", "20261201000000_create_gadgets"), identitySet(t))
	s.up(migrator, 3)

	reverted, err := migrator.DownFramework(ctx, 5)
	if err != nil || !slices.Equal(names(reverted), []string{"identity"}) {
		t.Fatalf("DownFramework reverted %v: %v", names(reverted), err)
	}
	s.tables("gadgets", "widgets")
	if reverted, err := migrator.DownFramework(ctx, 1); err != nil || len(reverted) != 0 {
		t.Fatalf("DownFramework with no framework migration applied: %v %v", names(reverted), err)
	}
	if reverted, err := migrator.Down(ctx, 1); err != nil || !slices.Equal(names(reverted), []string{"create_gadgets"}) {
		t.Fatalf("Down after DownFramework reverted %v: %v", names(reverted), err)
	}
	if _, err := migrator.DownFramework(ctx, 0); err == nil {
		t.Fatal("DownFramework accepted 0 steps")
	}

	// An irreversible framework migration is refused before anything is
	// reverted.
	withCache := s.migrator(appSet("20260101000000_create_widgets", "20261201000000_create_gadgets"), identitySet(t), cacheSet(t))
	s.up(withCache, 3)
	reverted, err = withCache.DownFramework(ctx, 2)
	if err == nil || !strings.Contains(err.Error(), "irreversible") || len(reverted) != 0 {
		t.Fatalf("DownFramework over an irreversible migration: %v %v", names(reverted), err)
	}
	s.tables("forge_test_cache", "forge_test_identity", "gadgets", "widgets")
}

// A framework set the build no longer composes stays applied and untouched,
// is reported as disabled, and is verified again once composed again.
func disabledFrameworkSetTolerated(t *testing.T, s *suite) {
	ctx := context.Background()
	s.up(s.migrator(appSet("20260101000000_create_widgets"), identitySet(t), cacheSet(t)), 3)

	appOnly := s.migrator(appSet("20260101000000_create_widgets"))
	s.up(appOnly, 0)
	got := states(t, appOnly)
	if got["identity"] != migrate.StateDisabled || got["cache"] != migrate.StateDisabled || got["create_widgets"] != migrate.StateApplied {
		t.Fatalf("status with the framework sets disabled: %v", got)
	}
	if reverted, err := appOnly.DownFramework(ctx, 5); err != nil || len(reverted) != 0 {
		t.Fatalf("DownFramework reverted a migration the migrator does not contain: %v %v", names(reverted), err)
	}
	if reverted, err := appOnly.Down(ctx, 5); err != nil || !slices.Equal(names(reverted), []string{"create_widgets"}) {
		t.Fatalf("Down reverted %v: %v", names(reverted), err)
	}
	_, err := appOnly.Down(ctx, 1)
	frameworkOnly(t, err, "cache")
	s.tables("forge_test_cache", "forge_test_identity")

	// Composed again: already applied, so only the application migration runs.
	enabled := s.migrator(appSet("20260101000000_create_widgets"), identitySet(t), cacheSet(t))
	if applied, err := enabled.Up(ctx); err != nil || !slices.Equal(names(applied), []string{"create_widgets"}) {
		t.Fatalf("Up with the sets enabled again applied %v: %v", names(applied), err)
	}
	// ...and its checksum is verified again.
	edited := identitySet(t)
	for name, file := range edited {
		if strings.HasSuffix(name, ".up.sql") {
			edited[name] = &fstest.MapFile{Data: append(slices.Clone(file.Data), []byte("-- edited\n")...)}
		}
	}
	if _, err := s.migrator(appSet("20260101000000_create_widgets"), edited, cacheSet(t)).Up(ctx); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("a modified framework migration was accepted: %v", err)
	}
}

// A version that is neither in the build nor a framework migration still
// stops every change.
func unknownVersionFailsClosed(t *testing.T, s *suite) {
	ctx := context.Background()
	s.up(s.migrator(appSet("20260101000000_create_widgets", "20260301000000_create_strays")), 2)
	migrator := s.migrator(appSet("20260101000000_create_widgets"))
	if _, err := migrator.Up(ctx); err == nil || !strings.Contains(err.Error(), "does not contain") {
		t.Fatalf("Up with an unknown version: %v", err)
	}
	if _, err := migrator.Down(ctx, 1); err == nil || !strings.Contains(err.Error(), "does not contain") {
		t.Fatalf("Down with an unknown version: %v", err)
	}
	if _, err := migrator.DownFramework(ctx, 1); err == nil || !strings.Contains(err.Error(), "does not contain") {
		t.Fatalf("DownFramework with an unknown version: %v", err)
	}
	if got := states(t, migrator); got["create_strays"] != migrate.StateUnknown {
		t.Fatalf("status of an unknown version: %v", got)
	}
	s.tables("strays", "widgets")
}

// An application migration may not take a framework version, in the build or
// recorded in the database.
func reservedVersionRefused(t *testing.T, s *suite) {
	ctx := context.Background()
	reserved := frameworkVersion(t, "cache", "cache")
	clash := fstest.MapFS{
		fmt.Sprintf("%d_create_widgets.up.sql", reserved): {Data: []byte("CREATE TABLE widgets (id BIGINT PRIMARY KEY);\n")},
	}
	var reservedErr *migrate.ReservedVersionError
	if _, err := migrate.LoadMigrationSets(clash); !errors.As(err, &reservedErr) || reservedErr.Reserved.Name != "cache" {
		t.Fatalf("LoadMigrationSets accepted a reserved version: %v", err)
	}
	loaded, err := migrate.LoadMigrations(clash)
	if err != nil {
		t.Fatal(err)
	}
	if migrator, err := s.db.NewMigrator(loaded, discard); !errors.As(err, &reservedErr) {
		if migrator != nil {
			_ = migrator.Close()
		}
		t.Fatalf("NewMigrator accepted a reserved version: %v", err)
	}

	// A row recorded under a reserved version by another migration (before
	// versions were reserved) is not the framework's: refused, not tolerated.
	migrator := s.migrator(appSet("20260101000000_create_widgets"))
	s.up(migrator, 1)
	row := fmt.Sprintf("INSERT INTO %s (version, name, checksum, applied_at) VALUES (%d, 'create_gadgets', '%s', 1)",
		migrate.Table, reserved, strings.Repeat("0", 64))
	if err := s.db.Exec(ctx, row); err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.Up(ctx); !errors.As(err, &reservedErr) || reservedErr.Name != "create_gadgets" {
		t.Fatalf("Up accepted a reserved version recorded by another migration: %v", err)
	}
	if _, err := migrator.Down(ctx, 1); !errors.As(err, &reservedErr) {
		t.Fatalf("Down accepted a reserved version recorded by another migration: %v", err)
	}
	if got := states(t, migrator); got["create_gadgets"] != migrate.StateUnknown {
		t.Fatalf("status of a reserved version recorded by another migration: %v", got)
	}
	s.tables("widgets")
}
