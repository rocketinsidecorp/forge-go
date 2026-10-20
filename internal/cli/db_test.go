package cli

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/dbtest"
	"github.com/rocketinsidecorp/forge-go/internal/dbtasks"
)

// fakeApp stands in for the application binary: it records the commands the
// CLI delegates and runs hooks, so lifecycle tests need no compiled app.
type fakeApp struct {
	calls []string
	hooks map[string]func() error
}

func (f *fakeApp) Run(_ context.Context, args []string) error {
	call := strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if hook := f.hooks[call]; hook != nil {
		return hook()
	}
	return nil
}

func (f *fakeApp) Close() {}

type dbHarness struct {
	*dbCommand
	app     *fakeApp
	out     bytes.Buffer
	errOut  bytes.Buffer
	process map[string]string
}

// newDBHarness builds a `forge db` run over root with the given .env.development
// content and the fake application.
func newDBHarness(t *testing.T, envFile string) *dbHarness {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env.development"), []byte(envFile), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &dbHarness{app: &fakeApp{hooks: map[string]func() error{}}, process: map[string]string{}}
	h.dbCommand = &dbCommand{
		root: root, stdin: strings.NewReader(""), stdout: &h.out, stderr: &h.errOut,
		processEnv: func(key string) (string, bool) { value, ok := h.process[key]; return value, ok },
	}
	h.newApp = func(context.Context) (appRunner, error) { return h.app, nil }
	h.target = func() (*dbtasks.Target, error) {
		env, err := environment(root, nil)
		if err != nil {
			return nil, err
		}
		return dbtasks.NewTarget(root, lookupIn(env))
	}
	return h
}

func (h *dbHarness) do(args ...string) error {
	return h.run(context.Background(), args)
}

func TestDBRejectsBadInvocations(t *testing.T) {
	t.Parallel()
	h := newDBHarness(t, "FORGE_DATABASE_URL=sqlite:storage/dev.db\n")
	for _, args := range [][]string{
		nil, {"bogus"}, {"create", "extra"}, {"setup", "--force"}, {"migrate", "x"}, {"drop", "--unknown"}, {"drop", "name"}, {"console", "psql"}, {"schema:dump", "x"},
	} {
		if err := h.do(args...); err == nil {
			t.Errorf("forge db %v succeeded, want an error", args)
		}
	}
	if len(h.app.calls) != 0 {
		t.Fatalf("a rejected command must not reach the application: %v", h.app.calls)
	}
}

func TestDBRefusesWithoutADatabase(t *testing.T) {
	t.Parallel()
	h := newDBHarness(t, "FORGE_ENV=development\n")
	if err := h.do("create"); err == nil || !strings.Contains(err.Error(), "database is not configured") {
		t.Fatalf("err = %v", err)
	}
}

