package dbtasks

import (
	"errors"
	"os"
	"strings"
	"testing"
)

const testPassword = "p@ss\"w'rd\\"

func fakeLookPath(name string) (string, error) { return "/usr/bin/" + name, nil }

func consoleTarget(t *testing.T, databaseURL string, env map[string]string) *Target {
	t.Helper()
	values := map[string]string{"FORGE_DATABASE_URL": databaseURL}
	for key, value := range env {
		values[key] = value
	}
	target, err := NewTarget(t.TempDir(), lookupOf(values))
	if err != nil {
		t.Fatal(err)
	}
	target.SetLookPath(fakeLookPath)
	return target
}

func TestPostgresConsoleKeepsThePasswordOutOfArguments(t *testing.T) {
	t.Setenv("PGPASSWORD", "from-the-shell")
	t.Setenv("PGSERVICE", "other")
	t.Setenv("PGOPTIONS", "-c search_path=evil")
	target := consoleTarget(t, "postgres://alice:p%40ss%22w%27rd%5C@db.internal:5433/shop?sslmode=require", nil)
	console, err := target.PrepareConsole([]string{"-c", "select 1"})
	if err != nil {
		t.Fatal(err)
	}
	defer console.Cleanup()

	if console.Path != "/usr/bin/psql" {
		t.Errorf("path = %q", console.Path)
	}
	if strings.Join(console.Args, " ") != "-X -c select 1" {
		t.Errorf("args = %v", console.Args)
	}
	for _, arg := range console.Args {
		if strings.Contains(arg, "p@ss") || strings.Contains(arg, "alice") {
			t.Errorf("an argument carries credentials: %q", arg)
		}
	}
	env := map[string]string{}
	for _, entry := range console.Env {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}
	if env["PGPASSWORD"] != testPassword || env["PGUSER"] != "alice" || env["PGHOST"] != "db.internal" || env["PGPORT"] != "5433" || env["PGDATABASE"] != "shop" || env["PGSSLMODE"] != "require" {
		t.Errorf("connection variables = %v", env)
	}
	if _, ok := env["PGSERVICE"]; ok {
		t.Error("an inherited PGSERVICE could redirect the client")
	}
	if _, ok := env["PGOPTIONS"]; ok {
		t.Error("an inherited PGOPTIONS could change the session")
	}
}

func TestMySQLConsoleUsesAPrivateOptionFile(t *testing.T) {
	target := consoleTarget(t, "mysql://alice:p%40ss%22w%27rd%5C@db.internal:3307/shop?tls=true", nil)
	console, err := target.PrepareConsole([]string{"-e", "select 1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(console.Args[0], "--defaults-extra-file=") {
		t.Fatalf("the option file must be the first argument: %v", console.Args)
	}
	for _, arg := range console.Args {
		if strings.Contains(arg, "p@ss") || strings.Contains(arg, "alice") {
			t.Errorf("an argument carries credentials: %q", arg)
		}
	}
	for _, entry := range console.Env {
		if strings.Contains(entry, "p@ss") {
			t.Errorf("the environment carries the password: %q", entry)
		}
	}
	name := strings.TrimPrefix(console.Args[0], "--defaults-extra-file=")
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("option file mode = %v, want 0600", info.Mode().Perm())
	}
	content, _ := os.ReadFile(name)
	for _, want := range []string{`user="alice"`, `password="p@ss\"w'rd\\"`, `host="db.internal"`, `port="3307"`, `database="shop"`, "protocol=tcp"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("option file lacks %s:\n%s", want, content)
		}
	}
	console.Cleanup()
	if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Cleanup must remove the option file, stat err = %v", err)
	}
}

func TestMySQLOptionFileSpellsTLSPerFlavor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		flavor, mode, want string
	}{
		{"mysql", "true", "ssl-mode=VERIFY_IDENTITY"},
		{"mysql", "skip-verify", "ssl-mode=REQUIRED"},
		{"mysql", "preferred", "ssl-mode=PREFERRED"},
		{"mysql", "", "ssl-mode=DISABLED"},
		{"mariadb", "true", "ssl-verify-server-cert=1"},
		{"mariadb", "skip-verify", "ssl=1"},
		{"mariadb", "", "ssl=0"},
	}
	for _, test := range tests {
		content, err := mysqlOptionFile(credentials{user: "u", host: "h", params: map[string][]string{"tls": {test.mode}}}, "db", test.flavor)
		if err != nil || !strings.Contains(content, test.want) {
			t.Errorf("%s tls=%q: %v\n%s\nwant %s", test.flavor, test.mode, err, content, test.want)
		}
	}
	if _, err := mysqlOptionFile(credentials{user: "u", password: "a\nb", host: "h"}, "db", "mysql"); err == nil {
		t.Error("a newline in the password would inject option lines")
	}
}

func TestSQLiteConsoleOpensTheFile(t *testing.T) {
	t.Parallel()
	target := consoleTarget(t, "sqlite:storage/dev.db", nil)
	console, err := target.PrepareConsole(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(console.Args) != 1 || console.Args[0] != target.Name || !strings.HasPrefix(console.Args[0], "/") {
		t.Fatalf("args = %v: the path must be absolute so it cannot be read as an option", console.Args)
	}
}

func TestConsoleExplainsAMissingClient(t *testing.T) {
	t.Parallel()
	target := consoleTarget(t, "postgres://u:p@h/db?sslmode=disable", nil)
	target.SetLookPath(func(string) (string, error) { return "", errors.New("not found") })
	if _, err := target.PrepareConsole(nil); err == nil || !strings.Contains(err.Error(), "psql was not found on PATH") {
		t.Fatalf("err = %v", err)
	}
}
