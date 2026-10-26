package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// testGeneratedApp vets and builds the application, runs go test verbosely on
// packages, requires every name in wantPass to have passed and no test to have
// been skipped except the ones named in allowedSkips, and lints app/ with the
// repository's golangci-lint configuration.
func testGeneratedApp(t *testing.T, dir string, packages, wantPass, allowedSkips []string) {
	t.Helper()
	runGo(t, dir, "go", "vet", "./...")
	runGo(t, dir, "go", "build", "-o", os.DevNull, "./cmd/mx")
	command := exec.Command("go", append([]string{"test", "-count=1", "-v"}, packages...)...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go test: %v\n%s", err, output)
	}
	for _, want := range wantPass {
		if !strings.Contains(string(output), "--- PASS: "+want+" ") && !strings.Contains(string(output), "--- PASS: "+want+"\n") {
			t.Fatalf("generated test did not run: missing %q\n%s", want, output)
		}
	}
	for _, line := range strings.Split(string(output), "\n") {
		if _, skipped, ok := strings.Cut(strings.TrimSpace(line), "--- SKIP: "); ok {
			name, _, _ := strings.Cut(skipped, " ")
			if !slices.Contains(allowedSkips, name) {
				t.Fatalf("a generated test was skipped: %s\n%s", name, output)
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
}

// TestGeneratedModelHasNoHTTPLayerAndPasses generates models (every field
// type, both scopes) into an application for each database and requires the
// result to vet, build, lint and pass its tests, with no HTTP adapter.
func TestGeneratedModelHasNoHTTPLayerAndPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	for _, database := range []string{"postgresql", "mysql", "sqlite"} {
		t.Run(database, func(t *testing.T) {
			t.Parallel()
			dir := matrixApplicationFor(t, database, "model", scaffoldRequests())
			for _, absent := range []string{"app/adapters/httpapi", "app/adapters/htmlui", "app/views/products", "app/views/categories"} {
				if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(absent))); err == nil {
					t.Errorf("%s exists: a model has no HTTP layer", absent)
				}
			}
			packages := []string{"./app/domain/...", "./app/application/..."}
			wantPass := []string{"TestProductServiceCreate", "TestCategoryServiceCreate", "TestProductNewKeepsEveryField"}
			if databaseAvailable(database) {
				packages = []string{"./app/domain/...", "./app/application/...", "./app/adapters/..."}
				wantPass = append(wantPass, "TestProductRepositoryCreateAndGet", "TestProductRepositoryIsolatesTenants", "TestCategoryRepositoryCreateAndGet")
				if database == "postgresql" {
					wantPass = append(wantPass, "TestProductRowLevelSecurity")
				}
			} else {
				t.Logf("no %s test server is configured; the persistence tests were skipped", database)
			}
			testGeneratedApp(t, dir, packages, wantPass, nil)
		})
	}
}

// TestGeneratedModelFieldMatrix vets and lints a model for every edge-case
// resource (each field type alone, both scopes, SQL reserved words).
func TestGeneratedModelFieldMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	dir := matrixApplicationFor(t, "sqlite", "model", matrixResources())
	runGo(t, dir, "go", "vet", "./app/...")
	runGo(t, dir, "go", "test", "-count=1", "./app/...")
	lint, err := exec.LookPath("golangci-lint")
	if err != nil {
		t.Log("golangci-lint is not on PATH; the lint check was skipped")
		return
	}
	golangciLint.Lock()
	defer golangciLint.Unlock()
	runGo(t, dir, lint, "run", "--allow-parallel-runners", "./app/...")
}

// controllerWiredTest is added to the generated application after the printed
// wiring is pasted into bootstrap.go. It proves what the controller promises
// about the real composition root: guarded routes refuse anonymous callers and
// public ones answer.
const controllerWiredTest = `package bootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
%s)

func TestControllersAreWired(t *testing.T) {
	cfg := config.Default()
%s	app, err := forge.New(forge.WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := Configure(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close(context.Background()) })

	get := func(path, accept string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Accept", accept)
		recorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(recorder, request)
		return recorder
	}
	for _, path := range []string{%s} {
		recorder := get(path, "text/html")
		redirected := recorder.Code == http.StatusSeeOther && recorder.Header().Get("Location") == "/session/new?return_to="+url.QueryEscape(path)
		if recorder.Code != http.StatusUnauthorized && recorder.Code != http.StatusForbidden && !redirected {
			t.Errorf("guarded GET %%s = %%d (Location %%q), want 401, 403 or the sign-in redirect: %%s", path, recorder.Code, recorder.Header().Get("Location"), recorder.Body)
		}
		if strings.Contains(recorder.Body.String(), "<h1>") {
			t.Errorf("guarded GET %%s served its page to an anonymous caller", path)
		}
	}
	for _, path := range []string{%s} {
		recorder := get(path, "text/html")
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "<h1>") {
			t.Errorf("public GET %%s = %%d: %%s", path, recorder.Code, recorder.Body)
		}
	}
	for _, path := range []string{%s} {
		recorder := get(path, "application/json")
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Header().Get("Content-Type"), "application/json") {
			t.Errorf("public GET %%s = %%d: %%s", path, recorder.Code, recorder.Body)
		}
	}
}
`

