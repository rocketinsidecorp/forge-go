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
		// With Forge identity a signed-out browser is sent to the sign-in page.
		redirected := recorder.Code == http.StatusSeeOther && recorder.Header().Get("Location") == "/session/new?return_to="+url.QueryEscape(path)
		if recorder.Code != http.StatusUnauthorized && recorder.Code != http.StatusForbidden && !redirected {
			t.Errorf("GET %%s = %%d (Location %%q), want 401, 403 or the sign-in redirect without credentials: %%s", path, recorder.Code, recorder.Header().Get("Location"), recorder.Body)
		}
		if strings.Contains(recorder.Body.String(), "<form") {
			t.Errorf("GET %%s served a form to an unauthenticated caller", path)
		}
		api := httptest.NewRequest(http.MethodGet, path, nil)
		api.Header.Set("Accept", "application/json")
		apiRecorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(apiRecorder, api)
		if apiRecorder.Code != http.StatusUnauthorized && apiRecorder.Code != http.StatusForbidden {
			t.Errorf("GET %%s as JSON = %%d, want 401 or 403", path, apiRecorder.Code)
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

// scaffoldSignInFlowTest is added to the generated PostgreSQL application
// with the printed wiring: the owner `setup` creates signs in through the
// generated pages, is sent back to the scaffold page it asked for, holds the
// scaffold's permissions through the owner role, and loses access on sign-out.
const scaffoldSignInFlowTest = `package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/csrf"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"

	appdb "example.com/mx/db"
)

func TestOwnerSignsInAndReachesTheScaffoldPages(t *testing.T) {
	ctx := context.Background()
	database, databaseConfig := postgrestest.NewWithConfig(t)
	loaded, err := migrate.LoadMigrationSets(authpostgres.Migrations(), appdb.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := postgres.NewMigrator(databaseConfig, loaded, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	_ = migrator.Close()
	repository, err := authpostgres.New(database)
	if err != nil {
		t.Fatal(err)
	}
	const password = "correct horse battery staple"
	if _, err := repository.ProvisionOwner(ctx, authpostgres.OwnerAccount{Organization: "Acme", TenantSlug: "acme", Email: "owner@example.com", Password: password}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Database = databaseConfig
	cfg.Auth.MFAKey = config.NewSecret("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	app, err := forge.New(forge.WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := Configure(ctx, app); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close(context.Background()) })

	cookies := map[string]*http.Cookie{}
	do := func(method, target string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		var request *http.Request
		if form != nil {
			form.Set(csrf.FormField, cookies[csrf.CookieName].Value)
			request = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			request = httptest.NewRequest(method, target, nil)
		}
		request.Header.Set("Accept", "text/html")
		for _, cookie := range cookies {
			request.AddCookie(cookie)
		}
		recorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(recorder, request)
		for _, cookie := range recorder.Result().Cookies() {
			if cookie.MaxAge < 0 {
				delete(cookies, cookie.Name)
			} else {
				cookies[cookie.Name] = cookie
			}
		}
		return recorder
	}

	redirect := do(http.MethodGet, "/products", nil)
	if redirect.Code != http.StatusSeeOther {
		t.Fatalf("signed-out GET /products = %d", redirect.Code)
	}
	signInPage := do(http.MethodGet, redirect.Header().Get("Location"), nil)
	if signInPage.Code != http.StatusOK || !strings.Contains(signInPage.Body.String(), ` + "`" + `name="return_to" value="/products"` + "`" + `) {
		t.Fatalf("sign-in page status=%d body=%s", signInPage.Code, signInPage.Body)
	}
	if failed := do(http.MethodPost, "/session", url.Values{"tenant": {"acme"}, "email": {"owner@example.com"}, "password": {"wrong password"}, "return_to": {"/products"}}); failed.Code != http.StatusUnprocessableEntity {
		t.Fatalf("wrong password status=%d", failed.Code)
	}
	signedIn := do(http.MethodPost, "/session", url.Values{"tenant": {"acme"}, "email": {"owner@example.com"}, "password": {password}, "return_to": {"/products"}})
	if signedIn.Code != http.StatusSeeOther || signedIn.Header().Get("Location") != "/products" {
		t.Fatalf("sign in status=%d Location=%q body=%s", signedIn.Code, signedIn.Header().Get("Location"), signedIn.Body)
	}
	for path, want := range map[string]int{
		"/products":        http.StatusOK,
		"/products/new":    http.StatusOK,
		"/categories":      http.StatusOK,
		"/categories/new":  http.StatusForbidden, // global writes stay behind protectPlatformWrite
		"/v1/auth/session": http.StatusNotFound,  // JSON authentication APIs are opt-in (--api-auth)
	} {
		if page := do(http.MethodGet, path, nil); page.Code != want {
			t.Errorf("signed-in GET %s = %d, want %d: %s", path, page.Code, want, page.Body)
		}
	}
	if page := do(http.MethodGet, "/products", nil); !strings.Contains(page.Body.String(), "Products") {
		t.Errorf("the products page did not render:\n%s", page.Body)
	}
	// Global rows are read by every tenant: the owner of one tenant can write
	// them neither through the JSON API (with the session cookie and the CSRF
	// token) nor through the form.
	write := httptest.NewRequest(http.MethodPost, "/v1/categories", strings.NewReader("{\"name\":\"Lamps\"}"))
	write.Header.Set("Content-Type", "application/json")
	write.Header.Set("X-CSRF-Token", cookies[csrf.CookieName].Value)
	for _, cookie := range cookies {
		write.AddCookie(cookie)
	}
	written := httptest.NewRecorder()
	app.Handler().ServeHTTP(written, write)
	if written.Code != http.StatusForbidden {
		t.Errorf("the owner's POST /v1/categories = %d, want 403: %s", written.Code, written.Body)
	}
	if form := do(http.MethodPost, "/categories", url.Values{"name": {"Lamps"}}); form.Code != http.StatusForbidden {
		t.Errorf("the owner's POST /categories = %d, want 403", form.Code)
	}
	if signedOut := do(http.MethodPost, "/session", url.Values{"_method": {"DELETE"}}); signedOut.Code != http.StatusSeeOther {
		t.Fatalf("sign out status=%d", signedOut.Code)
	}
	if again := do(http.MethodGet, "/products", nil); again.Code != http.StatusSeeOther {
		t.Fatalf("after sign-out GET /products = %d", again.Code)
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
					Identity: true, Organizations: true, BrowserPipeline: true, ViewRenderer: true,
				})
			}
			wireResources(t, dir, specs)
			// Every database application here has Forge identity, which needs
			// the MFA key.
			mfa := "\tcfg.Auth.MFAKey = config.NewSecret(\"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\")\n"
			writeFile(t, filepath.Join(dir, "app", "bootstrap", "scaffold_wired_test.go"), fmt.Sprintf(scaffoldWiredTest, database.adapter, database.testPackage, mfa))
			if database.adapter == "postgres" {
				writeFile(t, filepath.Join(dir, "app", "bootstrap", "scaffold_sign_in_test.go"), scaffoldSignInFlowTest)
			}

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
				if database.adapter == "postgres" {
					for _, want := range []string{"--- PASS: TestOwnerSignsInAndReachesTheScaffoldPages", "--- PASS: TestPagesAgainstTheIdentityStore", "--- PASS: TestFailedSignInIsUniform", "--- PASS: TestMFAPagesAgainstTheIdentityStore", "--- PASS: TestMFASettingsNeedASignedInAccount"} {
						if !strings.Contains(string(output), want) {
							t.Fatalf("the sign-in flow did not run: missing %q\n%s", want, output)
						}
					}
				}
			}
			lint, err := exec.LookPath("golangci-lint")
			if err != nil {
				t.Log("golangci-lint is not on PATH; the lint check was skipped")
				return
			}
			golangciLint.Lock()
			defer golangciLint.Unlock()
			runGo(t, dir, lint, "run", "--allow-parallel-runners", "./app/...")
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
	runGo(t, dir, lint, "run", "--allow-parallel-runners", "./app/domain/...", "./app/application/...", "./app/adapters/...", "./app/views/...")
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
