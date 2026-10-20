package dbtasks

import (
	"context"
	"net/url"
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
