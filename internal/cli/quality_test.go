package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/exectest"
)

// toolsOnPath installs fake executables named after the keys of tools in a
// directory that becomes the whole PATH, so forge finds exactly them. Each
// script logs its arguments to calls, one line per invocation, before body
// runs. The test must not be parallel: PATH and the working directory belong
// to the process.
func toolsOnPath(t *testing.T, tools map[string]string) (calls string) {
	t.Helper()
	bin := t.TempDir()
	calls = filepath.Join(t.TempDir(), "calls")
	for name, body := range tools {
		script := "#!/bin/sh\necho \"" + name + " $*\" >> '" + calls + "'\n" + body + "\n"
		if err := exectest.Write(filepath.Join(bin, name), script); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	return calls
}

func recorded(t *testing.T, calls string) []string {
	t.Helper()
	data, err := os.ReadFile(calls)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// qualityProject is a directory with a go.mod, source files and the
// directories the go tool ignores.
func qualityProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for path, body := range map[string]string{
		"go.mod":                       "module example.com/shop\n",
		"main.go":                      "package main\n",
		"app/app.go":                   "package app\n",
		"app/app_test.go":              "package app\n",
		"vendor/dep/dep.go":            "package dep\n",
		"testdata/fixture.go":          "package fixture\n",
		".hidden/hidden.go":            "package hidden\n",
		"_scratch/scratch.go":          "package scratch\n",
		"nested/go.mod":                "module example.com/nested\n",
		"nested/nested.go":             "package nested\n",
		"app/notes.txt":                "not go\n",
		"cmd/shop/main.go":             "package main\n",
		"app/testdata/more_fixture.go": "package fixture\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCheckBuildsAndVetsTheApplication(t *testing.T) {
	calls := toolsOnPath(t, map[string]string{"go": ""})
	t.Chdir(qualityProject(t))
	var stdout, stderr strings.Builder
	if code := Run([]string{"check"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if got := strings.Join(recorded(t, calls), "|"); got != "go build ./...|go vet ./..." {
		t.Errorf("go was run as %q", got)
	}
	if !strings.Contains(stdout.String(), "check: ok") {
		t.Errorf("stdout %q", stdout.String())
	}
}

func TestCheckStopsAtTheFirstFailingStep(t *testing.T) {
	calls := toolsOnPath(t, map[string]string{"go": `[ "$1" = build ] && { echo "main.go:1: boom" >&2; exit 1; }; exit 0`})
	t.Chdir(qualityProject(t))
	var stdout, stderr strings.Builder
	if code := Run([]string{"check"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "main.go:1: boom") || !strings.Contains(stderr.String(), "go build exited with status 1") {
		t.Errorf("stderr %q", stderr.String())
	}
	if got := recorded(t, calls); len(got) != 1 {
		t.Errorf("vet ran after a failed build: %v", got)
	}
	if strings.Contains(stdout.String(), "check: ok") {
		t.Errorf("a failing check reported ok: %q", stdout.String())
	}
}

func TestQualityCommandsNeedAnApplicationAndTakeNoArguments(t *testing.T) {
	calls := toolsOnPath(t, map[string]string{"go": "", "gofmt": "", "golangci-lint": ""})
	t.Chdir(t.TempDir())
	for _, name := range []string{"check", "format", "lint"} {
		var stdout, stderr strings.Builder
		if code := Run([]string{name}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "no go.mod here") {
			t.Errorf("forge %s outside an application: exit %d, stderr %q", name, code, stderr.String())
		}
		stderr.Reset()
		if code := Run([]string{name, "extra"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage: forge "+name) {
			t.Errorf("forge %s extra: exit %d, stderr %q", name, code, stderr.String())
		}
	}
	if got := recorded(t, calls); len(got) != 0 {
		t.Errorf("a tool ran: %v", got)
	}
}

func TestFormatListsAndRewritesOnlyTheApplicationsGoFiles(t *testing.T) {
	calls := toolsOnPath(t, map[string]string{"gofmt": `echo "$4"; exit 0`})
	t.Chdir(qualityProject(t))
	var stdout, stderr strings.Builder
	if code := Run([]string{"format"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	got := recorded(t, calls)
	if len(got) != 1 || !strings.HasPrefix(got[0], "gofmt -l -w -- ") {
		t.Fatalf("gofmt was run as %v", got)
	}
	files := strings.Fields(strings.TrimPrefix(got[0], "gofmt -l -w -- "))
	want := []string{"app/app.go", "app/app_test.go", "cmd/shop/main.go", "main.go"}
	if strings.Join(files, " ") != strings.Join(want, " ") {
		t.Errorf("gofmt formatted %v, want %v (vendor, testdata, hidden, underscore and nested-module directories are skipped)", files, want)
	}
	// The fake lists its fourth argument, the first file after "--", as rewritten.
	if strings.TrimSpace(stdout.String()) != "app/app.go" {
		t.Errorf("rewritten files %q", stdout.String())
	}
}

func TestFormatCheckListsFilesWritesNothingAndFails(t *testing.T) {
	calls := toolsOnPath(t, map[string]string{"gofmt": `echo "$3"; exit 0`})
	t.Chdir(qualityProject(t))
	var stdout, stderr strings.Builder
	if code := Run([]string{"format", "--check"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1, stderr %q", code, stderr.String())
	}
	if got := recorded(t, calls); len(got) != 1 || !strings.HasPrefix(got[0], "gofmt -l -- app/") || strings.Contains(got[0], "-w") {
		t.Errorf("--check must list without writing: %v", got)
	}
	if strings.TrimSpace(stdout.String()) != "app/app.go" || !strings.Contains(stderr.String(), "1 file(s) need formatting: run `forge format`") {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

func TestFormatCheckPassesWhenNothingNeedsFormatting(t *testing.T) {
	toolsOnPath(t, map[string]string{"gofmt": ""})
	t.Chdir(qualityProject(t))
	var stdout, stderr strings.Builder
	if code := Run([]string{"format", "--check"}, &stdout, &stderr); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestFormatSplitsLongFileListsAndReportsGofmtFailures(t *testing.T) {
	calls := toolsOnPath(t, map[string]string{"gofmt": `[ -n "$GOFMT_FAIL" ] && { echo "syntax error" >&2; exit 2; }; exit 0`})
	root := qualityProject(t)
	for i := range gofmtBatch + 5 {
		if err := os.WriteFile(filepath.Join(root, "app", "gen"+strings.Repeat("x", i%7)+"_"+string(rune('a'+i%26))+strings.Repeat("y", i/26)+".go"), []byte("package app\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)
	var stdout, stderr strings.Builder
	if code := Run([]string{"format"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if got := recorded(t, calls); len(got) < 2 {
		t.Errorf("more than %d files went to gofmt in one call: %d call(s)", gofmtBatch, len(got))
	}
	t.Setenv("GOFMT_FAIL", "1")
	stderr.Reset()
	if code := Run([]string{"format", "--check"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "syntax error") || !strings.Contains(stderr.String(), "gofmt exited with status 2") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestFormatNeedsGofmt(t *testing.T) {
	toolsOnPath(t, nil)
	t.Chdir(qualityProject(t))
	var stdout, stderr strings.Builder
	if code := Run([]string{"format"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "gofmt not found on PATH") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestFormatWithTheRealGofmt(t *testing.T) {
	// The real gofmt is found through the toolchain's own directory.
	goroot := os.Getenv("GOROOT")
	if goroot == "" {
		t.Skip("GOROOT is not set")
	}
	t.Setenv("PATH", filepath.Join(goroot, "bin"))
	if _, err := os.Stat(filepath.Join(goroot, "bin", "gofmt")); err != nil {
		t.Skip("no gofmt in GOROOT")
	}
	root := qualityProject(t)
	messy := filepath.Join(root, "app", "app.go")
	if err := os.WriteFile(messy, []byte("package app\nfunc  F( ) { }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A file named like a flag is a file: gofmt is told where its options end.
	dashed := filepath.Join(root, "-x.go")
	if err := os.WriteFile(dashed, []byte("package main\nfunc  X( ) { }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vendored := filepath.Join(root, "vendor", "dep", "dep.go")
	if err := os.WriteFile(vendored, []byte("package dep\nfunc  G( ) { }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	var stdout, stderr strings.Builder
	if code := Run([]string{"format", "--check"}, &stdout, &stderr); code != 1 || strings.Join(strings.Fields(stdout.String()), " ") != "-x.go app/app.go" {
		t.Fatalf("--check: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if data, _ := os.ReadFile(messy); !strings.Contains(string(data), "func  F") {
		t.Fatalf("--check rewrote the file: %q", data)
	}
	stdout.Reset()
	if code := Run([]string{"format"}, &stdout, &stderr); code != 0 || strings.Join(strings.Fields(stdout.String()), " ") != "-x.go app/app.go" {
		t.Fatalf("format: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if data, _ := os.ReadFile(messy); string(data) != "package app\n\nfunc F() {}\n" {
		t.Errorf("formatted file %q", data)
	}
	if data, _ := os.ReadFile(dashed); string(data) != "package main\n\nfunc X() {}\n" {
		t.Errorf("the file named -x.go was not formatted: %q", data)
	}
	if data, _ := os.ReadFile(vendored); !strings.Contains(string(data), "func  G") {
		t.Errorf("vendored code was reformatted: %q", data)
	}
	stdout.Reset()
	if code := Run([]string{"format", "--check"}, &stdout, &stderr); code != 0 || stdout.Len() != 0 {
		t.Errorf("a formatted tree failed --check: exit %d, stdout %q", code, stdout.String())
	}
}

func TestLintRunsGolangciLintOnTheApplication(t *testing.T) {
	calls := toolsOnPath(t, map[string]string{"golangci-lint": ""})
	t.Chdir(qualityProject(t))
	var stdout, stderr strings.Builder
	if code := Run([]string{"lint"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if got := strings.Join(recorded(t, calls), "|"); got != "golangci-lint run ./..." {
		t.Errorf("golangci-lint was run as %q", got)
	}
	if !strings.Contains(stdout.String(), "lint: ok") {
		t.Errorf("stdout %q", stdout.String())
	}
}

func TestLintFailsWhenTheLinterReportsIssues(t *testing.T) {
	toolsOnPath(t, map[string]string{"golangci-lint": `echo "main.go:1:1: bad (revive)"; exit 1`})
	t.Chdir(qualityProject(t))
	var stdout, stderr strings.Builder
	if code := Run([]string{"lint"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "main.go:1:1: bad (revive)") || strings.Contains(stdout.String(), "lint: ok") || !strings.Contains(stderr.String(), "golangci-lint exited with status 1") {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

func TestLintNamesHowToInstallAMissingGolangciLint(t *testing.T) {
	toolsOnPath(t, map[string]string{"go": ""})
	t.Chdir(qualityProject(t))
	var stdout, stderr strings.Builder
	if code := Run([]string{"lint"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	for _, want := range []string{"golangci-lint not found on PATH", "go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v", "golangci-lint.run"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q: %q", want, stderr.String())
		}
	}
}

func TestQualityCommandsHaveHelp(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"check", "format", "lint"} {
		var stdout, stderr strings.Builder
		if code := Run([]string{name, "--help"}, &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "usage: forge "+name) {
			t.Errorf("forge %s --help: exit %d, stdout %q, stderr %q", name, code, stdout.String(), stderr.String())
		}
	}
	var stdout, stderr strings.Builder
	if code := Run([]string{"format", "--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "--check") {
		t.Errorf("forge format --help lacks --check: %q", stdout.String())
	}
}