func TestDBSQLiteLifecycle(t *testing.T) {
	t.Parallel()
	h := newDBHarness(t, "FORGE_DATABASE_URL=sqlite:storage/dev.db\n")
	file := filepath.Join(h.root, "storage", "dev.db")
	exists := func() bool { _, err := os.Stat(file); return err == nil }

	if err := h.do("prepare"); err != nil {
		t.Fatal(err)
	}
	if !exists() {
		t.Fatal("prepare must create a missing database")
	}
	if got := strings.Join(h.app.calls, ","); got != "migrate,seed" {
		t.Fatalf("prepare on a missing database = %s, want setup (migrate then seed)", got)
	}

	h.app.calls = nil
	if err := h.do("prepare"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.app.calls, ","); got != "migrate" {
		t.Fatalf("prepare on an existing database = %s, want migrate only (idempotent)", got)
	}

	if err := os.WriteFile(file, []byte("rows"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.app.calls = nil
	if err := h.do("reset"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(file); string(data) == "rows" {
		t.Fatal("reset must drop the old database")
	}
	if got := strings.Join(h.app.calls, ","); got != "migrate,seed" {
		t.Fatalf("reset = %s, want drop then setup", got)
	}

	if err := h.do("drop"); err != nil {
		t.Fatal(err)
	}
	if exists() {
		t.Fatal("drop must remove the file")
	}
	if err := h.do("drop"); err != nil || !strings.Contains(h.out.String(), "does not exist") {
		t.Fatalf("a second drop must be a no-op: %v\n%s", err, h.out.String())
	}
}

func TestDBDelegatesToTheApplication(t *testing.T) {
	t.Parallel()
	h := newDBHarness(t, "FORGE_DATABASE_URL=sqlite:storage/dev.db\n")
	for args, want := range map[string]string{
		"migrate": "migrate", "status": "migrate status", "seed": "seed",
	} {
		h.app.calls = nil
		if err := h.do(args); err != nil {
			t.Fatal(err)
		}
		if len(h.app.calls) != 1 || h.app.calls[0] != want {
			t.Errorf("forge db %s ran %v, want %q", args, h.app.calls, want)
		}
	}
	h.app.calls = nil
	if err := h.do("rollback", "-steps", "2"); err != nil {
		t.Fatal(err)
	}
	if len(h.app.calls) != 1 || h.app.calls[0] != "rollback -steps 2" {
		t.Errorf("rollback ran %v", h.app.calls)
	}
}

func TestDBSetupStopsWhenAStepFails(t *testing.T) {
	t.Parallel()
	h := newDBHarness(t, "FORGE_DATABASE_URL=sqlite:storage/dev.db\n")
	h.app.hooks["migrate"] = func() error { return os.ErrInvalid }
	if err := h.do("setup"); err == nil {
		t.Fatal("setup must fail when migrate fails")
	}
	if got := strings.Join(h.app.calls, ","); got != "migrate" {
		t.Fatalf("seed must not run after a failed migrate, calls: %s", got)
	}
}

const productionEnv = "FORGE_ENV=production\nFORGE_HTTP_TRANSPORT=trusted-proxy\nFORGE_DATABASE_URL=postgres://app:pw@db.internal:5432/shop_production?sslmode=verify-full\n"

func TestDBProductionGuardRefusesDestructiveCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		args        []string
		interactive bool
		input       string
		process     map[string]string
		wantErr     string
	}{
		{"drop without a flag", []string{"drop"}, true, "shop_production\n", nil, "pass --force-production"},
		{"reset without a flag", []string{"reset"}, true, "shop_production\n", nil, "pass --force-production"},
		{"schema:load without a flag", []string{"schema:load"}, true, "shop_production\n", nil, "pass --force-production"},
		{"rollback without a flag", []string{"rollback"}, true, "shop_production\n", nil, "pass --force-production"},
		{"yes without the flag is not enough", []string{"drop", "--yes"}, false, "", map[string]string{"FORGE_ALLOW_DESTRUCTIVE": "true"}, "pass --force-production"},
		{"forced but no terminal and no yes", []string{"drop", "--force-production"}, false, "shop_production\n", nil, "interactive terminal"},
		{"yes without the environment grant", []string{"drop", "--force-production", "--yes"}, false, "", nil, "FORGE_ALLOW_DESTRUCTIVE=true"},
		{"yes with a grant that is not exactly true", []string{"drop", "--force-production", "--yes"}, false, "", map[string]string{"FORGE_ALLOW_DESTRUCTIVE": "1"}, "FORGE_ALLOW_DESTRUCTIVE=true"},
		{"wrong name typed", []string{"drop", "--force-production"}, true, "shop\n", nil, "did not match"},
		{"empty answer", []string{"drop", "--force-production"}, true, "", nil, "no confirmation"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newDBHarness(t, productionEnv)
			h.interactive = test.interactive
			h.stdin = strings.NewReader(test.input)
			h.process = test.process
			if h.process == nil {
				h.process = map[string]string{}
			}
			h.processEnv = func(key string) (string, bool) { v, ok := h.process[key]; return v, ok }
			err := h.do(test.args...)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, test.wantErr)
			}
			if len(h.app.calls) != 0 {
				t.Fatalf("a refused command must not run anything: %v", h.app.calls)
			}
			if strings.Contains(err.Error(), "pw@") || strings.Contains(h.out.String()+h.errOut.String(), ":pw@") {
				t.Fatal("output leaks the database password")
			}
		})
	}
}

