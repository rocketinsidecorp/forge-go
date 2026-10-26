package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/dbtasks"
	"github.com/rocketinsidecorp/forge-go/internal/exectest"
)

const sqliteEnv = "FORGE_DATABASE_URL=sqlite:storage/dev.db\n"

// withTools makes every database client of the harness's target the shell
// script body, which sees its arguments in $@ and its standard input.
func withTools(h *dbHarness, tools func(name string) string) {
	inner := h.target
	dir := h.root
	h.target = func() (*dbtasks.Target, error) {
		target, err := inner()
		if err != nil {
			return nil, err
		}
		target.SetLookPath(func(name string) (string, error) {
			body := tools(name)
			if body == "" {
				return "", exec.ErrNotFound
			}
			path := filepath.Join(dir, ".tools", name)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return "", err
			}
			return path, exectest.Write(path, "#!/bin/sh\n"+body+"\n")
		})
		return target, nil
	}
}

func TestDBEveryCommandReportsATargetThatCannotBeResolved(t *testing.T) {
	t.Parallel()
	boom := errors.New("cannot read the environment")
	for _, args := range [][]string{
		{"create"}, {"drop"}, {"reset"}, {"setup"}, {"prepare"}, {"rollback"}, {"schema:dump"}, {"schema:load"}, {"console"},
	} {
		h := newDBHarness(t, sqliteEnv)
		h.target = func() (*dbtasks.Target, error) { return nil, boom }
		if err := h.do(args...); !errors.Is(err, boom) {
			t.Errorf("forge db %v: err = %v, want the resolution error", args, err)
		}
		if len(h.app.calls) != 0 {
			t.Errorf("forge db %v reached the application: %v", args, h.app.calls)
		}
	}
}

