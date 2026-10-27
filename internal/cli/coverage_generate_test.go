package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var generateNow = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// limitedWriter accepts okWrites writes, then fails every one.
type limitedWriter struct {
	okWrites int
	written  bytes.Buffer
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.okWrites <= 0 {
		return 0, errBrokenPipe
	}
	w.okWrites--
	return w.written.Write(p)
}

func TestGenerateWrappersWorkInTheCurrentDirectory(t *testing.T) {
	// Not parallel: the working directory.
	root := resourceProject(t, postgresURL)
	t.Chdir(root)
	var out bytes.Buffer
	if err := runGenerate([]string{"migration", "add_prices", "--dry-run"}, &out); err != nil || !strings.Contains(out.String(), "would create  db/migrations/") {
		t.Errorf("runGenerate: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := runGenerateAt([]string{"view", "welcome", "--dry-run"}, &out, root); err != nil || !strings.Contains(out.String(), "would create  app/views/pages/welcome.gohtml") {
		t.Errorf("runGenerateAt: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := runGenerateAtTime([]string{"job", "nightly_report", "--dry-run"}, &out, root, generateNow); err != nil || !strings.Contains(out.String(), "app/jobs/nightly_report.go") {
		t.Errorf("runGenerateAtTime: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := runDestroy([]string{"migration", "x"}, &out); err == nil {
		t.Error("runDestroy accepted a malformed command")
	}
	if err := runRoutes(nil, &out, io.Discard); err != nil {
		t.Errorf("runRoutes: %v", err)
	}
	out.Reset()
	if err := runInspect([]string{"--json"}, &out); err != nil && err != errInspectIncomplete {
		t.Errorf("runInspect: %v", err)
	}
	if !strings.Contains(out.String(), `"kind":"forge.application"`) {
		t.Errorf("runInspect output: %s", out.String())
	}
	out.Reset()
	if err := runDoctor(nil, &out); err != nil || !strings.Contains(out.String(), "configuration: ok") {
		t.Errorf("runDoctor: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := runI18n([]string{"check", "--json"}, &out); err == nil || !strings.Contains(err.Error(), "config/locales") {
		t.Errorf("runI18n in a project without translations: %v", err)
	}
}

func TestGeneratorsNeedAnApplication(t *testing.T) {
	t.Parallel()
	gone := filepath.Join(t.TempDir(), "gone")
	noModule := t.TempDir()
	for name, args := range map[string][]string{
		"migration":      {"migration", "add_prices"},
		"view":           {"view", "welcome"},
		"job":            {"job", "nightly"},
		"queue job":      {"job", "nightly", "--queue"},
		"model":          {"model", "post", "title:string", "--global"},
		"resource":       {"resource", "post", "title:string", "--global"},
		"scaffold":       {"scaffold", "post", "title:string", "--global"},
		"controller":     {"controller", "pages", "index"},
		"mailer":         {"mailer", "welcome", "greet"},
		"authentication": {"authentication"},
	} {
		var out bytes.Buffer
		if err := runGenerateEnv(args, &out, gone, nil, generateNow); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("%s in a missing directory: %v", name, err)
		}
		if err := runGenerateEnv(args, &out, noModule, nil, generateNow); err == nil {
			t.Errorf("%s without go.mod succeeded", name)
		}
	}
	if err := os.WriteFile(filepath.Join(noModule, "go.mod"), []byte("go 1.27.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"queue-job"}, {"mailer", "welcome", "greet"}, {"job", "nightly", "--queue"}} {
		var out bytes.Buffer
		if err := runGenerateEnv(args, &out, noModule, nil, generateNow); err == nil {
			t.Errorf("%v with a go.mod without a module succeeded", args)
		}
	}
}

func TestGeneratorsRejectMalformedCommandLines(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, postgresURL)
	for name, args := range map[string][]string{
		"model without a name":          {"model"},
		"model with a bad flag":         {"model", "post", "--wat"},
		"model broadcast":               {"model", "post", "title:string", "--global", "--broadcast"},
		"resource without a name":       {"resource"},
		"resource too many fields":      append([]string{"resource", "post", "--global"}, fieldList(maxResourceFields+1)...),
		"resource plural is a filename": {"resource", "post", "title:string", "--global", "--plural", "posts_test"},
		"scaffold without a name":       {"scaffold"},
		"controller without a name":     {"controller"},
		"mailer without a name":         {"mailer"},
		"queue job without a name":      {"job", "--queue"},
		"queue job bad flag":            {"job", "x", "--queue", "--wat"},
		"job extra argument":            {"job", "nightly", "extra", "more"},
		"unknown generator":             {"widget", "x"},
		"nothing":                       nil,
	} {
		var out bytes.Buffer
		if err := runGenerateEnv(args, &out, root, nil, generateNow); err == nil {
			t.Errorf("%s: forge generate %v succeeded", name, args)
		}
		if out.Len() != 0 {
			t.Errorf("%s: output %q", name, out.String())
		}
	}
}

func fieldList(n int) []string {
	fields := make([]string, n)
	for i := range fields {
		fields[i] = "f" + strings.Repeat("a", i%5) + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + ":string"
	}
	return fields
}

func TestGeneratorsReportAnOutputThatCannotBeWritten(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		args   []string
		writes int // writes that succeed before the failure
	}{
		"migration dry run":  {[]string{"migration", "add_prices", "--dry-run"}, 0},
		"view":               {[]string{"view", "welcome"}, 6},
		"view dry run":       {[]string{"view", "welcome", "--dry-run"}, 0},
		"job first note":     {[]string{"job", "nightly_report"}, 2},
		"job second note":    {[]string{"job", "nightly_report"}, 3},
		"resource dry run":   {[]string{"resource", "post", "title:string", "--global", "--dry-run"}, 0},
		"model dry run":      {[]string{"model", "post", "title:string", "--global", "--dry-run"}, 0},
		"mailer dry run":     {[]string{"mailer", "welcome", "greet", "--dry-run"}, 0},
		"queue job dry run":  {[]string{"job", "nightly_report", "--queue", "--dry-run"}, 0},
		"controller dry run": {[]string{"controller", "pages", "index", "--dry-run"}, 0},
		"scaffold dry run":   {[]string{"scaffold", "post", "title:string", "--global", "--dry-run"}, 0},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := resourceProject(t, postgresURL)
			out := &limitedWriter{okWrites: tc.writes}
			err := runGenerateEnv(tc.args, out, root, nil, generateNow)
			if err == nil {
				t.Skipf("forge generate %v wrote fewer than %d lines: nothing to fail", tc.args, tc.writes+1)
			}
			if !errors.Is(err, errBrokenPipe) {
				t.Errorf("err = %v, want the write error", err)
			}
		})
	}
}

