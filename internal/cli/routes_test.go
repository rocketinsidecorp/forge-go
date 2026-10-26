package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func writeRoutesProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n\ngo 1.27.1\n")
	writeFile(t, filepath.Join(root, "app", "bootstrap", "bootstrap.go"), `package bootstrap

func routes(app interface {
	HandleFunc(string, any) error
	Resources(string, any, ...any) error
}, pattern string) {
	_ = app.HandleFunc("GET /{$}", nil)
	_ = app.HandleFunc("POST /webhooks", nil)
	_ = app.Resources("/posts", nil)
	_ = app.Resources("/broken/", nil)
	_ = app.HandleFunc(pattern, nil)
}
`)
	return root
}

func TestRunRoutesPrintsATable(t *testing.T) {
	t.Parallel()
	root := writeRoutesProject(t)
	var output bytes.Buffer
	if err := runRoutesAt(nil, &output, io.Discard, root); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(output.String(), "\n")
	var table []string
	for _, line := range lines {
		if line == "" {
			break
		}
		table = append(table, strings.Join(strings.Fields(line), " "))
	}
	want := []string{
		"METHOD PATH ACTION SOURCE",
		"GET /health - forge (built in)",
		"GET /health/live - forge (built in)",
		"GET /health/ready - forge (built in)",
		"GET /openapi.json - forge (built in; development and test, or FORGE_OPENAPI_ENABLED=true)",
		"GET /posts index app/bootstrap/bootstrap.go:9",
		"POST /posts create app/bootstrap/bootstrap.go:9",
		"GET /posts/new new app/bootstrap/bootstrap.go:9",
		"DELETE /posts/{id} destroy app/bootstrap/bootstrap.go:9",
		"GET /posts/{id} show app/bootstrap/bootstrap.go:9",
		"PATCH /posts/{id} update app/bootstrap/bootstrap.go:9",
		"PUT /posts/{id} update app/bootstrap/bootstrap.go:9",
		"GET /posts/{id}/edit edit app/bootstrap/bootstrap.go:9",
		"POST /webhooks - app/bootstrap/bootstrap.go:8",
		"GET /{$} - app/bootstrap/bootstrap.go:7",
	}
	if strings.Join(table, "\n") != strings.Join(want, "\n") {
		t.Fatalf("table:\n%s\nwant:\n%s", strings.Join(table, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(output.String(), "Scope: literal first arguments") || strings.Contains(output.String(), "broken") {
		t.Fatalf("output:\n%s", output.String())
	}
}

func TestRunRoutesRejectsArgumentsAndNonProjects(t *testing.T) {
	t.Parallel()
	if err := runRoutesAt([]string{"--json"}, &bytes.Buffer{}, io.Discard, writeRoutesProject(t)); err == nil {
		t.Fatal("accepted arguments")
	}
	if err := runRoutesAt(nil, &bytes.Buffer{}, io.Discard, t.TempDir()); err == nil || !strings.Contains(err.Error(), "go.mod") {
		t.Fatalf("outside a project: %v", err)
	}
}

func TestRunRoutesWarnsAboutUnparsableSource(t *testing.T) {
	t.Parallel()
	root := writeRoutesProject(t)
	writeFile(t, filepath.Join(root, "app", "broken.go"), "package app\nfunc {\n")
	var output bytes.Buffer
	if err := runRoutesAt(nil, &output, io.Discard, root); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "warning: Some Go files could not be parsed") || !strings.Contains(output.String(), "/posts/{id}/edit") {
		t.Fatalf("output:\n%s", output.String())
	}
}

func TestRunInspectExpandsResources(t *testing.T) {
	t.Parallel()
	root := writeRoutesProject(t)
	var output bytes.Buffer
	if err := runInspectAt([]string{"--json"}, &output, root, nil); err == nil {
		t.Fatal("a project without a Forge import is reported complete")
	}
	var report projectInspection
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(report.Routes, ","); got != "DELETE /posts/{id},GET /posts,GET /posts/new,GET /posts/{id},GET /posts/{id}/edit,GET /{$},PATCH /posts/{id},POST /posts,POST /webhooks,PUT /posts/{id}" {
		t.Fatalf("routes = %s", got)
	}
	if report.RouteScope != routeScope {
		t.Fatalf("scope = %q", report.RouteScope)
	}
}

func TestRoutesAndInspectIgnoreRoutesMountedByTests(t *testing.T) {
	t.Parallel()
	root := writeRoutesProject(t)
	writeFile(t, filepath.Join(root, "app", "bootstrap", "bootstrap_test.go"), `package bootstrap

func newTestServer(mux interface{ Handle(string, any) }) {
	mux.Handle("GET /items/new", nil)
	mux.Handle("DELETE /items/{id}", nil)
}
`)
	var table bytes.Buffer
	if err := runRoutesAt(nil, &table, io.Discard, root); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(table.String(), "/items") || strings.Contains(table.String(), "_test.go") {
		t.Fatalf("forge routes lists routes mounted by tests:\n%s", table.String())
	}
	var output bytes.Buffer
	if err := runInspectAt([]string{"--json"}, &output, root, nil); err == nil {
		t.Fatal("a project without a Forge import is reported complete")
	}
	var report projectInspection
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(report.Routes, ","), "/items") {
		t.Fatalf("forge inspect lists routes mounted by tests: %v", report.Routes)
	}
}