// wiredTestConfig is the imports and the configuration lines a wired test
// needs for database: a server-backed database needs its test configuration.
func wiredTestConfig(database string) (imports, config string) {
	switch database {
	case "postgresql":
		return "\t\"github.com/rocketinsidecorp/forge-go/postgres/postgrestest\"\n",
			"\tcfg.Database = postgrestest.Config(t)\n\tcfg.Auth.MFAKey = config.NewSecret(\"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\")\n"
	case "sqlite":
		return "\t\"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest\"\n", "\tcfg.Database = sqlitetest.Config(t)\n"
	default:
		return "", ""
	}
}

func quoted(paths ...string) string {
	var out []string
	for _, path := range paths {
		out = append(out, fmt.Sprintf("%q", path))
	}
	return strings.Join(out, ", ")
}

// controllerRequests are controllers the generator must turn into code that
// compiles, lints and passes: guarded and public, HTML and JSON, and actions
// named like Go keywords and like the identifiers the generated code uses.
func controllerRequests() [][]string {
	return [][]string{
		{"pages", "index", "home", "about_us"},
		{"reports", "monthly", "type", "range", "func", "routes", "err", "controller", "renderer", "protect", "new", "api_keys", "i_d", "tenant"},
		{"landing", "index", "--public"},
		{"status_checks", "index", "detail", "--api"},
		{"open_things", "ping", "--api", "--public"},
	}
}

// TestGeneratedControllersBuildWireAndPass generates every kind of controller
// into applications with Forge identity, without it and without a database,
// pastes the printed wiring into bootstrap.go and requires the application to
// vet, build, lint and pass its tests.
func TestGeneratedControllersBuildWireAndPass(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	for _, database := range []struct{ name, adapter string }{{"postgresql", "postgres"}, {"sqlite", "sqlite"}, {"none", "none"}} {
		t.Run(database.name, func(t *testing.T) {
			t.Parallel()
			dir := matrixApplicationFor(t, database.name, "controller", controllerRequests())
			identity := database.adapter == "postgres"
			var parts []resourceWiringParts
			for _, args := range controllerRequests() {
				request, err := parseControllerArgs(args)
				if err != nil {
					t.Fatal(err)
				}
				parts = append(parts, buildControllerWiring(controllerSpec{
					controllerRequest: request, Module: "example.com/mx", Identity: identity,
					ViewRenderer: identity && !request.API,
				}))
			}
			wireRoutes(t, dir, parts)
			imports, configuration := wiredTestConfig(database.name)
			writeFile(t, filepath.Join(dir, "app", "bootstrap", "controllers_wired_test.go"), fmt.Sprintf(controllerWiredTest, imports, configuration,
				quoted("/pages/home", "/pages/about_us", "/pages", "/reports/monthly", "/reports/type", "/v1/status_checks", "/v1/status_checks/detail"),
				quoted("/landing"), quoted("/v1/open_things/ping")))

			packages := []string{"./app/adapters/htmlui/...", "./app/adapters/httpapi/...", "./app/views/..."}
			wantPass := []string{
				"TestPagesControllerRendersEveryAction", "TestPagesControllerRequiresItsPermission", "TestReportsControllerRendersEveryAction",
				"TestLandingControllerRendersEveryAction", "TestStatusChecksControllerAnswersEveryAction", "TestStatusChecksControllerRequiresItsPermission",
				"TestOpenThingsControllerAnswersEveryAction", "TestControllersAreWired",
			}
			if database.adapter == "none" || databaseAvailable(database.name) {
				packages = []string{"./app/..."}
			} else {
				wantPass = wantPass[:len(wantPass)-1]
			}
			testGeneratedApp(t, dir, packages, wantPass, []string{"TestRoutes"})
		})
	}
}