func TestGeneratorsRefuseWhatCannotBeInspected(t *testing.T) {
	t.Parallel()
	t.Run("the migrations directory is a symlink", func(t *testing.T) {
		t.Parallel()
		root := resourceProject(t, postgresURL)
		if err := os.RemoveAll(filepath.Join(root, "db", "migrations")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(root, "db", "migrations")); err != nil {
			t.Skipf("symlinks are not available: %v", err)
		}
		var out bytes.Buffer
		if err := runGenerateEnv([]string{"migration", "add_prices"}, &out, root, nil, generateNow); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("db is a file", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n")
		writeFile(t, filepath.Join(root, "db"), "a file")
		var out bytes.Buffer
		if err := runGenerateEnv([]string{"migration", "add_prices"}, &out, root, nil, generateNow); err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("app is a file", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n")
		writeFile(t, filepath.Join(root, "app"), "a file")
		var out bytes.Buffer
		for _, args := range [][]string{{"view", "welcome"}, {"job", "nightly"}, {"job", "nightly", "--queue"}} {
			if err := runGenerateEnv(args, &out, root, nil, generateNow); err == nil {
				t.Errorf("%v with app as a file succeeded", args)
			}
		}
	})
	t.Run("the generated files exist already", func(t *testing.T) {
		t.Parallel()
		root := resourceProject(t, postgresURL)
		var out bytes.Buffer
		for _, args := range [][]string{{"view", "welcome"}, {"job", "nightly"}} {
			if err := runGenerateEnv(args, &out, root, nil, generateNow); err != nil {
				t.Fatal(err)
			}
			if err := runGenerateEnv(args, &out, root, nil, generateNow); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
				t.Errorf("%v again: %v", args, err)
			}
		}
	})
	t.Run("a broken environment file", func(t *testing.T) {
		t.Parallel()
		root := resourceProject(t, postgresURL)
		writeFile(t, filepath.Join(root, ".env.development"), "bad line\n")
		for _, args := range [][]string{
			{"resource", "post", "title:string", "--global"},
			{"model", "post", "title:string", "--global"},
			{"controller", "pages", "index"},
			{"scaffold", "post", "title:string", "--global"},
		} {
			var out bytes.Buffer
			env := []string{"FORGE_UNRELATED=1"}
			err := runGenerateEnv(args, &out, root, env, generateNow)
			if err == nil || !strings.Contains(err.Error(), "environment files could not be loaded") {
				t.Errorf("forge generate %v: %v", args, err)
			}
		}
	})
}