func TestDBReportsDatabaseFailuresOfEachStep(t *testing.T) {
	t.Parallel()
	// A file where the database directory should be: creating, existence
	// checks and removal all fail.
	blocked := func(t *testing.T) *dbHarness {
		h := newDBHarness(t, sqliteEnv)
		if err := os.WriteFile(filepath.Join(h.root, "storage"), []byte("a file"), 0o600); err != nil {
			t.Fatal(err)
		}
		return h
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"create":  {[]string{"create"}, "create database directory"},
		"setup":   {[]string{"setup"}, "create database directory"},
		"prepare": {[]string{"prepare"}, "not a directory"},
	} {
		h := blocked(t)
		if err := h.do(tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
		if len(h.app.calls) != 0 {
			t.Errorf("%s: the application was reached after a failed database step: %v", name, h.app.calls)
		}
	}
	// A directory where the database file should be.
	h := newDBHarness(t, sqliteEnv)
	if err := os.MkdirAll(filepath.Join(h.root, "storage", "dev.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"drop"}, {"reset"}} {
		if err := h.do(args...); err == nil || !strings.Contains(err.Error(), "is not a regular file") {
			t.Errorf("forge db %v: err = %v", args, err)
		}
	}
}

func TestDBDelegatingCommandsReportAnApplicationThatCannotBeBuilt(t *testing.T) {
	t.Parallel()
	boom := errors.New("build: exit status 1")
	for _, args := range [][]string{{"migrate"}, {"status"}, {"seed"}, {"rollback"}} {
		h := newDBHarness(t, sqliteEnv)
		h.newApp = func(context.Context) (appRunner, error) { return nil, boom }
		if err := h.do(args...); !errors.Is(err, boom) {
			t.Errorf("forge db %v: err = %v", args, err)
		}
	}
	// The application is built once and reused by later steps of one run.
	h := newDBHarness(t, sqliteEnv)
	builds := 0
	h.newApp = func(context.Context) (appRunner, error) { builds++; return h.app, nil }
	if err := h.do("setup"); err != nil {
		t.Fatal(err)
	}
	if builds != 1 || strings.Join(h.app.calls, ",") != "migrate,seed" {
		t.Errorf("builds %d, calls %v", builds, h.app.calls)
	}
}

func TestDBRollbackConsumesItsSafetyFlagsAndPassesTheRest(t *testing.T) {
	t.Parallel()
	h := newDBHarness(t, sqliteEnv)
	if err := h.do("rollback", "--force-production", "--step", "2", "--yes"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.app.calls, ","); got != "rollback --step 2" {
		t.Errorf("the application got %q", got)
	}
	// In production the flags are what the guard checks.
	prod := newDBHarness(t, productionEnv)
	if err := prod.do("rollback"); err == nil || !strings.Contains(err.Error(), "refusing to roll back") {
		t.Errorf("rollback in production without flags: %v", err)
	}
	if len(prod.app.calls) != 0 {
		t.Errorf("a refused rollback reached the application: %v", prod.app.calls)
	}
	prod.process["FORGE_ALLOW_DESTRUCTIVE"] = "true"
	if err := prod.do("rollback", "--force-production", "--yes"); err != nil {
		t.Errorf("confirmed rollback: %v", err)
	}
}

func TestDBSchemaDumpAndLoadReportToolAndFileFailures(t *testing.T) {
	t.Parallel()
	t.Run("the vendor tool is missing", func(t *testing.T) {
		t.Parallel()
		h := newDBHarness(t, sqliteEnv)
		withTools(h, func(string) string { return "" })
		if err := h.do("schema:dump"); err == nil || !strings.Contains(err.Error(), "sqlite3 was not found on PATH") {
			t.Errorf("dump: %v", err)
		}
	})
	t.Run("the schema file cannot be written", func(t *testing.T) {
		t.Parallel()
		h := newDBHarness(t, sqliteEnv)
		withTools(h, func(string) string {
			return `case "$*" in *sqlite_master*) ;; *) echo "CREATE TABLE t (id INTEGER);";; esac`
		})
		if err := os.WriteFile(filepath.Join(h.root, "db"), []byte("a file"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := h.do("schema:dump"); err == nil {
			t.Error("dump into a directory that is a file must fail")
		}
		if err := os.Remove(filepath.Join(h.root, "db")); err != nil {
			t.Fatal(err)
		}
		if err := h.do("schema:dump"); err != nil {
			t.Fatal(err)
		}
		written, err := os.ReadFile(filepath.Join(h.root, "db", "schema.sql"))
		if err != nil || !strings.Contains(string(written), "CREATE TABLE t") {
			t.Errorf("schema file %q, %v", written, err)
		}
		if matches, _ := filepath.Glob(filepath.Join(h.root, "db", ".schema-*")); len(matches) != 0 {
			t.Errorf("temporary files left behind: %v", matches)
		}
	})
	t.Run("loading needs a dumped schema and a database", func(t *testing.T) {
		t.Parallel()
		h := newDBHarness(t, sqliteEnv)
		withTools(h, func(string) string { return "cat > /dev/null" })
		if err := h.do("schema:load"); err == nil || !strings.Contains(err.Error(), "db/schema.sql not found: run forge db schema:dump first") {
			t.Errorf("no schema file: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(h.root, "db"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h.root, "db", "schema.sql"), []byte("CREATE TABLE t (id INTEGER);"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := h.do("schema:load"); err == nil || !strings.Contains(err.Error(), "the database does not exist") {
			t.Errorf("no database: %v", err)
		}
		if err := h.do("create"); err != nil {
			t.Fatal(err)
		}
		if err := h.do("schema:load"); err != nil || !strings.Contains(h.out.String(), "loaded db/schema.sql into") {
			t.Errorf("load: %v\n%s", err, h.out.String())
		}
	})
	t.Run("the vendor tool fails while loading", func(t *testing.T) {
		t.Parallel()
		h := newDBHarness(t, sqliteEnv)
		withTools(h, func(string) string { return "echo 'Parse error near line 1' >&2; exit 1" })
		if err := os.MkdirAll(filepath.Join(h.root, "db"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h.root, "db", "schema.sql"), []byte("bad"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := h.do("create"); err != nil {
			t.Fatal(err)
		}
		if err := h.do("schema:load"); err == nil || !strings.Contains(err.Error(), "Parse error near line 1") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestDBConsoleFailureModes(t *testing.T) {
	t.Parallel()
	t.Run("client arguments need the separator", func(t *testing.T) {
		t.Parallel()
		h := newDBHarness(t, sqliteEnv)
		if err := h.do("console", "select 1"); err == nil || !strings.Contains(err.Error(), "usage: forge db console") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("the client is not installed", func(t *testing.T) {
		t.Parallel()
		h := newDBHarness(t, sqliteEnv)
		withTools(h, func(string) string { return "" })
		if err := h.do("console"); err == nil || !strings.Contains(err.Error(), "sqlite3 was not found on PATH") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("the client cannot start", func(t *testing.T) {
		t.Parallel()
		h := newDBHarness(t, sqliteEnv)
		inner := h.target
		h.target = func() (*dbtasks.Target, error) {
			target, err := inner()
			if err != nil {
				return nil, err
			}
			notExecutable := filepath.Join(h.root, "client")
			if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0o600); err != nil {
				return nil, err
			}
			target.SetLookPath(func(string) (string, error) { return notExecutable, nil })
			return target, nil
		}
		if err := h.do("console"); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("the client exits with a status", func(t *testing.T) {
		t.Parallel()
		h := newDBHarness(t, sqliteEnv)
		withTools(h, func(string) string { return "echo 'no such table' >&2; exit 3" })
		err := h.do("console", "--", "-c", "select 1")
		if err == nil || err.Error() != "sqlite3 exited with status 3" {
			t.Errorf("err = %v", err)
		}
		if !strings.Contains(h.errOut.String(), "no such table") {
			t.Errorf("the client's stderr was not forwarded: %q", h.errOut.String())
		}
	})
}

func TestWriteFileAtomicReplacesTheFileOrLeavesItAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "db", "schema.sql")
	if err := writeFileAtomic(path, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("two")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "two" {
		t.Errorf("content %q", got)
	}
	// A directory in the way: the rename fails and no temporary file stays.
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(blocked, []byte("x")); err == nil {
		t.Error("replacing a non-empty directory must fail")
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".schema-*")); len(matches) != 0 {
		t.Errorf("temporary files left behind: %v", matches)
	}
	if err := writeFileAtomic(filepath.Join(dir, "blocked", "child", "x", "y"), nil); err != nil {
		t.Errorf("a nested directory is created: %v", err)
	}
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(file, "sub", "schema.sql"), nil); err == nil {
		t.Error("a path below a regular file must fail")
	}
}

// --- the application binary ---

// fakeProject writes a minimal Forge-shaped module whose application prints
// its arguments, and fails with the status of its first argument "exit=N".
func fakeProject(t *testing.T, name, source string) string {
	t.Helper()
	root := t.TempDir()
	for file, content := range map[string]string{
		"go.mod":                              "module example.com/" + name + "\n\ngo 1.24\n",
		filepath.Join("cmd", name, "main.go"): source,
	} {
		path := filepath.Join(root, file)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const echoApp = `package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func main() {
	fmt.Println("args:", strings.Join(os.Args[1:], " "), "env:", os.Getenv("FORGE_ENV"))
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "exit=") {
		n, _ := strconv.Atoi(strings.TrimPrefix(os.Args[1], "exit="))
		os.Exit(n)
	}
}
`

func TestBuildAppRunnerBuildsAndRunsTheApplicationBinary(t *testing.T) {
	t.Parallel()
	root := fakeProject(t, "shop", echoApp)
	var out, errOut strings.Builder
	app, err := buildAppRunner(context.Background(), root, &out, &errOut)
	if err != nil {
		t.Fatalf("buildAppRunner: %v\n%s", err, errOut.String())
	}
	built := app.(*builtApp)
	if _, err := os.Stat(built.binary); err != nil {
		t.Fatalf("the binary was not built: %v", err)
	}
	if err := app.Run(context.Background(), []string{"migrate", "status"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "args: migrate status env: development\n" {
		t.Errorf("application output %q", got)
	}
	err = app.Run(context.Background(), []string{"exit=7"})
	if err == nil || err.Error() != `application command "exit=7" failed with status 7` {
		t.Errorf("a failing command: %v", err)
	}
	missing := &builtApp{root: root, dir: built.dir, binary: filepath.Join(built.dir, "missing")}
	if err := missing.Run(context.Background(), []string{"x"}); err == nil || strings.Contains(err.Error(), "failed with status") {
		t.Errorf("a binary that cannot start: %v", err)
	}
	app.Close()
	if _, err := os.Stat(built.dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Close left the build directory: %v", err)
	}
}

func TestBuildAppRunnerReportsWhatPreventsTheBuild(t *testing.T) {
	t.Parallel()
	broken := fakeProject(t, "shop", "package main\n\nfunc main() { undefined() }\n")
	var errOut strings.Builder
	if app, err := buildAppRunner(context.Background(), broken, &strings.Builder{}, &errOut); err == nil || app != nil || !strings.HasPrefix(err.Error(), "build: ") || !strings.Contains(errOut.String(), "undefined") {
		t.Errorf("a compile error: app %v, err %v, output %q", app, err, errOut.String())
	}
	if _, err := buildAppRunner(context.Background(), t.TempDir(), &strings.Builder{}, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "no go.mod here") {
		t.Errorf("outside an application: %v", err)
	}
	badEnv := fakeProject(t, "shop", echoApp)
	if err := os.WriteFile(filepath.Join(badEnv, ".env.development"), []byte("not a valid line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildAppRunner(context.Background(), badEnv, &strings.Builder{}, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), ".env.development") {
		t.Errorf("an invalid environment file: %v", err)
	}
}

func TestRunDBWiresTheRealTargetAndApplication(t *testing.T) {
	// Not parallel: forge db works in the current directory.
	root := fakeProject(t, "shop", echoApp)
	if err := os.WriteFile(filepath.Join(root, ".env.development"), []byte(sqliteEnv), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	var out, errOut strings.Builder
	if err := runDB([]string{"create"}, &out, &errOut); err != nil || !strings.Contains(out.String(), "created sqlite database") {
		t.Fatalf("create: %v\n%s%s", err, out.String(), errOut.String())
	}
	out.Reset()
	if err := runDB([]string{"migrate"}, &out, &errOut); err != nil || !strings.Contains(out.String(), "args: migrate") {
		t.Fatalf("migrate: %v\n%s%s", err, out.String(), errOut.String())
	}
	if err := runDB(nil, &out, &errOut); err == nil || !strings.Contains(err.Error(), "usage: forge db") {
		t.Errorf("no subcommand: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env.development"), []byte("broken line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runDB([]string{"create"}, &out, &errOut); err == nil || !strings.Contains(err.Error(), ".env.development") {
		t.Errorf("an invalid environment file: %v", err)
	}
}
