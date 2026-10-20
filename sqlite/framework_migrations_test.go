package sqlite_test

import (
	"bytes"
	"context"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	forge "github.com/rocketinsidecorp/forge-go"
	cachesqlite "github.com/rocketinsidecorp/forge-go/cache/sqlite"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/migrate/migratetest"
	queuesqlite "github.com/rocketinsidecorp/forge-go/queue/sqlite"
	ratelimitsqlite "github.com/rocketinsidecorp/forge-go/ratelimit/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
)

func TestFrameworkMigrationOwnership(t *testing.T) {
	t.Parallel()
	migratetest.RunFrameworkOwnership(t, func(t testing.TB) migratetest.Database {
		db, cfg := sqlitetest.NewWithConfig(t)
		return migratetest.Database{
			NewMigrator: func(migrations []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
				return sqlite.NewMigrator(cfg, migrations, logger)
			},
			Exec: func(ctx context.Context, query string) error {
				_, err := db.ExecContext(ctx, query)
				return err
			},
			TableExists: func(ctx context.Context, table string) (bool, error) {
				var count int
				err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&count)
				return count == 1, err
			},
		}
	})
}

// TestRollbackKeepsRealFrameworkSets composes the shipped SQLite framework
// sets (all irreversible) with an application migration whose version is
// older than theirs, through the application's commands.
func TestRollbackKeepsRealFrameworkSets(t *testing.T) {
	t.Parallel()
	db, cfg := sqlitetest.NewWithConfig(t)
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
		"20260101000000_create_notes.up.sql":   {Data: []byte("CREATE TABLE notes (id TEXT PRIMARY KEY);")},
		"20260101000000_create_notes.down.sql": {Data: []byte("DROP TABLE notes;")},
	}
	noop := func(context.Context, *forge.App) error { return nil }
	run := func(sets []fs.FS, args ...string) (string, string, int) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := forge.Execute(context.Background(), args, lookup, &stdout, &stderr, noop, sqlite.CommandsFromSets(sets...)...)
		return stdout.String(), stderr.String(), code
	}
	all := []fs.FS{app, queuesqlite.Migrations(), ratelimitsqlite.Migrations(), cachesqlite.Migrations()}

	if out, errOut, code := run(all, "migrate"); code != 0 || strings.Count(out, "applied  ") != 4 {
		t.Fatalf("migrate: code=%d %s %s", code, out, errOut)
	}
	if out, errOut, code := run(all, "rollback"); code != 0 || !strings.Contains(out, "reverted 20260101000000_create_notes") {
		t.Fatalf("rollback: code=%d %s %s", code, out, errOut)
	}
	out, errOut, code := run(all, "rollback")
	if code != 1 || strings.Contains(out, "reverted 20") || !strings.Contains(errOut, "newest 20261019000100_cache, from the cache package") {
		t.Fatalf("rollback with only framework migrations: code=%d stdout=%q stderr=%s", code, out, errOut)
	}
	for _, table := range []string{"forge_jobs", "forge_rate_limits", "forge_cache_entries"} {
		var count int
		if err := db.QueryRowContext(context.Background(), "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("framework table %s: count=%d err=%v", table, count, err)
		}
	}

	// Turning the cache off no longer breaks migrate; status shows it.
	withoutCache := []fs.FS{app, queuesqlite.Migrations(), ratelimitsqlite.Migrations()}
	if out, errOut, code := run(withoutCache, "migrate"); code != 0 || !strings.Contains(out, "applied  20260101000000_create_notes") {
		t.Fatalf("migrate without the cache set: code=%d %s %s", code, out, errOut)
	}
	if out, errOut, code := run(withoutCache, "migrate", "status"); code != 0 || !strings.Contains(out, "disabled  20261019000100_cache") {
		t.Fatalf("status without the cache set: code=%d %s %s", code, out, errOut)
	}
}
