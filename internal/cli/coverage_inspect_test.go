package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const forgeMain = "package main\n\nimport _ \"github.com/rocketinsidecorp/forge-go\"\n\nfunc main() {}\n"

func inspectCodes(report projectInspection) string {
	var codes []string
	for _, d := range report.Diagnostics {
		codes = append(codes, d.Code)
	}
	return strings.Join(codes, ",")
}

func TestInspectReportsEachWayAProjectCannotBeRead(t *testing.T) {
	t.Parallel()
	good := "module example.com/shop\n\ngo 1.24\n"
	tests := map[string]struct {
		setup func(t *testing.T, root string)
		want  string
	}{
		"no such directory": {nil, "project.unreadable"},
		"no go.mod":         {func(t *testing.T, root string) {}, "project.go_mod_missing"},
		"go.mod is a directory": {func(t *testing.T, root string) {
			mkdir(t, filepath.Join(root, "go.mod"))
		}, "project.go_mod_unreadable"},
		"go.mod without a module": {func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "go.mod"), "go 1.24\n")
		}, "project.go_mod_invalid"},
		"go.mod too large": {func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "go.mod"), good+strings.Repeat("// padding\n", maxInspectFile/10))
		}, "project.go_mod_too_large"},
		"cmd is a file": {func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "go.mod"), good)
			writeFile(t, filepath.Join(root, "cmd"), "not a directory")
		}, "project.commands_unreadable"},
		"migrations is a file": {func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "go.mod"), good)
			writeFile(t, filepath.Join(root, "db"), "not a directory")
		}, "project.migrations_unreadable"},
		"a source file that does not parse": {func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "go.mod"), good)
			writeFile(t, filepath.Join(root, "app", "broken.go"), "package app\nfunc (")
		}, "project.source_parse_error"},
		"environment file with a bad line": {func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "go.mod"), good)
			writeFile(t, filepath.Join(root, ".env.development"), "not valid\n")
		}, "environment_file.invalid"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "project")
			if tc.setup != nil {
				mkdir(t, root)
				tc.setup(t, root)
			}
			report := inspectProject(root, nil)
			if report.Complete || !strings.Contains(inspectCodes(report), tc.want) {
				t.Errorf("complete %v, diagnostics %q, want %q", report.Complete, inspectCodes(report), tc.want)
			}
			var out bytes.Buffer
			if err := runInspectAt([]string{"--json"}, &out, root, nil); err != errInspectIncomplete {
				t.Errorf("runInspectAt = %v, want errInspectIncomplete", err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("the JSON report lacks %q: %s", tc.want, out.String())
			}
		})
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
}

func TestInspectSourceLimits(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n\ngo 1.24\n")
	writeFile(t, filepath.Join(root, "cmd", "shop", "main.go"), forgeMain)
	big := "package app\n// " + strings.Repeat("x", maxInspectFile) + "\n"
	writeFile(t, filepath.Join(root, "app", "a_big.go"), big)
	writeFile(t, filepath.Join(root, "app", "b_big.go"), big) // the same finding is reported once
	report := inspectProject(root, nil)
	if report.Complete || inspectCodes(report) != "project.scan_limit" {
		t.Errorf("oversized files: complete %v, diagnostics %q", report.Complete, inspectCodes(report))
	}
	for _, d := range report.Diagnostics {
		if !strings.Contains(d.Summary, "could not be read within the inspection limits") {
			t.Errorf("summary %q", d.Summary)
		}
	}

	many := t.TempDir()
	writeFile(t, filepath.Join(many, "go.mod"), "module example.com/shop\n\ngo 1.24\n")
	writeFile(t, filepath.Join(many, "cmd", "shop", "main.go"), forgeMain)
	for i := range maxInspectGoFiles + 1 {
		writeFile(t, filepath.Join(many, "app", fmt.Sprintf("f%04d.go", i)), "package app\n")
	}
	report = inspectProject(many, nil)
	if report.Complete || !strings.Contains(inspectCodes(report), "project.scan_limit") {
		t.Errorf("too many files: complete %v, diagnostics %q", report.Complete, inspectCodes(report))
	}
	if report.Diagnostics[0].Code != "project.scan_limit" || !strings.Contains(report.Diagnostics[0].Summary, "file limit") {
		t.Errorf("diagnostics %+v", report.Diagnostics)
	}
}

func TestInspectListsCommandsAndKeepsPlainFilesOutOfThem(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n\ngo 1.24\n")
	writeFile(t, filepath.Join(root, "cmd", "shop", "main.go"), forgeMain)
	writeFile(t, filepath.Join(root, "cmd", "README.md"), "a file, not a command")
	mkdir(t, filepath.Join(root, "cmd", "empty"))
	report := inspectProject(root, nil)
	if strings.Join(report.Commands, ",") != "shop" || !report.Complete {
		t.Errorf("commands %v, complete %v (%q)", report.Commands, report.Complete, inspectCodes(report))
	}
}

func TestDatabaseAdapterOfTheEnvironment(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for value, want := range map[string]string{
		"":                      "none",
		"no-scheme":             "unknown",
		"POSTGRES://u@h/db":     "postgres",
		"postgresql://u@h/db":   "postgres",
		"mysql://u@h/db":        "mysql",
		"mariadb://u@h/db":      "unknown",
		"sqlite:storage/dev.db": "sqlite",
		"oracle://u@h/db":       "unknown",
	} {
		got, err := databaseAdapter(root, []string{"FORGE_DATABASE_URL=" + value})
		if err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", value, got, err, want)
		}
	}
	if got, err := databaseAdapter(root, nil); err != nil || got != "none" {
		t.Errorf("unset: %q, %v", got, err)
	}
	writeFile(t, filepath.Join(root, ".env.development"), "bad line\n")
	if got, err := databaseAdapter(root, nil); err == nil || got != "unknown" {
		t.Errorf("a broken environment file: %q, %v", got, err)
	}
}

func TestInspectOutputFailureIsReported(t *testing.T) {
	t.Parallel()
	if err := runInspectAt([]string{"--json"}, failingWriter{}, t.TempDir(), nil); err == nil || err == errInspectIncomplete {
		t.Errorf("err = %v, want the write error", err)
	}
}
