package postgres_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	forge "github.com/rocketinsidecorp/forge-go"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/migrate/migratetest"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

func TestFrameworkMigrationOwnership(t *testing.T) {
	t.Parallel()
	migratetest.RunFrameworkOwnership(t, func(t testing.TB) migratetest.Database {
		db, cfg := postgrestest.NewWithConfig(t)
		return migratetest.Database{
			NewMigrator: func(migrations []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
				return postgres.NewMigrator(cfg, migrations, logger)
			},
			Exec: func(ctx context.Context, query string) error {
				_, err := db.Exec(ctx, query)
				return err
			},
			TableExists: func(ctx context.Context, table string) (bool, error) {
				var exists bool
				err := db.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists)
				return exists, err
			},
		}
	})
}

// TestRollbackKeepsIdentityTables is the generated application's composition:
// the shipped identity set with an application migration, through the
// application's commands. Rollback reverts the application migration and
// never the identity schema.
func TestRollbackKeepsIdentityTables(t *testing.T) {
	t.Parallel()
	db, cfg := postgrestest.NewWithConfig(t)
	lookup := func(key string) (string, bool) {
		switch key {
		case "FORGE_ENV":
			return "test", true
		case "FORGE_DATABASE_URL":
			return cfg.URL.Reveal(), true
		}
		return "", false
	}
	app := fstest.MapFS{
		"20260101000000_create_notes.up.sql":   {Data: []byte("CREATE TABLE notes (id uuid PRIMARY KEY);")},
		"20260101000000_create_notes.down.sql": {Data: []byte("DROP TABLE notes;")},
	}
	noop := func(context.Context, *forge.App) error { return nil }
	commands := postgres.CommandsFromSets(authpostgres.Migrations(), app)
	run := func(args ...string) (string, string, int) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := forge.Execute(context.Background(), args, lookup, &stdout, &stderr, noop, commands...)
		return stdout.String(), stderr.String(), code
	}
	if out, errOut, code := run("migrate"); code != 0 || !strings.Contains(out, "applied  20260101000000_create_notes") {
		t.Fatalf("migrate: code=%d %s %s", code, out, errOut)
	}
	if out, errOut, code := run("rollback", "-steps", "50"); code != 0 || strings.Count(out, "reverted 20") != 1 || !strings.Contains(out, "reverted 20260101000000_create_notes") {
		t.Fatalf("rollback: code=%d %s %s", code, out, errOut)
	}
	out, errOut, code := run("rollback")
	if code != 1 || strings.Contains(out, "reverted 20") || !strings.Contains(errOut, "from the auth/postgres package") {
		t.Fatalf("rollback with only framework migrations: code=%d stdout=%s stderr=%s", code, out, errOut)
	}
	var users bool
	if err := db.QueryRow(context.Background(), "SELECT to_regclass('forge_users') IS NOT NULL").Scan(&users); err != nil || !users {
		t.Fatalf("identity table forge_users after rollback: %v %v", users, err)
	}
}

// The identity migrations dropped DEFAULT uuidv7() before any release, so
// they apply on PostgreSQL 16 and 17. A database that applied the earlier
// text (recorded under its checksum) still migrates and reports every
// migration applied; any other checksum is still a modified migration.
func TestIdentityMigrationsAppliedWithTheUUIDv7DefaultsStillVerify(t *testing.T) {
	t.Parallel()
	db, cfg := postgrestest.NewWithConfig(t)
	ctx := context.Background()
	loaded, err := migrate.LoadMigrationSets(authpostgres.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := postgres.NewMigrator(cfg, loaded, discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if _, err := m.Up(ctx); err != nil {
		t.Fatalf("the identity migrations on this server: %v", err)
	}
	var defaults int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_name LIKE 'forge\\_%' AND column_name = 'id' AND column_default IS NOT NULL").Scan(&defaults); err != nil {
		t.Fatal(err)
	}
	if defaults != 0 {
		t.Fatalf("%d forge_ id columns still have a database default; IDs come from the application", defaults)
	}
	record := func(checksum string) {
		t.Helper()
		if _, err := db.Exec(ctx, "UPDATE "+migrate.Table+" SET checksum = $1 WHERE version = 20260927090000", checksum); err != nil {
			t.Fatal(err)
		}
	}
	// What a database migrated before the change recorded.
	record("97b2e99f3157ead576f950a2dbb031e3d7df41d559afd884fe9e69023481efde")
	if _, err := m.Up(ctx); err != nil {
		t.Fatalf("a database that applied the earlier identity migration: %v", err)
	}
	statuses, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range statuses {
		if status.State != migrate.StateApplied {
			t.Errorf("%d_%s is %s, want applied", status.Version, status.Name, status.State)
		}
	}
	// The earlier checksum excuses only the script Forge ships now: a build
	// whose identity migration was tampered with is still refused.
	tampered := slices.Clone(loaded)
	for i := range tampered {
		if tampered[i].Version == 20260927090000 {
			tampered[i].Up.SQL += "\nCREATE TABLE forge_backdoor (id int);"
			sum := sha256.Sum256([]byte(tampered[i].Up.SQL))
			tampered[i].Checksum = hex.EncodeToString(sum[:])
		}
	}
	forged, err := postgres.NewMigrator(cfg, tampered, discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = forged.Close() }()
	if _, err := forged.Up(ctx); err == nil || !strings.Contains(err.Error(), "was modified after it was applied") {
		t.Fatalf("a tampered build over a database with the earlier checksum: err = %v, want the modified-migration refusal", err)
	}
	record(strings.Repeat("0", 64))
	if _, err := m.Up(ctx); err == nil || !strings.Contains(err.Error(), "was modified after it was applied") {
		t.Fatalf("a checksum Forge never shipped: err = %v, want the modified-migration refusal", err)
	}
}
