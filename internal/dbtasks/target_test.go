package dbtasks

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
)

func lookupOf(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestNewTargetResolvesEachAdapter(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tests := []struct {
		name        string
		env         map[string]string
		adapter     config.Adapter
		database    string
		environment config.Environment
	}{
		{"postgres", map[string]string{"FORGE_DATABASE_URL": "postgres://app:s3cret@db.internal:5433/shop_development?sslmode=disable"}, config.AdapterPostgres, "shop_development", config.Development},
		{"mysql", map[string]string{"FORGE_DATABASE_URL": "mysql://app:s3cret@db.internal/shop"}, config.AdapterMySQL, "shop", config.Development},
		{"sqlite relative", map[string]string{"FORGE_DATABASE_URL": "sqlite:storage/dev.db"}, config.AdapterSQLite, filepath.Join(root, "storage", "dev.db"), config.Development},
		{"sqlite absolute", map[string]string{"FORGE_DATABASE_URL": "sqlite:///var/lib/app/app.db", "FORGE_ENV": "test"}, config.AdapterSQLite, "/var/lib/app/app.db", config.Test},
	}
	for _, test := range tests {
		target, err := NewTarget(root, lookupOf(test.env))
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if target.Adapter != test.adapter || target.Name != test.database || target.Environment != test.environment {
			t.Errorf("%s: target = %+v", test.name, target)
		}
		if strings.Contains(target.Describe(), "s3cret") || strings.Contains(target.Describe(), "app:") {
			t.Errorf("%s: Describe leaks credentials: %q", test.name, target.Describe())
		}
	}
}

func TestNewTargetRefusesUnsafeOrMissingConfiguration(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tests := map[string]map[string]string{
		"no database":              {},
		"invalid configuration":    {"FORGE_DATABASE_URL": "postgres://u:p@h/db?sslmode=disable", "FORGE_HTTP_READ_TIMEOUT": "banana"},
		"database name with a dot": {"FORGE_DATABASE_URL": "postgres://u:p@h/my.db?sslmode=disable"},
		"database name too long":   {"FORGE_DATABASE_URL": "postgres://u:p@h/" + strings.Repeat("a", 64) + "?sslmode=disable"},
		"missing database name":    {"FORGE_DATABASE_URL": "postgres://u:p@h:5432?sslmode=disable"},
		"name with a quote":        {"FORGE_DATABASE_URL": "postgres://u:p@h/a%22b?sslmode=disable"},
		"privileged adapter differs": {
			"FORGE_DATABASE_URL": "postgres://u:p@h/db?sslmode=disable", EnvPrivilegedURL: "mysql://root:p@h/db",
		},
		"privileged url invalid": {
			"FORGE_DATABASE_URL": "postgres://u:p@h/db?sslmode=disable", EnvPrivilegedURL: "postgres://u:p@h/db?sslmode=disable&sslmode=require",
		},
		"production plaintext privileged": {
			"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy",
			"FORGE_DATABASE_URL": "postgres://u:p@h/db?sslmode=verify-full", EnvPrivilegedURL: "postgres://root:p@h/db?sslmode=disable",
		},
	}
	for name, env := range tests {
		target, err := NewTarget(root, lookupOf(env))
		if err == nil {
			t.Errorf("%s: NewTarget = %+v, want an error", name, target)
			continue
		}
		for _, secret := range []string{":p@", "s3cret"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: error leaks a credential: %v", name, err)
			}
		}
	}
}

