package dbtasks

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/dbtest"
)

// pgTarget returns a target for a throwaway database name on the test
// server; the database does not exist yet. It is dropped when the test ends.
func pgTarget(t *testing.T) (*Target, string) {
	t.Helper()
	server := dbtest.ServerURL(t, config.AdapterPostgres)
	parsed, err := url.Parse(server)
	if err != nil {
		t.Fatal("the PostgreSQL test URL is not valid")
	}
	name := dbtest.DatabaseName()
	parsed.Path = "/" + name
	target, err := NewTarget(t.TempDir(), lookupOf(map[string]string{"FORGE_DATABASE_URL": parsed.String()}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
		defer cancel()
		if _, err := target.Drop(ctx); err != nil {
			t.Errorf("cleanup drop: %v", err)
		}
	})
	return target, parsed.String()
}

func TestPostgresCreateExistsDrop(t *testing.T) {
	t.Parallel()
	target, _ := pgTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
	defer cancel()

	if exists, err := target.Exists(ctx); err != nil || exists {
		t.Fatalf("Exists before create = %v, %v", exists, err)
	}
	if created, err := target.Create(ctx); err != nil || !created {
		t.Fatalf("Create = %v, %v", created, err)
	}
	if exists, err := target.Exists(ctx); err != nil || !exists {
		t.Fatalf("Exists after create = %v, %v", exists, err)
	}
	if created, err := target.Create(ctx); err != nil || created {
		t.Fatalf("second Create = %v, %v: it must be a no-op, not an error", created, err)
	}
	if dropped, err := target.Drop(ctx); err != nil || !dropped {
		t.Fatalf("Drop = %v, %v", dropped, err)
	}
	if exists, err := target.Exists(ctx); err != nil || exists {
		t.Fatalf("Exists after drop = %v, %v", exists, err)
	}
	if dropped, err := target.Drop(ctx); err != nil || dropped {
		t.Fatalf("second Drop = %v, %v: it must be a no-op", dropped, err)
	}
}

func TestPostgresDropEndsSessionsThatStillUseTheDatabase(t *testing.T) {
	t.Parallel()
	target, _ := pgTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
	defer cancel()
	if _, err := target.Create(ctx); err != nil {
		t.Fatal(err)
	}
	busy, err := target.pgConnect(ctx, target.Name) // a running development server, say
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close(context.WithoutCancel(ctx)) }()
	if dropped, err := target.Drop(ctx); err != nil || !dropped {
		t.Fatalf("Drop with an open session = %v, %v", dropped, err)
	}
}

