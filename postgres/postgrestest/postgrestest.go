// Package postgrestest gives each test its own empty PostgreSQL database.
//
// Tests use the server in FORGE_TEST_POSTGRES_URL, or FORGE_TEST_DATABASE_URL
// when it is a postgres:// URL. The user needs permission to create
// databases (and roles, for NewRLS). Without a server tests are skipped, unless
// FORGE_TEST_REQUIRE_DATABASE=true turns the skip into a failure (see dbtest).
//
// Forge generates UUIDv7 IDs in Rust, but its identity migrations and many
// applications' tables use the uuidv7() function PostgreSQL 18 added. On an
// older server a test that needs it is skipped with a "requires PostgreSQL 18"
// reason, or fails with it when FORGE_TEST_REQUIRE_DATABASE=true, instead of
// failing on an opaque undefined-function error: NewMigrated, NewMigratedSets
// and NewRLS check the migrations they apply, and a test that calls uuidv7()
// itself calls RequireUUIDv7.
package postgrestest

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/dbtest"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// New creates a fresh database, returns a pool connected to it and drops the
// database when the test ends. Parallel tests are fully isolated.
func New(t testing.TB) *postgres.DB {
	t.Helper()
	db, _ := NewWithConfig(t)
	return db
}

// NewMigrated is New followed by applying every migration in migrations.
func NewMigrated(t testing.TB, migrations fs.FS) *postgres.DB {
	t.Helper()
	db, cfg := NewWithConfig(t)
	if loaded, err := migrate.LoadMigrations(migrations); err == nil {
		requireUUIDv7For(t, db, loaded)
	}
	dbtest.Migrate(t, migrations, func(loaded []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
		return postgres.NewMigrator(cfg, loaded, logger)
	})
	return db
}

// NewMigratedSets is New followed by applying the migrations of several
// independent sets as one globally ordered history, the way the application's
// migrate command composes them (for example the Forge identity migrations
// and the application's own).
func NewMigratedSets(t testing.TB, sets ...fs.FS) *postgres.DB {
	t.Helper()
	db, cfg := NewWithConfig(t)
	loaded, err := migrate.LoadMigrationSets(sets...)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	requireUUIDv7For(t, db, loaded)
	migrator, err := postgres.NewMigrator(cfg, loaded, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open migrator: %v", err)
	}
	defer func() { _ = migrator.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
	defer cancel()
	if _, err := migrator.Up(ctx); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db
}

// minUUIDv7Version is server_version_num of PostgreSQL 18, which added
// uuidv7().
const minUUIDv7Version = 180000

// RequireUUIDv7 ends the test, skipped or (with FORGE_TEST_REQUIRE_DATABASE=true)
// failed, with a "requires PostgreSQL 18" reason when db's server predates
// uuidv7(). Call it first in a test that uses the function directly.
func RequireUUIDv7(t testing.TB, db *postgres.DB) {
	t.Helper()
	var version int
	var name string
	if err := db.QueryRow(context.Background(), "SELECT current_setting('server_version_num')::int, current_setting('server_version')").Scan(&version, &name); err != nil {
		t.Fatalf("read the server version: %v", err)
	}
	if reason := uuidv7Unsupported(version, name); reason != "" {
		dbtest.Unsupported(t, "%s", reason)
	}
}

// uuidv7Unsupported explains why a server of the given version cannot run
// uuidv7(), or returns "" when it can.
func uuidv7Unsupported(version int, name string) string {
	if version >= minUUIDv7Version {
		return ""
	}
	return "requires PostgreSQL 18 (uuidv7()): the test server is PostgreSQL " + name
}

var (
	callsUUIDv7   = regexp.MustCompile(`(?i)\buuidv7\s*\(`)
	definesUUIDv7 = regexp.MustCompile(`(?is)\bcreate\s+(?:or\s+replace\s+)?function\s+(?:[a-z_][a-z0-9_$]*\.)?uuidv7\s*\(`)
)

// needsUUIDv7 reports whether the migrations call uuidv7() without defining
// it themselves (an application may ship a polyfill for older servers).
func needsUUIDv7(loaded []migrate.Migration) bool {
	calls := false
	for _, migration := range loaded {
		script := migration.Up.SQL
		if definesUUIDv7.MatchString(script) {
			return false
		}
		calls = calls || callsUUIDv7.MatchString(script)
	}
	return calls
}

func requireUUIDv7For(t testing.TB, db *postgres.DB, loaded []migrate.Migration) {
	t.Helper()
	if needsUUIDv7(loaded) {
		RequireUUIDv7(t, db)
	}
}

// Config returns configuration for a fresh, empty database, for tests of
// code that opens its own pool (for example Configure).
func Config(t testing.TB) config.Database {
	t.Helper()
	db, cfg := NewWithConfig(t)
	db.Close()
	return cfg
}

// NewWithConfig is New that also returns the database's configuration.
func NewWithConfig(t testing.TB) (*postgres.DB, config.Database) {
	t.Helper()
	db, cfg, _ := create(t)
	return db, cfg
}

// server is the test server's administration URL and the database created
// on it.
type server struct {
	base string
	name string
}

// create makes a fresh database on the test server and opens a pool on it
// as the server's own user.
func create(t testing.TB) (*postgres.DB, config.Database, server) {
	t.Helper()
	base := dbtest.ServerURL(t, config.AdapterPostgres)
	serverURL, err := url.Parse(base)
	if err != nil {
		t.Fatalf("%s is not a valid URL", dbtest.EnvAdapterURL(config.AdapterPostgres)) // not wrapped: may contain a password
	}

	name := dbtest.DatabaseName()
	ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
	defer cancel()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to the PostgreSQL test server: %v", err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(ctx)) }()
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	// Registered before anything else can fail, so the database is always
	// dropped. Cleanups run last-in first-out: the pool closes first.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
		defer cancel()
		admin, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Errorf("drop test database: %v", err)
			return
		}
		defer func() { _ = admin.Close(ctx) }()
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
	})

	testURL := *serverURL
	testURL.Path = "/" + name
	cfg := config.Default().Database
	cfg.URL = config.NewSecret(testURL.String())
	db, err := postgres.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(db.Close)
	return db, cfg, server{base: base, name: name}
}

