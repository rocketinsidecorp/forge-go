package dbtasks

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/dbtest"
)

// scriptTools installs shell scripts as the vendor tools of target and
// returns the files they append their arguments and standard input to.
func scriptTools(t *testing.T, target *Target, scripts map[string]string) (argsFile, stdinFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile, stdinFile = filepath.Join(dir, "args"), filepath.Join(dir, "stdin")
	paths := map[string]string{}
	for name, body := range scripts {
		path := filepath.Join(dir, name)
		script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + argsFile + "'\ncat >> '" + stdinFile + "'\n" + body + "\n"
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil { // #nosec G306 -- test script must be executable
			t.Fatal(err)
		}
		paths[name] = path
	}
	target.SetLookPath(func(name string) (string, error) {
		if path, ok := paths[name]; ok {
			return path, nil
		}
		return "", exec.ErrNotFound
	})
	return argsFile, stdinFile
}

const unreachablePostgres = "postgres://app:pw@127.0.0.1:1/shop?sslmode=disable&connect_timeout=2"
const unreachableMySQL = "mysql://app:pw@127.0.0.1:1/shop"

func TestServerCommandsReportAnUnreachableServer(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ url, want string }{
		"postgres": {unreachablePostgres, "connect to the PostgreSQL server"},
		"mysql":    {unreachableMySQL, "connect to the MySQL server"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			target := consoleTarget(t, tc.url, nil)
			ctx := context.Background()
			if exists, err := target.Exists(ctx); err == nil || exists || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Exists = %v, %v", exists, err)
			}
			if created, err := target.Create(ctx); err == nil || created || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Create = %v, %v", created, err)
			}
			if dropped, err := target.Drop(ctx); err == nil || dropped || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Drop = %v, %v", dropped, err)
			}
			if err := target.LoadSchema(ctx, filepath.Join(t.TempDir(), "schema.sql")); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("LoadSchema = %v", err)
			}
		})
	}
}

func TestDumpReportsAnUnreachableServerAfterTheToolRan(t *testing.T) {
	t.Parallel()
	pg := consoleTarget(t, unreachablePostgres, nil)
	scriptTools(t, pg, map[string]string{"pg_dump": "echo 'CREATE TABLE t (id int);'"})
	if _, err := pg.DumpSchema(context.Background()); err == nil || !strings.Contains(err.Error(), "read applied migrations") || !strings.Contains(err.Error(), "connect to the database") {
		t.Errorf("postgres: %v", err)
	}
	my := consoleTarget(t, unreachableMySQL, nil)
	scriptTools(t, my, map[string]string{"mysqldump": "echo 'CREATE TABLE t (id int) AUTO_INCREMENT=7;'"})
	if _, err := my.DumpSchema(context.Background()); err == nil || !strings.Contains(err.Error(), "read applied migrations") || !strings.Contains(err.Error(), "connect to the MySQL server") {
		t.Errorf("mysql: %v", err)
	}
}

func TestPostgresDumpExplainsAServerVersionMismatch(t *testing.T) {
	t.Parallel()
	target := consoleTarget(t, "postgres://app:pw@db.internal/shop", nil)
	scriptTools(t, target, map[string]string{"pg_dump": "echo 'pg_dump: error: aborting because of server version mismatch' >&2; exit 1"})
	_, err := target.DumpSchema(context.Background())
	if err == nil || !strings.Contains(err.Error(), "server version mismatch") || !strings.Contains(err.Error(), "pg_dump of the server's major version or newer") {
		t.Fatalf("err = %v", err)
	}
}

