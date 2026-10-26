package cli

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestRoutesListsTheJSONRoutesOfGeneratedScaffolds proves what the source scan
// missed (audit C-5): the /v1/<plural> routes of a resource are built from
// package constants and a loop, so only running the composed application sees
// them. The application is a generated SQLite scaffold with its wiring pasted
// in, so no database server is needed.
//
// It is not parallel: the build of the generated application resolves
// dependencies offline from the module cache.
func TestRoutesListsTheJSONRoutesOfGeneratedScaffolds(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on a generated project")
	}
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOPROXY", "off")
	dir := matrixApplicationFor(t, "sqlite", "scaffold", scaffoldRequests())
	var specs []resourceSpec
	for _, args := range scaffoldRequests() {
		request, err := parseGeneratorArgs(args, scaffoldUsage)
		if err != nil {
			t.Fatal(err)
		}
		request.Scaffold = true
		specs = append(specs, resourceSpec{resourceRequest: request, Module: "example.com/mx", Database: "sqlite", BrowserPipeline: true})
	}
	wireResources(t, dir, specs)

	var output, stderr bytes.Buffer
	if err := runRoutesAt(nil, &output, &stderr, dir); err != nil {
		t.Fatalf("forge routes: %v\n%s", err, output.String())
	}
	table := output.String()
	if stderr.Len() != 0 {
		t.Fatalf("forge routes fell back to the source scan:\n%s\n%s", stderr.String(), table)
	}
	// Columns are padded to the longest cell: compare the cells only.
	var rows []string
	for _, line := range strings.Split(table, "\n") {
		rows = append(rows, strings.Join(strings.Fields(line), " "))
	}
	for _, want := range []string{
		"GET /v1/products - app/adapters/httpapi/product.go:",
		"POST /v1/products - app/adapters/httpapi/product.go:",
		"GET /v1/products/{id} - app/adapters/httpapi/product.go:",
		"PATCH /v1/products/{id} - app/adapters/httpapi/product.go:",
		"DELETE /v1/products/{id} - app/adapters/httpapi/product.go:",
		"GET /v1/categories - app/adapters/httpapi/category.go:",
		"DELETE /v1/categories/{id} - app/adapters/httpapi/category.go:",
		// The HTML pages come from a Resources call, with their action.
		"GET /products index app/adapters/htmlui/product.go:",
		"PUT /products/{id} update app/adapters/htmlui/product.go:",
		"GET /health/ready - forge (built in)",
		"GET /openapi.json - forge (built in; development and test, or FORGE_OPENAPI_ENABLED=true)",
	} {
		if !slices.ContainsFunc(rows, func(row string) bool { return strings.HasPrefix(row, want) }) {
			t.Errorf("the table has no row starting %q:\n%s", want, table)
		}
	}

	// With a second binary, forge routes needs a NAME to choose one; without
	// it, it says so on stderr and lists what a source scan finds.
	writeFile(t, filepath.Join(dir, "cmd", "worker", "main.go"), readFile(t, filepath.Join(dir, "cmd", "mx", "main.go")))
	var ambiguousOut, ambiguousErr bytes.Buffer
	if err := runRoutesAt(nil, &ambiguousOut, &ambiguousErr, dir); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ambiguousErr.String(), "several commands found (mx, worker): pass one name") || strings.Contains(ambiguousOut.String(), "several commands") {
		t.Errorf("without a NAME: stdout=%s stderr=%s", ambiguousOut.String(), ambiguousErr.String())
	}
	for _, name := range []string{"mx", "worker"} {
		var named, namedErr bytes.Buffer
		if err := runRoutesAt([]string{name}, &named, &namedErr, dir); err != nil {
			t.Fatalf("forge routes %s: %v", name, err)
		}
		if namedErr.Len() != 0 || !strings.Contains(strings.Join(strings.Fields(named.String()), " "), "GET /v1/products - app/adapters/httpapi/product.go:") {
			t.Errorf("forge routes %s: stderr=%q\n%s", name, namedErr.String(), named.String())
		}
	}
}

// TestRoutesFallsBackToTheSourceScanWhenTheApplicationCannotRun keeps
// forge routes useful, and honest about what it misses, for an application that
// does not build or does not configure. The explanation goes to stderr, so
// stdout is only the table and `forge routes | grep /v1` stays clean.
func TestRoutesFallsBackToTheSourceScanWhenTheApplicationCannotRun(t *testing.T) {
	t.Parallel()
	root := writeRoutesProject(t)
	writeFile(t, root+"/cmd/shop/main.go", "package main\n\nfunc main() { undefined() }\n")
	var stdout, stderr bytes.Buffer
	if err := runRoutesAt(nil, &stdout, &stderr, root); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/posts", "/webhooks", "METHOD", "Scope:"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "note:") || strings.Contains(stdout.String(), "could not list its routes") {
		t.Errorf("the fallback note is on stdout:\n%s", stdout.String())
	}
	for _, want := range []string{"note: the application could not list its routes (build: ", "misses routes whose paths are built from constants or loops", "Fix that error, or"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
	if strings.Contains(stderr.String(), "METHOD") {
		t.Errorf("the table is on stderr:\n%s", stderr.String())
	}
}

func TestRoutesNameMustBeAPlainArgumentForAnExistingCommand(t *testing.T) {
	t.Parallel()
	root := writeRoutesProject(t)
	writeFile(t, root+"/cmd/shop/main.go", "package main\n\nfunc main() {}\n")
	var stdout, stderr bytes.Buffer
	for _, args := range [][]string{{"--json"}, {"shop", "worker"}} {
		if err := runRoutesAt(args, &stdout, &stderr, root); err == nil || !strings.Contains(err.Error(), "usage: forge routes [NAME]") {
			t.Errorf("forge routes %v: %v", args, err)
		}
	}
	// A name that does not exist is an error, not a reason to scan the source.
	if err := runRoutesAt([]string{"nope"}, &stdout, &stderr, root); err == nil || !strings.Contains(err.Error(), "cmd/nope/main.go not found") {
		t.Errorf("a missing command: %v", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("a rejected command wrote output: %q %q", stdout.String(), stderr.String())
	}
}
