package cli

import (
	"bytes"
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

	var output bytes.Buffer
	if err := runRoutesAt(nil, &output, dir); err != nil {
		t.Fatalf("forge routes: %v\n%s", err, output.String())
	}
	table := output.String()
	if strings.Contains(table, "note: the application could not list its routes") {
		t.Fatalf("forge routes fell back to the source scan:\n%s", table)
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
}

// TestRoutesFallsBackToTheSourceScanWhenTheApplicationCannotRun keeps
// forge routes useful, and honest about what it misses, for an application that
// does not build or does not configure.
func TestRoutesFallsBackToTheSourceScanWhenTheApplicationCannotRun(t *testing.T) {
	t.Parallel()
	root := writeRoutesProject(t)
	writeFile(t, root+"/cmd/shop/main.go", "package main\n\nfunc main() { undefined() }\n")
	var output bytes.Buffer
	if err := runRoutesAt(nil, &output, root); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/posts", "/webhooks", "note: the application could not list its routes (build: ", "misses routes whose paths are built from constants or loops"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("the output lacks %q:\n%s", want, output.String())
		}
	}
}