// TestGeneratedControllerPermissionMigration checks the migration a guarded
// controller adds in an application with Forge identity, and that a public one
// adds none.
func TestGeneratedControllerPermissionMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	dir := matrixApplicationFor(t, "postgresql", "controller", [][]string{{"pages", "home"}, {"landing", "index", "--public"}})
	entries, err := os.ReadDir(filepath.Join(dir, "db", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		found = append(found, entry.Name())
	}
	joined := strings.Join(found, " ")
	if !strings.Contains(joined, "_create_pages_controller_permissions.up.sql") || !strings.Contains(joined, "_create_pages_controller_permissions.down.sql") {
		t.Errorf("a guarded controller must register its permission; migrations: %v", found)
	}
	if strings.Contains(joined, "landing") {
		t.Errorf("a public controller must not register a permission; migrations: %v", found)
	}
	up := readFile(t, filepath.Join(dir, "db", "migrations", found[slices.IndexFunc(found, func(name string) bool { return strings.HasSuffix(name, "_create_pages_controller_permissions.up.sql") })]))
	for _, want := range []string{"'pages:read'", "forge_permissions", "forge_role_permissions", "r.name = 'owner'"} {
		if !strings.Contains(up, want) {
			t.Errorf("the permission migration lacks %s:\n%s", want, up)
		}
	}
}

// mailerWiredTest is added to the generated application after the printed
// wiring is pasted into bootstrap.go: mail is configured to write files, the
// previews are on, and the application serves them and delivers an email.
const mailerWiredTest = `package bootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
%s)

func newMailerApp(t *testing.T, configure func(*config.Config)) *forge.App {
	t.Helper()
	cfg := config.Default()
%s	configure(&cfg)
	app, err := forge.New(forge.WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := Configure(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close(context.Background()) })
	return app
}

func TestMailerPreviewsAreServedInDevelopmentOnly(t *testing.T) {
	mails := t.TempDir()
	app := newMailerApp(t, func(cfg *config.Config) {
		cfg.Environment = config.Development
		cfg.Mail.URL = config.NewSecret("file:" + filepath.ToSlash(mails))
		cfg.Mail.From = "App <app@example.com>"
		cfg.Mail.Preview = true
		cfg.Auth.PasswordResetURL = "https://app.example.com/passwords/edit" // PostgreSQL applications send reset emails when mail is configured
	})
	for _, path := range []string{%s} {
		recorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("GET %%s = %%d: %%s", path, recorder.Code, recorder.Body)
		}
	}
	if _, err := os.ReadDir(mails); err != nil {
		t.Fatal(err)
	}

	off := newMailerApp(t, func(cfg *config.Config) {
		cfg.Environment = config.Development
		cfg.Mail.URL = config.NewSecret("file:" + filepath.ToSlash(mails))
		cfg.Mail.From = "App <app@example.com>"
		cfg.Auth.PasswordResetURL = "https://app.example.com/passwords/edit"
	})
	recorder := httptest.NewRecorder()
	off.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/_mail/user/", nil))
	if recorder.Code == http.StatusOK || strings.Contains(recorder.Body.String(), "Welcome") {
		t.Errorf("previews were served without FORGE_MAIL_PREVIEW: %%d %%s", recorder.Code, recorder.Body)
	}
}

func TestApplicationStartsWithoutMailConfigured(t *testing.T) {
	// Mail is optional: the application starts, and sending fails closed.
	newMailerApp(t, func(cfg *config.Config) { cfg.Environment = config.Development })
}
`

// TestGeneratedMailersBuildWireAndPass generates mailers, pastes the printed
// wiring into bootstrap.go and requires the application to vet, build, lint
// and pass its tests, including delivery through the framework's recorder and
// the previews served by the wired application.
func TestGeneratedMailersBuildWireAndPass(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	requests := [][]string{
		{"user", "welcome:name,reset_url", "password_reset", "type:err,ctx"},
		{"order_notice", "shipped:order_id"},
	}
	for _, database := range []struct{ name, adapter string }{{"postgresql", "postgres"}, {"sqlite", "sqlite"}, {"none", "none"}} {
		t.Run(database.name, func(t *testing.T) {
			t.Parallel()
			dir := matrixApplicationFor(t, database.name, "mailer", requests)
			var parts []resourceWiringParts
			for _, args := range requests {
				request, err := parseMailerArgs(args)
				if err != nil {
					t.Fatal(err)
				}
				parts = append(parts, buildMailerWiring(request, "example.com/mx"))
			}
			wireParts(t, dir, parts)
			imports, configuration := wiredTestConfig(database.name)
			writeFile(t, filepath.Join(dir, "app", "bootstrap", "mailers_wired_test.go"), fmt.Sprintf(mailerWiredTest, imports, configuration,
				quoted("/_mail/user/", "/_mail/user/welcome", "/_mail/user/password_reset", "/_mail/order_notice/shipped")))

			packages := []string{"./app/mailers/..."}
			wantPass := []string{
				"TestUserMailerWelcomeDelivers", "TestUserMailerWelcomeEscapesTheHTMLPart", "TestUserMailerPasswordResetDelivers",
				"TestOrderNoticeMailerShippedDelivers", "TestUserMailerPreviewsAreDevelopmentOnly", "TestNewUserMailerRefusesIncompleteSetups",
				"TestUserMailerWelcomeRefusesAHeaderInjectingRecipient", "TestUserMailerWelcomeDoesNotSendWhenDeliveryFails",
			}
			if database.adapter == "none" || databaseAvailable(database.name) {
				packages = append(packages, "./app/bootstrap/...")
				wantPass = append(wantPass, "TestMailerPreviewsAreServedInDevelopmentOnly", "TestApplicationStartsWithoutMailConfigured")
			}
			testGeneratedApp(t, dir, packages, wantPass, []string{"TestRoutes"})
		})
	}
}

