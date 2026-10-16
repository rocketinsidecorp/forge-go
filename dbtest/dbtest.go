// Package dbtest is the adapter-independent part of Forge's database test
// helpers (postgrestest, mysqltest, sqlitetest): finding the test server,
// naming throwaway databases and applying migrations.
package dbtest

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

const (
	// EnvURL is the test server of a generated application, whichever
	// database it uses.
	EnvURL = "FORGE_TEST_DATABASE_URL"
	// EnvRequire=true turns a missing test server into a failure instead of
	// a skip, so database tests can never silently stop running in CI.
	EnvRequire = "FORGE_TEST_REQUIRE_DATABASE"

	// SetupTimeout bounds creating, migrating and dropping test databases.
	SetupTimeout = 30 * time.Second
)

// EnvAdapterURL names the adapter-specific variable, such as
// FORGE_TEST_POSTGRES_URL, which lets one process test several databases.
func EnvAdapterURL(adapter config.Adapter) string {
	return "FORGE_TEST_" + strings.ToUpper(string(adapter)) + "_URL"
}

// ServerURL returns the URL of the test server for adapter: the
// adapter-specific variable, else EnvURL when its scheme selects adapter.
// Without one the test is skipped, or fails when EnvRequire is true.
func ServerURL(t testing.TB, adapter config.Adapter) string {
	t.Helper()
	if raw := os.Getenv(EnvAdapterURL(adapter)); raw != "" {
		return raw
	}
	if raw := os.Getenv(EnvURL); raw != "" {
		database := config.Default().Database
		database.URL = config.NewSecret(raw)
		if database.Adapter() == adapter {
			return raw
		}
	}
	if raw := os.Getenv(EnvRequire); raw != "" {
		// A value that does not parse must not silently turn required
		// database tests back into skips.
		required, err := strconv.ParseBool(raw)
		if err != nil {
			t.Fatalf("%s must be true or false", EnvRequire)
		}
		if required {
			t.Fatalf("%s or %s is required when %s is set", EnvAdapterURL(adapter), EnvURL, EnvRequire)
		}
	}
	t.Skipf("set %s to run %s tests", EnvAdapterURL(adapter), adapter)
	return ""
}

// DatabaseName returns a fresh name safe to use as an unquoted identifier.
func DatabaseName() string {
	return "forge_test_" + strings.ReplaceAll(uuid.MustNew().String(), "-", "")
}

// Migrate loads migrations, applies them with the migrator newMigrator
// returns, and closes it.
func Migrate(t testing.TB, migrations fs.FS, newMigrator func([]migrate.Migration, *slog.Logger) (*migrate.Migrator, error)) {
	t.Helper()
	loaded, err := migrate.LoadMigrations(migrations)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	migrator, err := newMigrator(loaded, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open migrator: %v", err)
	}
	defer func() { _ = migrator.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), SetupTimeout)
	defer cancel()
	if _, err := migrator.Up(ctx); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
}
