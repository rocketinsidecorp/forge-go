package cli

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestRunDispatchesEveryCommandAndReportsItsFailure(t *testing.T) {
	// Not parallel: the commands work in the current directory, an empty one.
	t.Chdir(t.TempDir())
	for _, command := range []string{
		"version", "uuid", "doctor", "inspect", "routes", "openapi", "new", "dev", "build", "test", "db", "migrate",
		"rollback", "setup", "seed", "generate", "destroy", "deploy", "logs", "app", "credentials", "i18n", "help",
	} {
		var stdout, stderr bytes.Buffer
		code := Run([]string{command, "--no-such-flag", "extra"}, &stdout, &stderr)
		if code == 0 {
			t.Errorf("forge %s --no-such-flag extra succeeded (stdout %q)", command, stdout.String())
		}
		if !strings.Contains(stderr.String(), "forge: ") && !strings.Contains(stderr.String(), "flag provided but not defined") && !strings.Contains(stderr.String(), "usage") {
			t.Errorf("forge %s: stderr %q names no error", command, stderr.String())
		}
	}
}

func TestRunHelpFlagsAndMissingCommand(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	for _, flag := range []string{"-h", "--help"} {
		stdout.Reset()
		if code := Run([]string{flag}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "usage: forge") {
			t.Errorf("%s: exit %d, %q", flag, code, stdout.String())
		}
	}
	stderr.Reset()
	if code := Run(nil, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage: forge") {
		t.Errorf("no command: exit %d, %q", code, stderr.String())
	}
}

func TestRunExitCodesOfCommandsThatReportAStatus(t *testing.T) {
	// Not parallel: the working directory.
	t.Chdir(t.TempDir())
	var stdout, stderr bytes.Buffer
	// An environment file that cannot be read: doctor reports it in JSON and
	// exits 1 without a second message.
	if err := writeTestFile(".env.development", "not valid\n"); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"doctor", "--json"}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), `"environment_file.invalid"`) || stderr.Len() != 0 {
		t.Errorf("doctor --json: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	// Without go.mod inspection is incomplete: exit 1, the report on stdout.
	if code := Run([]string{"inspect", "--json"}, &stdout, &stderr); code != 1 || stdout.Len() == 0 {
		t.Errorf("inspect: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestDoctorExplainsEachProblemInTextAndJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		env  []string
		args []string
		want string
	}{
		"text usage":              {nil, []string{"--yaml"}, "usage: forge doctor [--json]"},
		"text invalid config":     {[]string{"FORGE_ENV=moon"}, nil, "configuration invalid"},
		"json invalid config":     {[]string{"FORGE_ENV=moon"}, []string{"--json"}, `"configuration.invalid"`},
		"json without a database": {nil, []string{"--json"}, `"database":"none"`},
		"text healthy":            {[]string{"FORGE_DATABASE_URL=postgres://u:pw@db.internal/shop?sslmode=disable"}, nil, "database: postgres://u@db.internal/shop?sslmode=disable"},
		"json healthy":            {[]string{"FORGE_DATABASE_URL=sqlite:storage/dev.db"}, []string{"--json"}, `"database":"sqlite"`},
	} {
		var out bytes.Buffer
		err := runDoctorAt(tc.args, &out, dir, tc.env)
		if !strings.Contains(out.String()+errText(err), tc.want) {
			t.Errorf("%s: output %q, err %v, want %q", name, out.String(), err, tc.want)
		}
		if strings.Contains(out.String(), "pw") {
			t.Errorf("%s: the password reached the output: %q", name, out.String())
		}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestDescribeDatabaseKeepsOnlyWhatIsSafe(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"://broken": "configured (unparseable)",
		"postgres://u:secret@db:5432/shop?password=x&sslmode=require": "postgres://u@db:5432/shop?sslmode=require",
		"mysql://u:secret@db/shop?tls=skip-verify&foo=bar":            "mysql://u@db/shop?tls=skip-verify",
		"sqlite:storage/dev.db":                                       "sqlite:storage/dev.db",
	} {
		if got := describeDatabase(raw); got != want {
			t.Errorf("describeDatabase(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestRunUUIDWritesTheRequestedNumberOfIdentifiers(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	if code := Run([]string{"uuid", "-count", "3"}, &out, &errOut); code != 0 || len(strings.Fields(out.String())) != 3 {
		t.Errorf("exit %d, %q", code, out.String())
	}
	if err := runUUID([]string{"-count", "2"}, failingWriter{}, &errOut); err == nil {
		t.Error("a failing output must fail the command")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errBrokenPipe }

var errBrokenPipe = errors.New("write: broken pipe")

func writeTestFile(name, content string) error { return os.WriteFile(name, []byte(content), 0o600) }