func TestDBProductionGuardAcceptsExplicitConfirmation(t *testing.T) {
	t.Parallel()
	h := newDBHarness(t, productionEnv)
	target, err := h.target()
	if err != nil {
		t.Fatal(err)
	}
	if target.Environment != config.Production {
		t.Fatalf("environment = %s", target.Environment)
	}

	// A person typed the name at a terminal.
	h.interactive = true
	h.stdin = strings.NewReader("shop_production\n")
	if err := h.confirm(target, "drop", destructiveFlags{forceProduction: true}); err != nil {
		t.Fatalf("typed confirmation: %v", err)
	}
	if !strings.Contains(h.errOut.String(), "FORGE_ENV is production") {
		t.Fatalf("the prompt must say why the database is protected: %q", h.errOut.String())
	}

	// --yes with the explicit environment grant.
	h.interactive = false
	h.process["FORGE_ALLOW_DESTRUCTIVE"] = "true"
	if err := h.confirm(target, "drop", destructiveFlags{forceProduction: true, yes: true}); err != nil {
		t.Fatalf("--yes with FORGE_ALLOW_DESTRUCTIVE=true: %v", err)
	}
}

func TestDBProductionGuardIgnoresEnvFileGrants(t *testing.T) {
	t.Parallel()
	// The grant must come from the process environment: a committed or
	// edited env file cannot switch the protection off.
	h := newDBHarness(t, productionEnv+"FORGE_ALLOW_DESTRUCTIVE=true\n")
	err := h.do("drop", "--force-production", "--yes")
	if err == nil || !strings.Contains(err.Error(), "FORGE_ALLOW_DESTRUCTIVE=true in the process environment") {
		t.Fatalf("err = %v", err)
	}
}

func TestDBGuardCoversEverythingButLocalDevelopmentAndTest(t *testing.T) {
	t.Parallel()
	const remote = "FORGE_DATABASE_URL=postgres://app:pw@db.example.com:5432/shop?sslmode=disable\n"
	const local = "FORGE_DATABASE_URL=postgres://app:pw@127.0.0.1:5432/shop?sslmode=disable\n"
	tests := []struct {
		name      string
		env       string
		protected bool
	}{
		{"development, local server", "FORGE_ENV=development\n" + local, false},
		{"test, local server", "FORGE_ENV=test\n" + local, false},
		{"FORGE_ENV unset, local server", local, false},
		{"development, localhost", "FORGE_ENV=development\nFORGE_DATABASE_URL=postgres://a:b@localhost/shop?sslmode=disable\n", false},
		{"development, IPv6 loopback", "FORGE_ENV=development\nFORGE_DATABASE_URL=postgres://a:b@[::1]:5432/shop?sslmode=disable\n", false},
		{"development, sqlite file", "FORGE_ENV=development\nFORGE_DATABASE_URL=sqlite:storage/x.db\n", false},
		{"staging, local server", "FORGE_ENV=staging\n" + local, true},
		{"staging, sqlite file", "FORGE_ENV=staging\nFORGE_DATABASE_URL=sqlite:storage/x.db\n", true},
		{"development, real server", "FORGE_ENV=development\n" + remote, true},
		{"test, real server", "FORGE_ENV=test\n" + remote, true},
		{"FORGE_ENV unset, real server", remote, true},
		{"privileged URL on a real server", "FORGE_ENV=development\n" + local + "FORGE_MIGRATION_DATABASE_URL=postgres://root:pw@db.example.com/postgres?sslmode=disable\n", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newDBHarness(t, test.env)
			target, err := h.target()
			if err != nil {
				t.Fatal(err)
			}
			err = h.confirm(target, "drop", destructiveFlags{})
			if test.protected && (err == nil || !strings.Contains(err.Error(), "pass --force-production")) {
				t.Fatalf("a drop must be refused without confirmation, got %v", err)
			}
			if !test.protected && err != nil {
				t.Fatalf("no confirmation expected: %v", err)
			}
			if test.protected {
				// And the full command stops before touching anything.
				for _, args := range [][]string{{"drop"}, {"reset"}, {"schema:load"}, {"rollback"}} {
					if err := h.do(args...); err == nil || !strings.Contains(err.Error(), "--force-production") {
						t.Errorf("forge db %v = %v, want it refused", args, err)
					}
				}
				if len(h.app.calls) != 0 {
					t.Fatalf("a refused command ran %v", h.app.calls)
				}
			}
		})
	}
}