// RLS is a migrated test database with the two roles of a deployment that
// isolates tenants with row-level security (docs/DATABASE.md, "Database
// roles"), so tests see the policies exactly as the application does.
type RLS struct {
	// Admin is the test server's own user (usually a superuser) on the
	// database, for setup and inspection; it bypasses row-level security.
	Admin *postgres.DB
	// Migrator owns the database and the schema and applied the
	// migrations, like FORGE_MIGRATION_DATABASE_URL's role.
	Migrator *postgres.DB
	// Runtime is the role an application serves with (FORGE_DATABASE_URL):
	// NOSUPERUSER NOBYPASSRLS, it owns nothing and holds only SELECT,
	// INSERT, UPDATE and DELETE on the tables and USAGE and SELECT on the
	// sequences the migrator creates, and no temporary tables.
	Runtime *postgres.DB
	// RuntimeConfig is Runtime's configuration, for code that opens its
	// own pool (for example the application's Configure).
	RuntimeConfig config.Database
	// RuntimeRole is the runtime role's name, quoted as an SQL identifier.
	RuntimeRole string
}

// NewRLS creates a fresh database owned by a new migrator role, applies the
// migrations of sets as that role (one globally ordered history, as
// NewMigratedSets), and connects a new runtime role whose privileges come
// from the migrator's default privileges, as in the generated
// docker/postgres/init.sql. The database and both roles are dropped when the
// test ends. The test server's user must be able to create roles and
// databases (a superuser, as in the generated compose service and CI).
func NewRLS(t testing.TB, sets ...fs.FS) *RLS {
	t.Helper()
	admin, adminCfg, srv := create(t)
	suffix := strings.ReplaceAll(uuid.MustNew().String(), "-", "")
	migratorRole, runtimeRole := "forge_test_migrator_"+suffix, "forge_test_runtime_"+suffix
	password := strings.ReplaceAll(uuid.MustNew().String(), "-", "")
	migrator, runtime := pgx.Identifier{migratorRole}.Sanitize(), pgx.Identifier{runtimeRole}.Sanitize()
	database := pgx.Identifier{srv.name}.Sanitize()

	// Runs after the pools below close and before create's own cleanup: the
	// roles can be dropped only once the database that refers to them is.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
		defer cancel()
		conn, err := pgx.Connect(ctx, srv.base)
		if err != nil {
			t.Errorf("drop test roles: %v", err)
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		for _, statement := range []string{
			"DROP DATABASE IF EXISTS " + database + " WITH (FORCE)",
			"DROP ROLE IF EXISTS " + runtime,
			"DROP ROLE IF EXISTS " + migrator,
		} {
			if _, err := conn.Exec(ctx, statement); err != nil {
				t.Errorf("drop test roles: %v", err)
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
	defer cancel()
	// Names and the password are generated above, never input.
	for _, statement := range []string{
		"CREATE ROLE " + migrator + " LOGIN PASSWORD '" + password + "' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"CREATE ROLE " + runtime + " LOGIN PASSWORD '" + password + "' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"ALTER DATABASE " + database + " OWNER TO " + migrator,
		"REVOKE ALL ON SCHEMA public FROM PUBLIC",
		"GRANT ALL ON SCHEMA public TO " + migrator,
		"GRANT USAGE ON SCHEMA public TO " + runtime,
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + migrator + " IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO " + runtime,
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + migrator + " IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO " + runtime,
		"REVOKE TEMPORARY ON DATABASE " + database + " FROM PUBLIC",
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatalf("create the migrator and runtime roles: %v", err)
		}
	}

	as := func(role string) config.Database {
		roleURL, err := url.Parse(adminCfg.URL.Reveal())
		if err != nil {
			t.Fatal("the test database URL is not valid") // not wrapped: may contain a password
		}
		roleURL.User = url.UserPassword(role, password)
		cfg := adminCfg
		cfg.URL = config.NewSecret(roleURL.String())
		return cfg
	}
	migratorCfg, runtimeCfg := as(migratorRole), as(runtimeRole)
	loaded, err := migrate.LoadMigrationSets(sets...)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	requireUUIDv7For(t, admin, loaded)
	m, err := postgres.NewMigrator(migratorCfg, loaded, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open migrator: %v", err)
	}
	defer func() { _ = m.Close() }()
	if _, err := m.Up(ctx); err != nil {
		t.Fatalf("apply migrations as the migrator role: %v", err)
	}
	rls := &RLS{Admin: admin, RuntimeConfig: runtimeCfg, RuntimeRole: runtime}
	for _, open := range []struct {
		cfg config.Database
		db  **postgres.DB
	}{{migratorCfg, &rls.Migrator}, {runtimeCfg, &rls.Runtime}} {
		db, err := postgres.Open(ctx, open.cfg)
		if err != nil {
			t.Fatalf("connect as a test role: %v", err)
		}
		t.Cleanup(db.Close)
		*open.db = db
	}
	return rls
}