func TestToolFailuresCarryTheirDiagnosticsUpToALimit(t *testing.T) {
	t.Parallel()
	target := consoleTarget(t, "sqlite:storage/dev.db", nil)
	scriptTools(t, target, map[string]string{"sqlite3": "head -c 6000 /dev/zero | tr '\\0' 'x' >&2; exit 3"})
	_, err := target.DumpSchema(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sqlite3 failed") || !strings.HasSuffix(err.Error(), strings.Repeat("x", maxToolOutput)+"...") {
		n := 0
		if err != nil {
			n = len(err.Error())
		}
		t.Fatalf("err (%d bytes) = %v", n, err)
	}
	if strings.Contains(err.Error(), strings.Repeat("x", maxToolOutput+1)) {
		t.Error("the diagnostics were not cut")
	}
	quiet := consoleTarget(t, "sqlite:storage/dev.db", nil)
	scriptTools(t, quiet, map[string]string{"sqlite3": "exit 4"})
	if _, err := quiet.DumpSchema(context.Background()); err == nil || !strings.Contains(err.Error(), "sqlite3 failed: exit status 4") || strings.Contains(err.Error(), "\n") {
		t.Errorf("a tool without diagnostics: %v", err)
	}
}

func TestSQLiteDumpRejectsRowsItCannotTrust(t *testing.T) {
	t.Parallel()
	sum := strings.Repeat("ab", 32)
	for name, tc := range map[string]struct{ script, want string }{
		"tracking query fails":          {`case "$*" in *sqlite_master*) echo boom >&2; exit 1;; esac`, "read applied migrations"},
		"rows query fails":              {`case "$*" in *sqlite_master*) echo forge_schema_migrations;; *"SELECT version"*) echo nope >&2; exit 1;; esac`, "read applied migrations"},
		"short row":                     {`case "$*" in *sqlite_master*) echo forge_schema_migrations;; *"SELECT version"*) echo 'only|two';; esac`, "unexpected migration row"},
		"version is not a number":       {`case "$*" in *sqlite_master*) echo forge_schema_migrations;; *"SELECT version"*) echo 'x|name|` + sum + `';; esac`, "unexpected migration version"},
		"name that must not be written": {`case "$*" in *sqlite_master*) echo forge_schema_migrations;; *"SELECT version"*) echo "1|Bad;Name|` + sum + `";; esac`, "will not write into a schema file"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			target := consoleTarget(t, "sqlite:storage/dev.db", nil)
			scriptTools(t, target, map[string]string{"sqlite3": tc.script})
			if _, err := target.DumpSchema(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSQLiteLoadSchemaRunsTheFileInOneTransaction(t *testing.T) {
	t.Parallel()
	target := consoleTarget(t, "sqlite:storage/dev.db", nil)
	_, stdinFile := scriptTools(t, target, map[string]string{"sqlite3": "true"})
	ctx := context.Background()
	schema := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(schema, []byte("CREATE TABLE items (id TEXT);"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := target.LoadSchema(ctx, schema); err == nil || !strings.Contains(err.Error(), "the database does not exist") {
		t.Fatalf("loading into a missing database: %v", err)
	}
	if _, err := target.Create(ctx); err != nil {
		t.Fatal(err)
	}
	if err := target.LoadSchema(ctx, filepath.Join(t.TempDir(), "absent.sql")); err == nil || !strings.Contains(err.Error(), "open schema file") {
		t.Fatalf("a missing schema file: %v", err)
	}
	if err := target.LoadSchema(ctx, schema); err != nil {
		t.Fatal(err)
	}
	stdin, _ := os.ReadFile(stdinFile)
	if got := string(stdin); got != "BEGIN;\nCREATE TABLE items (id TEXT);\nCOMMIT;\n" {
		t.Errorf("the tool read %q", got)
	}
	// A tool failure is wrapped.
	failing := consoleTarget(t, "sqlite:storage/other.db", nil)
	scriptTools(t, failing, map[string]string{"sqlite3": "echo 'Parse error' >&2; exit 1"})
	if _, err := failing.Create(ctx); err != nil {
		t.Fatal(err)
	}
	if err := failing.LoadSchema(ctx, schema); err == nil || !strings.Contains(err.Error(), "load schema: sqlite3 failed") || !strings.Contains(err.Error(), "Parse error") {
		t.Errorf("a failing load: %v", err)
	}
	// A path that is not a regular file is not a database.
	dir := consoleTarget(t, "sqlite:storage", nil)
	if err := os.MkdirAll(dir.Name, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := dir.LoadSchema(ctx, schema); err == nil || !strings.Contains(err.Error(), "is not a regular file") {
		t.Errorf("a directory: %v", err)
	}
}

func TestConsolesNeedTheirClientAndAValidOptionFile(t *testing.T) {
	t.Parallel()
	for _, rawURL := range []string{"postgres://app:pw@db.internal/shop", "mysql://app:pw@db.internal/shop", "sqlite:storage/dev.db"} {
		target := consoleTarget(t, rawURL, nil)
		target.SetLookPath(func(string) (string, error) { return "", exec.ErrNotFound })
		if console, err := target.PrepareConsole(nil); err == nil || console != nil || !strings.Contains(err.Error(), "was not found on PATH") {
			t.Errorf("%s: console %v, err %v", rawURL, console, err)
		}
	}
	// A control character cannot be written into the option file.
	target := consoleTarget(t, "mysql://app:pw%0Aevil@db.internal/shop", nil)
	if _, err := target.PrepareConsole(nil); err == nil || !strings.Contains(err.Error(), "control character") {
		t.Errorf("a password with a newline: %v", err)
	}
	if _, err := target.DumpSchema(context.Background()); err == nil || !strings.Contains(err.Error(), "control character") {
		t.Errorf("dump with a password with a newline: %v", err)
	}
}

func TestMySQLOptionFileCannotBeWrittenWithoutATemporaryDirectory(t *testing.T) {
	// Not parallel: it changes TMPDIR for the process.
	target := consoleTarget(t, "mysql://app:pw@db.internal/shop", nil)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does-not-exist"))
	if console, err := target.PrepareConsole(nil); err == nil || console != nil || !strings.Contains(err.Error(), "create option file") {
		t.Fatalf("console %v, err %v", console, err)
	}
}

func TestConsoleCommandRunsTheClientWithItsArgumentsAndEnvironment(t *testing.T) {
	t.Parallel()
	target := consoleTarget(t, "sqlite:storage/dev.db", nil)
	scriptTools(t, target, map[string]string{"sqlite3": `echo "client:$FORGE_CONSOLE_TEST:$1:$2"`})
	console, err := target.PrepareConsole([]string{"select 1"})
	if err != nil {
		t.Fatal(err)
	}
	defer console.Cleanup()
	console.Env = append(console.Env, "FORGE_CONSOLE_TEST=yes")
	out, err := console.Command(context.Background()).Output()
	if err != nil || string(out) != "client:yes:"+target.Name+":select 1\n" {
		t.Fatalf("output %q, err %v", out, err)
	}
	// The command is bound to its context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := console.Command(ctx).Run(); err == nil {
		t.Error("a cancelled context must stop the client")
	}
}

// --- SQLite files ---

func TestSQLiteFileOperationsReportWhatWentWrong(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	file := filepath.Join(root, "plain-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	under := consoleTarget(t, "sqlite:"+filepath.Join(file, "dev.db"), nil)
	ctx := context.Background()
	if exists, err := under.Exists(ctx); err == nil || exists || strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("Exists below a file: %v, %v", exists, err)
	}
	if created, err := under.Create(ctx); err == nil || created || !strings.Contains(err.Error(), "create database directory") {
		t.Errorf("Create below a file: %v, %v", created, err)
	}
	tooLong := consoleTarget(t, "sqlite:"+filepath.Join(root, strings.Repeat("n", 300)+".db"), nil)
	if created, err := tooLong.Create(ctx); err == nil || created || !strings.Contains(err.Error(), "create database file") {
		t.Errorf("Create with a name beyond the file system's limit: %v, %v", created, err)
	}
	if dropped, err := tooLong.Drop(ctx); err == nil || dropped {
		t.Errorf("Drop of a name beyond the limit: %v, %v", dropped, err)
	}
}

func TestSQLiteDropReportsACompanionItCannotRemove(t *testing.T) {
	t.Parallel()
	target := consoleTarget(t, "sqlite:storage/dev.db", nil)
	ctx := context.Background()
	if _, err := target.Create(ctx); err != nil {
		t.Fatal(err)
	}
	// A non-empty directory cannot be removed like a file.
	blocked := target.Name + "-wal"
	if err := os.MkdirAll(filepath.Join(blocked, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	dropped, err := target.Drop(ctx)
	if err == nil || !dropped || !strings.Contains(err.Error(), "remove dev.db-wal") {
		t.Fatalf("Drop = %v, %v", dropped, err)
	}
	if _, statErr := os.Stat(target.Name); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("the database file itself is gone before the companions are handled")
	}
}

// --- target ---

func TestURLChecksNameTheVariableAndNeverQuoteTheURL(t *testing.T) {
	t.Parallel()
	if _, err := parseURL("://secret-password@host"); err == nil || err.Error() != "the database URL is not valid" {
		t.Errorf("parseURL: %v", err)
	}
	err := refuseLocationParams(&url.URL{Scheme: "postgres", Host: "h", RawQuery: "sslmode=disable&application_name=a b"}, "FORGE_DATABASE_URL")
	if err == nil || !strings.Contains(err.Error(), "FORGE_DATABASE_URL has whitespace or a control character in its parameters") || strings.Contains(err.Error(), "a b") {
		t.Errorf("whitespace in the query: %v", err)
	}
	if !isLocalHost(nil) {
		t.Error("no URL is local: a SQLite target has none")
	}
}

func TestPgConnConfigRefusesAPortPgxWouldReadDifferently(t *testing.T) {
	t.Parallel()
	target := &Target{priv: &url.URL{Scheme: "postgres", Host: "localhost:99999", Path: "/shop"}}
	if _, err := target.pgConnConfig("shop"); err == nil || !strings.Contains(err.Error(), "invalid port") {
		t.Fatalf("err = %v", err)
	}
}

// --- real servers ---

// mysqlTarget returns a target for a throwaway database on the test server,
// dropped when the test ends.
func mysqlTarget(t *testing.T) *Target {
	t.Helper()
	server := dbtest.ServerURL(t, config.AdapterMySQL)
	parsed, err := url.Parse(server)
	if err != nil {
		t.Fatal("the MySQL test URL is not valid")
	}
	parsed.Path = "/" + dbtest.DatabaseName()
	target, err := NewTarget(t.TempDir(), lookupOf(map[string]string{"FORGE_DATABASE_URL": parsed.String()}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
		defer cancel()
		_, _ = target.Drop(ctx)
	})
	return target
}

func requireMySQLTools(t *testing.T) {
	t.Helper()
	for _, name := range []string{"mysql", "mysqldump"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is not installed", name)
		}
	}
}

const mysqlTestSchema = `
CREATE TABLE forge_schema_migrations (version BIGINT PRIMARY KEY, name VARCHAR(255) NOT NULL, checksum CHAR(64) NOT NULL, applied_at BIGINT NOT NULL);
CREATE TABLE items (id CHAR(36) PRIMARY KEY, tenant_id CHAR(36) NOT NULL, name VARCHAR(100) NOT NULL AUTO_INCREMENT_PLACEHOLDER) ENGINE=InnoDB;
CREATE INDEX items_tenant_idx ON items (tenant_id, name);
INSERT INTO forge_schema_migrations VALUES (20260101000000, 'create_items', '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef', 1767225600000000);
INSERT INTO forge_schema_migrations VALUES (20260102000000, 'add_price', 'fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210', 1767225600000001);
`

func execMySQL(t *testing.T, target *Target, statements ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
	defer cancel()
	db, err := target.mysqlConnect(ctx, target.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func TestMySQLSchemaDumpIsDeterministicAndLoads(t *testing.T) {
	t.Parallel()
	requireMySQLTools(t)
	source := mysqlTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*dbtest.SetupTimeout)
	defer cancel()
	if _, err := source.Create(ctx); err != nil {
		t.Fatal(err)
	}
	// A database without the migration table dumps without its history.
	empty, err := source.DumpSchema(ctx)
	if err != nil || strings.Contains(string(empty), "Applied migrations") {
		t.Fatalf("empty dump: %v\n%s", err, empty)
	}
	for _, statement := range strings.Split(strings.ReplaceAll(mysqlTestSchema, " AUTO_INCREMENT_PLACEHOLDER", ""), ";\n") {
		if strings.TrimSpace(statement) != "" {
			execMySQL(t, source, statement)
		}
	}
	execMySQL(t, source, "CREATE TABLE counters (id INT AUTO_INCREMENT PRIMARY KEY, label VARCHAR(20))", "INSERT INTO counters (label) VALUES ('a'), ('b'), ('c')")

	first, err := source.DumpSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	execMySQL(t, source, "INSERT INTO counters (label) VALUES ('d')") // moves AUTO_INCREMENT: normalized away
	second, err := source.DumpSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("two dumps of the same schema differ:\n--- first\n%s\n--- second\n%s", first, second)
	}
	text := string(first)
	for _, want := range []string{
		"CREATE TABLE `items`", "items_tenant_idx", "CREATE TABLE `counters`",
		"VALUES (20260101000000, 'create_items', '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef', 0);",
		"VALUES (20260102000000, 'add_price',",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("dump lacks %q", want)
		}
	}
	if strings.Contains(text, "AUTO_INCREMENT=") {
		t.Error("the dump carries an AUTO_INCREMENT counter")
	}

	schemaFile := filepath.Join(t.TempDir(), "schema.sql")
	if err := writeFile(schemaFile, first); err != nil {
		t.Fatal(err)
	}
	clone := mysqlTarget(t)
	if err := clone.LoadSchema(ctx, schemaFile); err == nil || !strings.Contains(err.Error(), "the database does not exist") {
		t.Fatalf("LoadSchema into a missing database: %v", err)
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
	if err := clone.LoadSchema(ctx, schemaFile); err == nil || !strings.Contains(err.Error(), "load schema") {
		t.Fatalf("loading into a database that has the schema must fail: %v", err)
	}
}

func TestMySQLDumpRefusesAMigrationTableItCannotRead(t *testing.T) {
	t.Parallel()
	requireMySQLTools(t)
	target := mysqlTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*dbtest.SetupTimeout)
	defer cancel()
	if _, err := target.Create(ctx); err != nil {
		t.Fatal(err)
	}
	// version is text: scanning it into a number fails.
	execMySQL(t, target,
		"CREATE TABLE forge_schema_migrations (version VARCHAR(20), name VARCHAR(255), checksum CHAR(64))",
		"INSERT INTO forge_schema_migrations VALUES ('not-a-number', 'x', 'y')")
	if _, err := target.DumpSchema(ctx); err == nil || !strings.Contains(err.Error(), "read applied migrations") {
		t.Fatalf("err = %v", err)
	}
	// A table without the expected columns fails the query itself.
	execMySQL(t, target, "DROP TABLE forge_schema_migrations", "CREATE TABLE forge_schema_migrations (id INT)")
	if _, err := target.DumpSchema(ctx); err == nil || !strings.Contains(err.Error(), "read applied migrations") {
		t.Fatalf("a table with other columns: %v", err)
	}
}

func TestPostgresDumpOfAnEmptyDatabaseAndAnUnreadableMigrationTable(t *testing.T) {
	t.Parallel()
	target, _ := pgTarget(t)
	requirePGTools(t, target)
	ctx, cancel := context.WithTimeout(context.Background(), 2*dbtest.SetupTimeout)
	defer cancel()
	if _, err := target.Create(ctx); err != nil {
		t.Fatal(err)
	}
	empty, err := target.DumpSchema(ctx)
	if err != nil || strings.Contains(string(empty), "Applied migrations") {
		t.Fatalf("empty dump: %v\n%s", err, empty)
	}
	conn, err := target.pgConnect(ctx, target.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	exec := func(sql string) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec("CREATE TABLE forge_schema_migrations (version TEXT, name TEXT, checksum TEXT)")
	exec("INSERT INTO forge_schema_migrations VALUES ('not-a-number', 'x', 'y')")
	if _, err := target.DumpSchema(ctx); err == nil || !strings.Contains(err.Error(), "read applied migrations") {
		t.Fatalf("a version that is not a number: %v", err)
	}
	exec("DROP TABLE forge_schema_migrations")
	exec("CREATE TABLE forge_schema_migrations (id int)")
	if _, err := target.DumpSchema(ctx); err == nil || !strings.Contains(err.Error(), "read applied migrations") {
		t.Fatalf("a table with other columns: %v", err)
	}
}