func TestPostgresNeverTouchesTheMaintenanceDatabases(t *testing.T) {
	t.Parallel()
	target, server := pgTarget(t)
	parsed, _ := url.Parse(server)
	parsed.Path = "/postgres"
	protected, err := NewTarget(t.TempDir(), lookupOf(map[string]string{"FORGE_DATABASE_URL": parsed.String()}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := protected.Drop(context.Background()); err == nil {
		t.Fatal("dropping the postgres database must be refused")
	}
	_ = target
}

func TestPostgresPrivilegedURLCreatesTheApplicationDatabase(t *testing.T) {
	t.Parallel()
	target, server := pgTarget(t)
	// The application URL is unusable on its own (no such role); the
	// privileged URL carries the working credentials.
	parsed, _ := url.Parse(server)
	broken := *parsed
	broken.User = url.UserPassword("no_such_role", "wrong")
	withPrivileged, err := NewTarget(t.TempDir(), lookupOf(map[string]string{
		"FORGE_DATABASE_URL": broken.String(), EnvPrivilegedURL: server,
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
	defer cancel()
	if created, err := withPrivileged.Create(ctx); err != nil || !created {
		t.Fatalf("Create with FORGE_MIGRATION_DATABASE_URL = %v, %v", created, err)
	}
	if exists, err := target.Exists(ctx); err != nil || !exists {
		t.Fatalf("the database must exist for the real credentials: %v, %v", exists, err)
	}
}

func TestPostgresConnectionErrorsDoNotLeakThePassword(t *testing.T) {
	t.Parallel()
	server := dbtest.ServerURL(t, config.AdapterPostgres)
	parsed, _ := url.Parse(server)
	parsed.User = url.UserPassword("no_such_role", "hunter2-secret")
	parsed.Path = "/" + dbtest.DatabaseName()
	target, err := NewTarget(t.TempDir(), lookupOf(map[string]string{"FORGE_DATABASE_URL": parsed.String()}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
	defer cancel()
	_, err = target.Create(ctx)
	if err == nil {
		t.Fatal("want a connection error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("the error leaks the password: %v", err)
	}
}

// pgDumpTool returns a pg_dump that can talk to the test server, skipping
// the test when the one on PATH is older than the server.
func requirePGTools(t *testing.T, target *Target) {
	t.Helper()
	for _, name := range []string{"pg_dump", "psql"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is not installed", name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
	defer cancel()
	if _, err := target.run(ctx, tool{name: "pg_dump", args: []string{"--schema-only"}, env: target.pgEnvironment(environ())}); err != nil && strings.Contains(err.Error(), "server version mismatch") {
		t.Skipf("pg_dump on PATH is older than the test server (put a newer one first on PATH): %v", err)
	}
}

const testSchema = `
CREATE TABLE forge_schema_migrations (version BIGINT PRIMARY KEY, name VARCHAR(255) NOT NULL, checksum CHAR(64) NOT NULL, applied_at BIGINT NOT NULL);
CREATE TABLE items (
	id uuid PRIMARY KEY,
	tenant_id uuid NOT NULL,
	name text NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX items_tenant_idx ON items (tenant_id, name);
ALTER TABLE items ENABLE ROW LEVEL SECURITY;
ALTER TABLE items FORCE ROW LEVEL SECURITY;
CREATE POLICY items_tenant ON items USING (tenant_id = current_setting('forge.tenant_id', true)::uuid);
INSERT INTO forge_schema_migrations VALUES (20260101000000, 'create_items', '` + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" + `', 1767225600000000);
`

func TestPostgresSchemaDumpIsDeterministicAndLoads(t *testing.T) {
	t.Parallel()
	source, _ := pgTarget(t)
	requirePGTools(t, source)
	ctx, cancel := context.WithTimeout(context.Background(), 2*dbtest.SetupTimeout)
	defer cancel()
	if _, err := source.Create(ctx); err != nil {
		t.Fatal(err)
	}
	conn, err := source.pgConnect(ctx, source.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, testSchema, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)

	first, err := source.DumpSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // a timestamp in the output would differ now
	second, err := source.DumpSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("two dumps of the same database differ:\n--- first\n%s\n--- second\n%s", first, second)
	}
	text := string(first)
	for _, want := range []string{
		"CREATE TABLE public.items", "items_tenant_idx", "ENABLE ROW LEVEL SECURITY", "FORCE ROW LEVEL SECURITY", "CREATE POLICY items_tenant",
		"VALUES (20260101000000, 'create_items', '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef', 0);",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("dump lacks %q", want)
		}
	}
	for _, noise := range []string{"Dumped by", "Dumped from", "restrict ", "OWNER TO", "GRANT "} {
		if strings.Contains(text, noise) {
			t.Errorf("dump carries %q", noise)
		}
	}

	// The dump loads into a fresh database and dumps identically again.
	schemaFile := filepath.Join(t.TempDir(), "schema.sql")
	if err := writeFile(schemaFile, first); err != nil {
		t.Fatal(err)
	}
	clone, _ := pgTarget(t)
	if err := clone.LoadSchema(ctx, schemaFile); err == nil {
		t.Fatal("LoadSchema into a missing database must fail")
	}
	if _, err := clone.Create(ctx); err != nil {
		t.Fatal(err)
	}
	if err := clone.LoadSchema(ctx, schemaFile); err != nil {
		t.Fatal(err)
	}
	loaded, err := clone.DumpSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded) != string(first) {
		t.Fatalf("schema:load then schema:dump is not a round trip:\n--- original\n%s\n--- loaded\n%s", first, loaded)
	}
	// Loading again must fail instead of replacing tables.
	if err := clone.LoadSchema(ctx, schemaFile); err == nil {
		t.Fatal("loading into a database that already has the schema must fail")
	}
}

// pgConnConfig must not trust net/url and pgx to read a URL alike: these
// URLs bypass NewTarget's checks on purpose, and pgx would dial another
// server, port or database than the URL's authority and path name.
func TestPgConnConfigPinsTheCheckedServerAndDatabase(t *testing.T) {
	refused := map[string]string{
		"space before host":   "postgres://m:pw@localhost/shop?sslmode=disable& host=evil.example",
		"space after host":    "postgres://m:pw@localhost/shop?sslmode=disable&host =evil.example",
		"space before dbname": "postgres://m:pw@localhost/shop?sslmode=disable& dbname=other",
		"space before port":   "postgres://m:pw@localhost/shop?sslmode=disable& port=6543",
		"plain host":          "postgres://m:pw@localhost/shop?host=evil.example",
		"multiple hosts":      "postgres://m:pw@localhost/shop?host=localhost,evil.example",
		"service":             "postgres://m:pw@localhost/shop?service=prod",
		"hostaddr":            "postgres://m:pw@localhost/shop?hostaddr=10.0.0.5",
		"session setting":     "postgres://m:pw@localhost/shop?search_path=evil",
	}
	for name, raw := range refused {
		priv, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		target := &Target{Adapter: config.AdapterPostgres, Name: "shop", app: priv, priv: priv}
		cfg, err := target.pgConnConfig("shop")
		if err == nil {
			t.Errorf("%s: pgConnConfig = host %q port %d database %q, want a refusal", name, cfg.Host, cfg.Port, cfg.Database)
			continue
		}
		if strings.Contains(err.Error(), "pw") || strings.Contains(err.Error(), "evil") || strings.Contains(err.Error(), "://") {
			t.Errorf("%s: error quotes the URL: %v", name, err)
		}
	}

	// Accepted: the URL's own address, with an explicit default port that
	// PGPORT in forge's environment cannot change, as for the tools.
	t.Setenv("PGPORT", "6543")
	t.Setenv("PGHOST", "evil.example")
	priv, _ := url.Parse("postgres://m:pw@localhost/shop?sslmode=prefer&connect_timeout=5&application_name=forge")
	target := &Target{Adapter: config.AdapterPostgres, Name: "shop", app: priv, priv: priv}
	cfg, err := target.pgConnConfig("postgres")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "localhost" || cfg.Port != 5432 || cfg.Database != "postgres" {
		t.Errorf("pgConnConfig = host %q port %d database %q", cfg.Host, cfg.Port, cfg.Database)
	}
	if env := strings.Join(target.pgEnvironment(nil), "\n"); !strings.Contains(env, "PGPORT=5432") || !strings.Contains(env, "PGHOST=localhost") {
		t.Errorf("the tools must dial the same address:\n%s", env)
	}
}

func adminTarget(t *testing.T, raw string) *Target {
	t.Helper()
	priv, err := url.Parse(raw)
	if err != nil {
		t.Fatal("test URL is not valid")
	}
	return &Target{Adapter: config.AdapterPostgres, Name: "shop", app: priv, priv: priv}
}

// pgx reads PG* variables from the process environment as defaults; none of
// them may change the admin connection, as for the vendor tools.
func TestPgConnConfigIgnoresPGEnvironment(t *testing.T) {
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGOPTIONS", "-c search_path=evil")
	t.Setenv("PGTARGETSESSIONATTRS", "read-only")
	t.Setenv("PGSERVICE", "missing-service")
	t.Setenv("PGSERVICEFILE", "/nonexistent/pg_service.conf")
	t.Setenv("PGUSER", "evil")
	t.Setenv("PGPASSWORD", "evilpw")
	t.Setenv("PGTZ", "Mars/Base")
	target := adminTarget(t, "postgres://m:pw@localhost/shop")
	cfg, err := target.pgConnConfig("shop")
	if err != nil {
		t.Fatalf("a PG* variable changed the parse: %v", err)
	}
	// No sslmode in the URL: libpq's default (prefer) applies, not PGSSLMODE.
	if cfg.TLSConfig == nil || len(cfg.Fallbacks) != 1 || cfg.Fallbacks[0].TLSConfig != nil {
		t.Errorf("sslmode must be prefer (TLS, then plaintext), got TLS %v and %d fallbacks", cfg.TLSConfig != nil, len(cfg.Fallbacks))
	}
	if cfg.User != "m" || cfg.Password != "pw" {
		t.Errorf("credentials come from the URL, got user %q", cfg.User)
	}
	if cfg.ValidateConnect != nil {
		t.Error("PGTARGETSESSIONATTRS reached the connection")
	}
	for key := range cfg.RuntimeParams {
		if key == "options" || key == "timezone" {
			t.Errorf("session setting %q reached the connection", key)
		}
	}
	// forge's own environment is left as it was.
	if got := os.Getenv("PGSSLMODE"); got != "disable" {
		t.Errorf("PGSSLMODE after the parse = %q, want it restored", got)
	}
	if got := os.Getenv("PGPASSWORD"); got != "evilpw" {
		t.Errorf("PGPASSWORD after the parse = %q, want it restored", got)
	}
}

// A PGSSLROOTCERT (or PGSSLCERT, PGSSLKEY, PGSSLPASSWORD) the URL does not
// name has no neutral value: ignoring it could swap the CA a verify mode
// checks against, so the admin connection is refused, naming the variable
// and quoting neither its value nor the URL.
func TestPgConnConfigRefusesATLSFileVariableTheURLDoesNotName(t *testing.T) {
	for _, name := range []string{"PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY", "PGSSLPASSWORD"} {
		t.Setenv(name, "/secret-value-4711/file.pem")
		target := adminTarget(t, "postgres://m:hunter2@localhost/shop?sslmode=verify-ca")
		_, err := target.pgConnConfig("shop")
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s: err = %v, want a refusal naming it", name, err)
		}
		for _, leak := range []string{"secret-value", "hunter2", "localhost", "://"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: the error quotes %q: %v", name, leak, err)
			}
		}
		t.Setenv(name, "")
	}
}

func TestPgConnConfigNamesAnUnreadableTLSFile(t *testing.T) {
	for _, param := range []string{"sslrootcert", "sslcert", "sslkey"} {
		missing := "/nonexistent-forge-dir/" + param + ".pem"
		raw := "postgres://m:pw@localhost/shop?sslmode=verify-full&" + param + "=" + missing
		if param != "sslrootcert" {
			raw += "&sslrootcert=system"
		}
		if param == "sslcert" {
			raw += "&sslkey=" + missing
		}
		if param == "sslkey" {
			raw += "&sslcert=" + missing
		}
		_, err := adminTarget(t, raw).pgConnConfig("shop")
		if err == nil {
			t.Fatalf("%s: a missing file must fail", param)
		}
		msg := err.Error()
		if !strings.Contains(msg, "invalid TLS setting") || !strings.Contains(msg, "sslrootcert, sslcert and sslkey") {
			t.Errorf("%s: error does not point at the TLS files: %v", param, err)
		}
		if strings.Contains(msg, "do not accept") || strings.Contains(msg, "nonexistent") || strings.Contains(msg, "pw") || strings.Contains(msg, "://") {
			t.Errorf("%s: misleading or quoting error: %v", param, err)
		}
	}
}

func TestOptionsParameterIsRefusedForAdminConnections(t *testing.T) {
	t.Parallel()
	const option = "options=-c%20search_path%3Devil"
	// pgConnConfig refuses it on its own.
	_, err := adminTarget(t, "postgres://m:pw@localhost/shop?sslmode=disable&"+option).pgConnConfig("shop")
	if err == nil || strings.Contains(err.Error(), "evil") || strings.Contains(err.Error(), "pw") {
		t.Errorf("pgConnConfig with options = %v, want a refusal that does not quote the URL", err)
	}
	// NewTarget refuses it before any connection, naming the variable.
	for name, env := range map[string]map[string]string{
		"application URL": {"FORGE_DATABASE_URL": "postgres://m:pw@localhost/shop?sslmode=disable&" + option},
		"privileged URL":  {"FORGE_DATABASE_URL": "postgres://app:apppw@localhost/shop?sslmode=disable", EnvPrivilegedURL: "postgres://m:pw@localhost/shop?sslmode=disable&" + option},
		"upper-case key":  {"FORGE_DATABASE_URL": "postgres://m:pw@localhost/shop?sslmode=disable&OPTIONS=-c%20search_path%3Devil"},
	} {
		_, err := NewTarget(t.TempDir(), lookupOf(env))
		if err == nil || !strings.Contains(err.Error(), "options") || strings.Contains(err.Error(), "evil") || strings.Contains(err.Error(), "pw") {
			t.Errorf("%s: NewTarget = %v, want a refusal naming the parameter only", name, err)
		}
	}
	// The application's own URL may keep it when another role administers.
	if _, err := NewTarget(t.TempDir(), lookupOf(map[string]string{
		"FORGE_DATABASE_URL": "postgres://app:apppw@localhost/shop?sslmode=disable&" + option,
		EnvPrivilegedURL:     "postgres://m:pw@localhost/shop?sslmode=disable",
	})); err != nil {
		t.Errorf("options on the application URL alone must stay accepted: %v", err)
	}
}