func TestPrivilegedURLSuppliesCredentialsButNotTheDatabase(t *testing.T) {
	t.Parallel()
	target, err := NewTarget(t.TempDir(), lookupOf(map[string]string{
		"FORGE_DATABASE_URL": "postgres://app:apppw@db:5432/shop?sslmode=disable",
		EnvPrivilegedURL:     "postgres://admin:adminpw@db:5432/postgres?sslmode=disable",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if target.Name != "shop" {
		t.Fatalf("the target database must come from FORGE_DATABASE_URL, got %q", target.Name)
	}
	env := strings.Join(target.pgEnvironment(nil), "\n")
	if !strings.Contains(env, "PGUSER=admin") || !strings.Contains(env, "PGPASSWORD=adminpw") || !strings.Contains(env, "PGDATABASE=shop") {
		t.Fatalf("tools must use the privileged role on the target database:\n%s", env)
	}
}

func TestReservedDatabasesAreNeverCreatedOrDropped(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"postgres", "template0", "template1", "mysql", "information_schema", "sys"} {
		target, err := NewTarget(t.TempDir(), lookupOf(map[string]string{"FORGE_DATABASE_URL": "postgres://u:p@h/" + name + "?sslmode=disable"}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := target.Create(context.Background()); err == nil {
			t.Errorf("Create(%s) must be refused", name)
		}
		if _, err := target.Drop(context.Background()); err == nil {
			t.Errorf("Drop(%s) must be refused", name)
		}
	}
}

func TestSQLiteCreateAndDrop(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target, err := NewTarget(root, lookupOf(map[string]string{"FORGE_DATABASE_URL": "sqlite:storage/dev.db"}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if exists, err := target.Exists(ctx); err != nil || exists {
		t.Fatalf("Exists = %v, %v before create", exists, err)
	}
	if created, err := target.Create(ctx); err != nil || !created {
		t.Fatalf("Create = %v, %v", created, err)
	}
	info, err := os.Stat(target.Name)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("database file: %v, %v (want mode 0600)", info, err)
	}
	if created, err := target.Create(ctx); err != nil || created {
		t.Fatalf("second Create = %v, %v, want false (already exists)", created, err)
	}
	if err := os.WriteFile(target.Name, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if created, err := target.Create(ctx); err != nil || created {
		t.Fatalf("Create over an existing database = %v, %v", created, err)
	}
	if data, _ := os.ReadFile(target.Name); string(data) != "data" {
		t.Fatal("Create must not touch an existing database")
	}
	for _, suffix := range sqliteCompanions {
		if err := os.WriteFile(target.Name+suffix, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if dropped, err := target.Drop(ctx); err != nil || !dropped {
		t.Fatalf("Drop = %v, %v", dropped, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(target.Name))
	if len(entries) != 0 {
		t.Fatalf("database and its companions must be gone, left: %v", entries)
	}
	if dropped, err := target.Drop(ctx); err != nil || dropped {
		t.Fatalf("second Drop = %v, %v, want false", dropped, err)
	}
}

func TestSQLiteDropRefusesNonRegularFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "db.sqlite"), 0o750); err != nil {
		t.Fatal(err)
	}
	target, err := NewTarget(root, lookupOf(map[string]string{"FORGE_DATABASE_URL": "sqlite:db.sqlite"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.Drop(context.Background()); err == nil {
		t.Fatal("Drop removed a directory")
	}
	if _, err := os.Stat(filepath.Join(root, "db.sqlite")); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSchemaAndConsoleRefuseSystemDatabases(t *testing.T) {
	t.Parallel()
	target, err := NewTarget(t.TempDir(), lookupOf(map[string]string{"FORGE_DATABASE_URL": "postgres://u:p@h/postgres?sslmode=disable"}))
	if err != nil {
		t.Fatal(err)
	}
	target.SetLookPath(fakeLookPath)
	if err := target.LoadSchema(context.Background(), filepath.Join(t.TempDir(), "schema.sql")); err == nil || !strings.Contains(err.Error(), "system database") {
		t.Errorf("LoadSchema into postgres = %v, want a refusal", err)
	}
	if _, err := target.PrepareConsole(nil); err == nil || !strings.Contains(err.Error(), "system database") {
		t.Errorf("PrepareConsole on postgres = %v, want a refusal", err)
	}
}

func TestProtectionReason(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		url, env  string
		protected bool
	}{
		"dev local":          {"postgres://u:p@127.0.0.1/db?sslmode=disable", "development", false},
		"test local":         {"mysql://u:p@localhost/db", "test", false},
		"sqlite":             {"sqlite:db.sqlite", "development", false},
		"staging":            {"postgres://u:p@127.0.0.1/db?sslmode=disable", "staging", true},
		"dev remote":         {"postgres://u:p@db.internal/db?sslmode=disable", "development", true},
		"dev remote address": {"mysql://u:p@10.1.2.3/db", "development", true},
	}
	for name, test := range tests {
		target, err := NewTarget(t.TempDir(), lookupOf(map[string]string{"FORGE_DATABASE_URL": test.url, "FORGE_ENV": test.env}))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := target.ProtectionReason() != ""; got != test.protected {
			t.Errorf("%s: protected = %v (%q), want %v", name, got, target.ProtectionReason(), test.protected)
		}
	}
}

func TestIsLocalHost(t *testing.T) {
	t.Parallel()
	for host, want := range map[string]bool{
		"127.0.0.1": true, "127.0.0.2": true, "[::1]:5432": true, "localhost": true, "LOCALHOST": true, "/var/run/postgresql": true,
		"db.internal": false, "10.0.0.1": false, "localhost.evil.com": false, "0.0.0.0": false,
	} {
		if got := isLocalHost(&url.URL{Host: host}); got != want {
			t.Errorf("isLocalHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// pgx, like libpq, lets URL query parameters override the host, port and
// database, so a URL whose authority is local could dial another server.
// Such URLs are refused, for both the application and the privileged URL.
func TestNewTargetRefusesQueryParametersThatPickTheServerOrDatabase(t *testing.T) {
	t.Parallel()
	urls := map[string]string{
		"host":                "postgres://m:pw@localhost/shop?host=prod.db.internal&sslmode=disable",
		"socket host":         "postgres://m:pw@localhost/shop?host=/var/run/postgresql",
		"hostaddr":            "postgres://m:pw@127.0.0.1/shop?hostaddr=10.0.0.5",
		"port":                "postgres://m:pw@localhost/shop?port=6543",
		"dbname":              "postgres://localhost/shop?dbname=postgres",
		"database":            "postgres://localhost/shop?database=postgres",
		"percent-encoded key": "postgres://localhost/shop?d%62name=postgres",
		"upper-case key":      "postgres://localhost/shop?HOST=prod.db.internal",
		"service":             "postgres://localhost/shop?service=prod",
		"servicefile":         "postgres://localhost/shop?servicefile=/tmp/pg_service.conf",
		// net/url keeps the spaces in the key, pgx trims them and obeys it.
		"space before host":   "postgres://m:pw@localhost/shop?sslmode=disable& host=prod.db.internal",
		"space after host":    "postgres://m:pw@localhost/shop?sslmode=disable&host =prod.db.internal",
		"space before dbname": "postgres://m:pw@localhost/shop?sslmode=disable& dbname=postgres",
		"space before port":   "postgres://m:pw@localhost/shop?sslmode=disable& port=6543",
		"plus before host":    "postgres://m:pw@localhost/shop?sslmode=disable&+host=prod.db.internal",
		"no-break space key":  "postgres://m:pw@localhost/shop?sslmode=disable&\u00a0host=prod.db.internal",
	}
	for name, raw := range urls {
		for _, env := range []map[string]string{
			{"FORGE_DATABASE_URL": raw},
			{"FORGE_DATABASE_URL": "postgres://app:apppw@localhost/shop?sslmode=disable", EnvPrivilegedURL: raw},
		} {
			target, err := NewTarget(t.TempDir(), lookupOf(env))
			if err == nil {
				t.Errorf("%s: NewTarget = local %v, name %q; want a refusal", name, target.Local(), target.Name)
				continue
			}
			if strings.Contains(err.Error(), "pw") || strings.Contains(err.Error(), "prod") || strings.Contains(err.Error(), "://") {
				t.Errorf("%s: error quotes the URL: %v", name, err)
			}
		}
	}
	// Connection options that do not pick the server stay accepted.
	target, err := NewTarget(t.TempDir(), lookupOf(map[string]string{"FORGE_DATABASE_URL": "postgres://m:pw@localhost/shop?sslmode=disable&connect_timeout=5"}))
	if err != nil {
		t.Fatal(err)
	}
	if !target.Local() {
		t.Fatal("localhost must be local")
	}
}

// A password with an unencoded "/", "?", "#" or "@" can push part of the
// userinfo into the host, path or query. Configuration refuses those URLs
// or parses them so that Describe and errors carry no credential.
func TestUnencodedPasswordsNeverReachDescribeOrErrors(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"postgres://usr7:pa55/w0rd@localhost/shop?sslmode=disable",
		"postgres://usr7:98765/w0rd@localhost/shop?sslmode=disable",
		"postgres://usr7:pa55?w0rd@localhost/shop?sslmode=disable",
		"postgres://usr7:pa55#w0rd@localhost/shop?sslmode=disable",
		"postgres://usr7:pa55@w0rd@localhost/shop?sslmode=disable",
		"postgres://usr7:98765@localhost/shop?sslmode=disable",
		"mysql://usr7:pa55/w0rd@localhost/shop",
		"mysql://usr7:98765/w0rd@localhost/shop",
		"mysql://usr7:pa55?w0rd@localhost/shop",
		"mysql://usr7:pa55#w0rd@localhost/shop",
		"mysql://usr7:pa55@w0rd@localhost/shop",
	} {
		target, err := NewTarget(t.TempDir(), lookupOf(map[string]string{"FORGE_DATABASE_URL": raw}))
		var shown string
		if err != nil {
			shown = err.Error()
		} else {
			shown = target.Describe()
		}
		for _, secret := range []string{"usr7", "pa55", "w0rd", "98765"} {
			if strings.Contains(shown, secret) {
				t.Errorf("%s: %q reveals %q", raw, shown, secret)
			}
		}
	}
}

func TestDescribeNamesTheServerTheCommandsTalkTo(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		privileged, want string
	}{
		"no privileged URL":    {"", `postgres database "shop" on db:5432`},
		"same server":          {"postgres://root7:r00tpw@DB:5432/postgres?sslmode=disable", `postgres database "shop" on DB:5432`},
		"another server":       {"postgres://root7:r00tpw@db-admin:5432/postgres?sslmode=disable", `postgres database "shop" on db-admin:5432 (application: db:5432)`},
		"another port, no pwd": {"postgres://root7@db:6432/postgres?sslmode=disable", `postgres database "shop" on db:6432 (application: db:5432)`},
	}
	for name, test := range tests {
		env := map[string]string{"FORGE_DATABASE_URL": "postgres://app:apppw@db:5432/shop?sslmode=disable"}
		if test.privileged != "" {
			env[EnvPrivilegedURL] = test.privileged
		}
		target, err := NewTarget(t.TempDir(), lookupOf(env))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := target.Describe()
		if got != test.want {
			t.Errorf("%s: Describe = %q, want %q", name, got, test.want)
		}
		for _, secret := range []string{"app:", "apppw", "root7", "r00tpw"} {
			if strings.Contains(got, secret) {
				t.Errorf("%s: Describe leaks %q: %q", name, secret, got)
			}
		}
	}
}
