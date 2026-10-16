// Package sqlitetest gives each test its own empty SQLite database: a fresh
// file in the test's temporary directory, opened with the same settings as
// production (foreign keys, WAL, busy timeout, immediate transactions).
//
// Unlike postgrestest and mysqltest it needs no server, so SQLite tests
// always run and are never skipped.
package sqlitetest

import (
	"context"
	"io/fs"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/dbtest"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/sqlite"
)

// New creates a fresh database and returns a pool connected to it, closed
// when the test ends. Parallel tests are fully isolated.
func New(t testing.TB) *sqldb.DB {
	t.Helper()
	db, _ := NewWithConfig(t)
	return db
}

// NewMigrated is New followed by applying every migration in migrations.
func NewMigrated(t testing.TB, migrations fs.FS) *sqldb.DB {
	t.Helper()
	db, cfg := NewWithConfig(t)
	dbtest.Migrate(t, migrations, func(loaded []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
		return sqlite.NewMigrator(cfg, loaded, logger)
	})
	return db
}

// Config returns configuration for a fresh, empty database file, for tests
// of code that opens its own pool (for example Configure). The file lives in
// t.TempDir, which the test removes when it ends.
func Config(t testing.TB) config.Database {
	t.Helper()
	path := filepath.ToSlash(filepath.Join(t.TempDir(), dbtest.DatabaseName()+".db"))
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	cfg := config.Default().Database
	cfg.URL = config.NewSecret((&url.URL{Scheme: "sqlite", Path: path}).String())
	return cfg
}

// NewWithConfig is New that also returns the database's configuration.
func NewWithConfig(t testing.TB) (*sqldb.DB, config.Database) {
	t.Helper()
	cfg := Config(t)
	ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
	defer cancel()
	db, err := sqlite.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	// Registered after t.TempDir's cleanup, so it runs first: the pool is
	// closed before its directory is removed.
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	return db, cfg
}
