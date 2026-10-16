package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInspectJSONInventoriesForgeProjectWithoutLeakingEnvironment(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), `module example.com/shop

go 1.27.1

require github.com/rocketinsidecorp/forge-go v0.0.0
replace github.com/rocketinsidecorp/forge-go => ../forge-go
`)
	writeFile(t, filepath.Join(root, "cmd", "shop", "main.go"), `package main
import (
	"net/http"
	"github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/jobs"
)
func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(http.ResponseWriter, *http.Request) {})
	_ = forge.New
	_ = jobs.New
}

`)
	writeFile(t, filepath.Join(root, "app", "routes.go"), `package app
import "github.com/rocketinsidecorp/forge-go/views"
func routes(mux interface{ Handle(string, interface{}) }) {
	mux.Handle("GET /items", nil)
	mux.Handle("GET /items", nil)
	_ = views.New
}
`)
	writeFile(t, filepath.Join(root, "db", "migrations", "001_create_items.up.sql"), "CREATE TABLE items (id text);\n")
	writeFile(t, filepath.Join(root, "db", "migrations", "001_create_items.down.sql"), "DROP TABLE items;\n")
	writeFile(t, filepath.Join(root, ".env.development"), "FORGE_DATABASE_URL=postgres://alice:private-secret@private-host/shop\n")

	var output bytes.Buffer
	if err := runInspectAt([]string{"--json"}, &output, root, nil); err != nil {
		t.Fatalf("inspect: %v\n%s", err, output.String())
	}
	var report projectInspection
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("decode inspection: %v\n%s", err, output.String())
	}
	if !report.Complete || report.Module != "example.com/shop" || report.GoVersion != "1.27.1" || report.FrameworkVersion != "v0.0.0" {
		t.Fatalf("unexpected project metadata: %+v", report)
	}
	if report.Database != "postgres" || strings.Contains(output.String(), "private-secret") || strings.Contains(output.String(), "private-host") {
		t.Fatalf("database inventory leaked configuration or chose wrong adapter: %s", output.String())
	}
	if len(report.Commands) != 1 || report.Commands[0] != "shop" || len(report.Migrations) != 1 || report.Migrations[0] != "001_create_items.up.sql" {
		t.Fatalf("unexpected commands/migrations: %+v", report)
	}
	if strings.Join(report.Routes, ",") != "GET /health,GET /items" {
		t.Fatalf("routes should be unique and sorted: %v", report.Routes)
	}
	if strings.Join(report.Modules, ",") != "core,jobs,views" {
		t.Fatalf("modules should be unique and sorted: %v", report.Modules)
	}
}

func TestRunInspectJSONDoesNotModifyProjectFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\ngo 1.27.1\nrequire github.com/rocketinsidecorp/forge-go v0.0.0\n")
	writeFile(t, filepath.Join(root, "app", "routes.go"), "package app\n")
	before := inspectFixtureSnapshot(t, root)

	var output bytes.Buffer
	if err := runInspectAt([]string{"--json"}, &output, root, nil); err != nil {
		t.Fatalf("inspect: %v\n%s", err, output.String())
	}
	if after := inspectFixtureSnapshot(t, root); !equalStringMap(before, after) {
		t.Fatalf("read-only inspection changed the project tree:\nbefore=%v\nafter=%v", before, after)
	}
}

func inspectFixtureSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if entry.IsDir() {
			files[relative+string(filepath.Separator)] = ""
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[relative] = string(content)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot fixture: %v", err)
	}
	return files
}

func equalStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if got, ok := right[key]; !ok || got != value {
			return false
		}
	}
	return true
}

func TestRunInspectJSONReturnsPartialReportForInvalidEnvironment(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\ngo 1.27.1\n")
	writeFile(t, filepath.Join(root, ".env.local"), "not-a-secret-safe-assignment private-secret\n")
	var output bytes.Buffer
	err := runInspectAt([]string{"--json"}, &output, root, nil)
	if !errors.Is(err, errInspectIncomplete) {
		t.Fatalf("error = %v, want incomplete inspection", err)
	}
	if strings.Contains(output.String(), "private-secret") {
		t.Fatalf("inspection leaked malformed environment value: %s", output.String())
	}
	var report projectInspection
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("decode partial inspection: %v\n%s", err, output.String())
	}
	if report.Complete || report.Database != "unknown" || len(report.Diagnostics) != 2 {
		t.Fatalf("unexpected partial report: %+v", report)
	}
	if report.Diagnostics[0].Code != "environment_file.invalid" {
		t.Fatalf("missing stable environment diagnostic: %+v", report.Diagnostics)
	}
}

func TestRunInspectJSONMarksSourceParseErrorsIncomplete(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\ngo 1.27.1\nrequire github.com/rocketinsidecorp/forge-go v0.0.0\n")
	writeFile(t, filepath.Join(root, "app", "broken.go"), "package app\nfunc broken(\n")
	var output bytes.Buffer
	err := runInspectAt([]string{"--json"}, &output, root, nil)
	if !errors.Is(err, errInspectIncomplete) {
		t.Fatalf("error = %v, want incomplete inspection", err)
	}
	var report projectInspection
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("decode partial inspection: %v\n%s", err, output.String())
	}
	if report.Complete || !hasInspectCode(report.Diagnostics, "project.source_parse_error") {
		t.Fatalf("source parse error was not disclosed: %+v", report)
	}
}

func TestRunInspectRejectsNonJSONArguments(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{}, {"--yaml"}, {"--json", "extra"}} {
		var output bytes.Buffer
		if err := runInspectAt(args, &output, t.TempDir(), nil); err == nil || output.Len() != 0 {
			t.Fatalf("args=%v err=%v output=%q; want usage error without report", args, err, output.String())
		}
	}
}

func TestRunInspectBoundsGoModRead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n"+strings.Repeat("# oversized\n", maxInspectFile/8))
	var output bytes.Buffer
	err := runInspectAt([]string{"--json"}, &output, root, nil)
	if !errors.Is(err, errInspectIncomplete) {
		t.Fatalf("error = %v, want incomplete inspection", err)
	}
	var report projectInspection
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("decode bounded inspection: %v\n%s", err, output.String())
	}
	if !hasInspectCode(report.Diagnostics, "project.go_mod_too_large") {
		t.Fatalf("oversized go.mod did not produce a safe diagnostic: %+v", report)
	}
}

func TestRunInspectIncompleteKeepsMachineReadableReportOnStdout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/plain\ngo 1.27.1\n")
	var stdout, stderr bytes.Buffer
	err := runInspectAt([]string{"--json"}, &stdout, root, nil)
	if !errors.Is(err, errInspectIncomplete) || stderr.Len() != 0 {
		t.Fatalf("err=%v stderr=%q", err, stderr.String())
	}
	var report projectInspection
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("expected machine-readable partial report: %v\n%s", err, stdout.String())
	}
	if report.Complete || !hasInspectCode(report.Diagnostics, "project.not_forge") {
		t.Fatalf("plain Go project should be reported as non-Forge: %+v", report)
	}
}

func hasInspectCode(diagnostics []doctorDiagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