// pgHarness returns a harness on a throwaway PostgreSQL database that is
// dropped when the test ends.
func pgHarness(t *testing.T) (*dbHarness, string) {
	t.Helper()
	server := dbtest.ServerURL(t, config.AdapterPostgres)
	parsed, err := url.Parse(server)
	if err != nil {
		t.Fatal("the PostgreSQL test URL is not valid")
	}
	name := dbtest.DatabaseName()
	parsed.Path = "/" + name
	h := newDBHarness(t, "FORGE_DATABASE_URL="+parsed.String()+"\n")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
		defer cancel()
		if target, err := h.target(); err == nil {
			_, _ = target.Drop(ctx)
		}
	})
	return h, parsed.String()
}

func pgExec(t *testing.T, databaseURL, statement string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, statement, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
}

func pgTableExists(t *testing.T, databaseURL, table string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

func TestDBPostgresPrepareIsIdempotentAndResetStartsOver(t *testing.T) {
	t.Parallel()
	h, databaseURL := pgHarness(t)
	h.app.hooks["migrate"] = func() error {
		pgExec(t, databaseURL, "CREATE TABLE IF NOT EXISTS widgets (id int)")
		return nil
	}

	if err := h.do("prepare"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.app.calls, ","); got != "migrate,seed" {
		t.Fatalf("first prepare = %s", got)
	}
	if !pgTableExists(t, databaseURL, "widgets") {
		t.Fatal("migrate hook did not run against the created database")
	}
	pgExec(t, databaseURL, "INSERT INTO widgets VALUES (1)")

	h.app.calls = nil
	if err := h.do("prepare"); err != nil {
		t.Fatalf("second prepare: %v", err)
	}
	if got := strings.Join(h.app.calls, ","); got != "migrate" {
		t.Fatalf("second prepare = %s, want migrate only", got)
	}
	if err := h.do("setup"); err != nil {
		t.Fatalf("setup on an existing database must be idempotent: %v", err)
	}

	// reset drops the data and rebuilds from migrations.
	h.app.calls = nil
	if err := h.do("reset"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.app.calls, ","); got != "migrate,seed" {
		t.Fatalf("reset = %s", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var rows int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM widgets").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows after reset = %d, %v; the data must be gone", rows, err)
	}

	if err := h.do("drop"); err != nil {
		t.Fatal(err)
	}
	if err := h.do("create"); err != nil || !strings.Contains(h.out.String(), "created") {
		t.Fatalf("create after drop: %v\n%s", err, h.out.String())
	}
}

func TestDBPostgresSchemaDumpIsDeterministicAndLoadsIntoANewDatabase(t *testing.T) {
	t.Parallel()
	h, databaseURL := pgHarness(t)
	if err := h.do("create"); err != nil {
		t.Fatal(err)
	}
	pgExec(t, databaseURL, `CREATE TABLE forge_schema_migrations (version BIGINT PRIMARY KEY, name VARCHAR(255) NOT NULL, checksum CHAR(64) NOT NULL, applied_at BIGINT NOT NULL);
CREATE TABLE widgets (id uuid PRIMARY KEY, name text NOT NULL);
INSERT INTO forge_schema_migrations VALUES (20260101000000, 'create_widgets', '`+strings.Repeat("ab", 32)+`', 1767225600000000);`)

	if err := h.do("schema:dump"); err != nil {
		t.Skipf("schema:dump needs pg_dump of the server's version on PATH: %v", err)
	}
	schemaPath := filepath.Join(h.root, "db", "schema.sql")
	first, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := h.do("schema:dump"); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(schemaPath)
	if !bytes.Equal(first, second) {
		t.Fatal("db/schema.sql changed between two dumps of an unchanged database")
	}
	if !strings.Contains(string(first), "CREATE TABLE public.widgets") || !strings.Contains(string(first), "'create_widgets'") {
		t.Fatalf("schema.sql:\n%s", first)
	}
	if info, err := os.Stat(schemaPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("schema.sql: %v, %v (forge writes files 0600)", info, err)
	}

	// Load it into a fresh database, as a test bootstrap would.
	other, otherURL := pgHarness(t)
	other.root = h.root
	other.target = func() (*dbtasks.Target, error) {
		return dbtasks.NewTarget(h.root, lookupIn([]string{"FORGE_DATABASE_URL=" + otherURL}))
	}
	if err := other.do("schema:load"); err == nil || !strings.Contains(err.Error(), "forge db create") {
		t.Fatalf("schema:load into a missing database: %v", err)
	}
	if err := other.do("create"); err != nil {
		t.Fatal(err)
	}
	if err := other.do("schema:load"); err != nil {
		t.Fatal(err)
	}
	if !pgTableExists(t, otherURL, "widgets") || !pgTableExists(t, otherURL, "forge_schema_migrations") {
		t.Fatal("schema:load did not create the tables")
	}
}

func TestDBPostgresConsoleRunsPsqlWithoutPasswordArguments(t *testing.T) {
	t.Parallel()
	h, _ := pgHarness(t)
	if err := h.do("create"); err != nil {
		t.Fatal(err)
	}
	if err := h.do("console", "--", "-Atc", "select 40 + 2"); err != nil {
		t.Skipf("psql unavailable: %v", err)
	}
	if lines := strings.Split(strings.TrimSpace(h.out.String()), "\n"); lines[len(lines)-1] != "42" {
		t.Fatalf("psql output = %q", h.out.String())
	}
}

// fakeClient makes the database client a shell script, found for any tool name.
func fakeClient(t *testing.T, h *dbHarness, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil { // #nosec G306 -- test client must be executable
		t.Fatal(err)
	}
	inner := h.target
	h.target = func() (*dbtasks.Target, error) {
		target, err := inner()
		if err != nil {
			return nil, err
		}
		target.SetLookPath(func(string) (string, error) { return path, nil })
		return target, nil
	}
}

func TestDBConsoleSurvivesACancelledContext(t *testing.T) {
	t.Parallel()
	// Ctrl-C cancels forge's context; the client handles the keystroke itself
	// and must keep running.
	h := newDBHarness(t, "FORGE_DATABASE_URL=sqlite:storage/dev.db\n")
	fakeClient(t, h, "sleep 0.4\necho client-finished\n")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	if err := h.run(ctx, []string{"console"}); err != nil {
		t.Fatalf("console: %v (the client was killed with the context)", err)
	}
	if !strings.Contains(h.out.String(), "client-finished") {
		t.Fatalf("client output = %q", h.out.String())
	}
}

func TestDBConsoleRemovesTheOptionFileOnHangUp(t *testing.T) {
	// Not parallel: it signals the test process.
	h := newDBHarness(t, "FORGE_DATABASE_URL=mysql://alice:s3cret@127.0.0.1:3306/shop\n")
	dir := t.TempDir()
	recorded, ready := filepath.Join(dir, "option-file"), filepath.Join(dir, "ready")
	fakeClient(t, h, `if [ "$1" = "--version" ]; then echo "mariadb from 11"; exit 0; fi
printf '%s' "${1#--defaults-file=}" > '`+recorded+`'
trap 'exit 0' HUP
: > '`+ready+`'
while :; do sleep 0.05; done
`)
	done := make(chan error, 1)
	go func() { done <- h.run(context.Background(), []string{"console"}) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the client never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	optionFile, err := os.ReadFile(recorded)
	if err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(string(optionFile)); err != nil || !strings.Contains(string(content), "s3cret") {
		t.Fatalf("the option file should hold the password while the client runs: %v", err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil { // the terminal closes
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("console: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("console did not end after the hang-up")
	}
	if _, err := os.Stat(string(optionFile)); !os.IsNotExist(err) {
		t.Fatalf("the option file with the password survived the hang-up (stat err = %v)", err)
	}
}
