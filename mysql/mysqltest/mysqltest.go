// Package mysqltest gives each test its own empty MySQL or MariaDB database.
//
// Tests use the server in FORGE_TEST_MYSQL_URL, or FORGE_TEST_DATABASE_URL
// when it is a mysql:// URL, for example mysql://root:secret@127.0.0.1:3306/mysql.
// The URL names any database the user can connect to (information_schema
// when omitted), and the user needs permission to create and drop databases.
// Without a server tests are skipped, unless FORGE_TEST_REQUIRE_DATABASE=true
// turns the skip into a failure (see dbtest).
package mysqltest

import (
	"context"
	"io/fs"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/dbtest"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/mysql"
	"github.com/rocketinsidecorp/forge-go/sqldb"
)

// safeName is what dbtest.DatabaseName produces; anything else is refused
// before it reaches SQL.
var safeName = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// New creates a fresh database, returns a pool connected to it and drops the
// database when the test ends. Parallel tests are fully isolated.
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
		return mysql.NewMigrator(cfg, loaded, logger)
	})
	return db
}

// Config returns configuration for a fresh, empty database, for tests of
// code that opens its own pool (for example Configure).
func Config(t testing.TB) config.Database {
	t.Helper()
	db, cfg := NewWithConfig(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close test pool: %v", err)
	}
	return cfg
}

// NewWithConfig is New that also returns the database's configuration.
func NewWithConfig(t testing.TB) (*sqldb.DB, config.Database) {
	t.Helper()
	base := dbtest.ServerURL(t, config.AdapterMySQL)
	serverURL, err := url.Parse(base)
	if err != nil {
		t.Fatalf("%s is not a valid URL", dbtest.EnvAdapterURL(config.AdapterMySQL)) // not wrapped: may contain a password
	}
	adminURL := *serverURL
	if strings.Trim(adminURL.Path, "/") == "" {
		adminURL.Path, adminURL.RawPath = "/information_schema", ""
	}

	name := dbtest.DatabaseName()
	if !safeName.MatchString(name) {
		t.Fatalf("unsafe test database name %q", name)
	}
	identifier := "`" + name + "`"
	ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
	defer cancel()
	admin, err := openAdmin(ctx, adminURL.String())
	if err != nil {
		t.Fatalf("connect to the MySQL test server: %v", err) // Open errors never contain the password
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+identifier+" CHARACTER SET utf8mb4"); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	// Registered before anything else can fail, so the database is always
	// dropped. Cleanups run last-in first-out: the pool closes first.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
		defer cancel()
		admin, err := openAdmin(ctx, adminURL.String())
		if err != nil {
			t.Errorf("drop test database: %v", err)
			return
		}
		defer func() { _ = admin.Close() }()
		if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+identifier); err != nil {
			t.Errorf("drop test database: %v", err)
		}
	})

	testURL := *serverURL
	testURL.Path, testURL.RawPath = "/"+name, ""
	cfg := config.Default().Database
	cfg.URL = config.NewSecret(testURL.String())
	db, err := mysql.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, cfg
}

// openAdmin connects to the server for creating and dropping databases.
func openAdmin(ctx context.Context, rawURL string) (*sqldb.DB, error) {
	cfg := config.Default().Database
	cfg.URL = config.NewSecret(rawURL)
	cfg.MaxConns = 1
	return mysql.Open(ctx, cfg)
}
