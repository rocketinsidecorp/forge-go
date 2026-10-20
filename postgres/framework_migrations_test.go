package postgres_test

import (
	"bytes"
	"context"
	"log/slog"
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