// TestGeneratedQueueJobsBuildWireAndPass generates typed queue jobs (every
// payload type, no payload) and requires them to vet, lint and pass, and to
// register with a real queue registry.
func TestGeneratedQueueJobsBuildAndPass(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	requests := [][]string{
		{"send_welcome", "user_id:uuid", "note:string?", "--queue"},
		{"every_type", "a:string", "b:text", "c:int", "d:bool", "e:time", "f:uuid", "g:string?", "h:text?", "i:int?", "j:bool?", "k:time?", "l:uuid?", "id:uuid", "version:int", "--queue"},
		{"nudge", "--queue"},
		{"scheduled_cleanup"},
	}
	for _, database := range []string{"sqlite", "none"} {
		t.Run(database, func(t *testing.T) {
			t.Parallel()
			// The scheduled stub is `generate job` without --queue and shares the package.
			dir := matrixApplicationFor(t, database, "job", requests)
			testGeneratedApp(t, dir, []string{"./app/jobs/..."},
				[]string{"TestRegisterSendWelcomeAddsTheKind", "TestEnqueueSendWelcomeStoresATypedJob", "TestEnqueueEveryTypeStoresATypedJob", "TestPerformNudgeRequiresImplementation", "TestRunScheduledCleanupRequiresImplementation"}, nil)
		})
	}
}

// TestDestroyRestoresTheApplication generates every generator into a fresh
// application, destroys each with the same arguments, and requires the file
// tree to be what it was before, and the application to still compile.
func TestDestroyRestoresTheApplication(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	for _, database := range []string{"postgresql", "sqlite", "none"} {
		t.Run(database, func(t *testing.T) {
			t.Parallel()
			dir := matrixApplicationFor(t, database, "view", [][]string{{"welcome"}})
			before := fileTree(t, dir)
			var env []string
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "FORGE_DATABASE_URL=") {
					env = append(env, entry)
				}
			}
			cases := []struct {
				generator string
				args      []string
			}{
				{"controller", []string{"pages", "home", "about_us"}},
				{"controller", []string{"status_checks", "index", "--api", "--public"}},
				{"mailer", []string{"user", "welcome:name"}},
				{"job", []string{"send_welcome", "user_id:uuid", "--queue"}},
				{"job", []string{"cleanup"}},
				{"view", []string{"reports/summary"}},
			}
			if database != "none" {
				cases = append(cases, []struct {
					generator string
					args      []string
				}{
					{"model", []string{"post", "title:string", "body:text?", "--tenant-scoped"}},
					{"resource", []string{"tag", "name:string", "--global"}},
					{"scaffold", []string{"comment", "text:text", "--global"}},
				}...)
			}
			now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
			for _, c := range cases {
				var output bytes.Buffer
				if err := runGenerateEnv(append([]string{c.generator}, c.args...), &output, dir, env, now); err != nil {
					t.Fatalf("generate %s %v: %v", c.generator, c.args, err)
				}
				now = now.Add(time.Minute)
			}
			for _, c := range slices.Backward(cases) {
				var output bytes.Buffer
				// Migrations are kept unless asked: the database may have run them.
				if err := runDestroyEnv(append([]string{c.generator}, append(slices.Clone(c.args), "--migrations")...), &output, dir, env, now); err != nil {
					t.Fatalf("destroy %s %v: %v\n%s", c.generator, c.args, err, output.String())
				}
			}
			after := fileTree(t, dir)
			// The views base a scaffold, view or controller adds stays: other
			// pages share it.
			for path := range after {
				if _, existed := before[path]; !existed && !strings.HasPrefix(path, "app/views/") && !strings.HasPrefix(path, "app/assets/") && !strings.HasPrefix(path, "app/javascript/") {
					t.Errorf("%s was left behind by destroy", path)
				}
			}
			for path, content := range before {
				if after[path] != content {
					t.Errorf("%s changed or disappeared", path)
				}
			}
			runGo(t, dir, "go", "vet", "./...")
		})
	}
}

// fileTree maps every file under dir to its content.
func fileTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	tree := make(map[string]string)
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path) // #nosec G304 -- the test's own temporary directory
		tree[filepath.ToSlash(relative)] = string(content)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}
