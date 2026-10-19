package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scaffoldWiredTest is added to the generated application after the printed
// wiring is pasted into bootstrap.go. It proves what the scaffold promises
// about the real composition root: the pages exist, and nobody can reach them
// or submit their forms without the guard and the CSRF token.
const scaffoldWiredTest = `package bootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/%[1]s/%[2]s"
)

func TestScaffoldPagesAreNotPublic(t *testing.T) {
	cfg := config.Default()
	cfg.Database = %[2]s.Config(t)
%[3]s	app, err := forge.New(forge.WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := Configure(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close(context.Background()) })

	for _, path := range []string{"/products", "/products/new", "/products/0190b2a1-7c3e-7000-8000-000000000001", "/categories", "/categories/new"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Accept", "text/html")
		recorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized && recorder.Code != http.StatusForbidden {
			t.Errorf("GET %%s = %%d, want 401 or 403 without credentials: %%s", path, recorder.Code, recorder.Body)
		}
		if strings.Contains(recorder.Body.String(), "<form") {
			t.Errorf("GET %%s served a form to an unauthenticated caller", path)
		}
	}
	// A form post without the CSRF token never reaches the handler.
	post := httptest.NewRequest(http.MethodPost, "/categories", strings.NewReader(url.Values{"name": {"x"}}.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, post)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "csrf") {
		t.Errorf("POST without a token = %%d %%s, want the CSRF rejection", recorder.Code, recorder.Body)
	}
}
`

func scaffoldRequests() [][]string {
	return [][]string{
		{
			"product", "name:string", "summary:string?", "body:text", "notes:text?", "quantity:int", "limit_count:int?",
			"active:bool", "featured:bool?", "released_at:time", "retired_at:time?", "owner_id:uuid", "parent_id:uuid?", "--tenant-scoped",
		},
		{"category", "name:string", "description:text?", "--global"},
	}
}

func runGo(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, output)
	}
}

// TestGeneratedScaffoldBuildsWiresAndPasses generates a tenant-scoped and a
// global scaffold into an application for each database, pastes the printed
// wiring into bootstrap.go, and requires the application to vet, build, lint
// and pass its generated tests: the page tests always, and the persistence and
// bootstrap tests when a server is configured (SQLite needs none; MySQL is
// skipped here when no server is configured).
func TestGeneratedScaffoldBuildsWiresAndPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	for _, database := range []struct{ name, adapter, testPackage string }{
		{"postgresql", "postgres", "postgrestest"},
		{"mysql", "mysql", "mysqltest"},
		{"sqlite", "sqlite", "sqlitetest"},
	} {
		t.Run(database.name, func(t *testing.T) {
			t.Parallel()
			dir := matrixApplicationFor(t, database.name, "scaffold", scaffoldRequests())
			var specs []resourceSpec
			for _, args := range scaffoldRequests() {
				request, err := parseGeneratorArgs(args, scaffoldUsage)
				if err != nil {
					t.Fatal(err)
				}
				request.Scaffold = true
				specs = append(specs, resourceSpec{
					resourceRequest: request, Module: "example.com/mx", Database: database.adapter,
					Identity: database.adapter == "postgres", BrowserPipeline: true,
				})
			}
			wireResources(t, dir, specs)
			mfa := ""
			if database.adapter == "postgres" {
				mfa = "\tcfg.Auth.MFAKey = config.NewSecret(\"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\")\n"
			}
			writeFile(t, filepath.Join(dir, "app", "bootstrap", "scaffold_wired_test.go"), fmt.Sprintf(scaffoldWiredTest, database.adapter, database.testPackage, mfa))

			runGo(t, dir, "go", "vet", "./...")
			runGo(t, dir, "go", "build", "-o", os.DevNull, "./cmd/mx")
			packages := []string{"./app/domain/...", "./app/application/...", "./app/adapters/htmlui/...", "./app/adapters/httpapi/...", "./app/views/..."}
			if databaseAvailable(database.name) {
				packages = []string{"./app/..."}
			} else {
				t.Logf("no %s test server is configured; the persistence and bootstrap tests were skipped", database.name)
			}
			command := exec.Command("go", append([]string{"test", "-count=1", "-v"}, packages...)...)
			command.Dir = dir
			command.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("go test: %v\n%s", err, output)
			}
			for _, want := range []string{"--- PASS: TestProductPagesLifecycle", "--- PASS: TestCategoryPagesRequirePermissions", "--- PASS: TestProductPagesRequireTheCSRFToken"} {
				if !strings.Contains(string(output), want) {
					t.Fatalf("generated tests did not run: missing %q\n%s", want, output)
				}
			}
			if databaseAvailable(database.name) {
				// The scaffold's own TestRoutes skips by design in database applications.
				if !strings.Contains(string(output), "--- PASS: TestScaffoldPagesAreNotPublic") || strings.Count(string(output), "--- SKIP") != 1 || !strings.Contains(string(output), "--- SKIP: TestRoutes") {
					t.Fatalf("the wired application test did not run, or an unexpected test was skipped\n%s", output)
				}
			}
			lint, err := exec.LookPath("golangci-lint")
			if err != nil {
				t.Log("golangci-lint is not on PATH; the lint check was skipped")
				return
			}
			golangciLint.Lock()
			defer golangciLint.Unlock()
			runGo(t, dir, lint, "run", "./app/...")
		})
	}
}

// TestGeneratedScaffoldFieldMatrix generates a scaffold for every edge-case
// resource (each field type alone, required and optional, both scopes, SQL
// reserved words), then vets, lints and runs the page tests of all of them.
func TestGeneratedScaffoldFieldMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	dir := matrixApplicationFor(t, "sqlite", "scaffold", matrixResources())
	runGo(t, dir, "go", "vet", "./app/...")
	runGo(t, dir, "go", "test", "-count=1", "./app/adapters/htmlui/...", "./app/views/...")
	lint, err := exec.LookPath("golangci-lint")
	if err != nil {
		t.Log("golangci-lint is not on PATH; the lint check was skipped")
		return
	}
	golangciLint.Lock()
	defer golangciLint.Unlock()
	runGo(t, dir, lint, "run", "./app/domain/...", "./app/application/...", "./app/adapters/...", "./app/views/...")
}

// TestGeneratedViewJoinsTheScaffoldLayout checks that `generate view`,
// `generate scaffold` and `forge new` agree on one views package: a page made
// by `generate view` and the scaffold's pages load into the same renderer.
func TestGeneratedViewJoinsTheScaffoldLayout(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	dir := matrixApplicationFor(t, "sqlite", "scaffold", [][]string{{"post", "title:string", "--global"}})
	if _, err := generateView(dir, "welcome"); err != nil {
		t.Fatal(err)
	}
	if _, err := generateView(dir, "posts/archive"); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runGenerateEnv([]string{"scaffold", "comment", "text:text", "--global"}, &output, dir, nil, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"pages/welcome.gohtml", "posts/archive.gohtml", "posts/index.gohtml", "comments/index.gohtml"} {
		if _, err := os.Stat(filepath.Join(dir, "app", "views", filepath.FromSlash(path))); err != nil {
			t.Error(err)
		}
	}
	runGo(t, dir, "go", "vet", "./app/...")
	runGo(t, dir, "go", "test", "-count=1", "./app/views/...", "./app/adapters/htmlui/...")
}
