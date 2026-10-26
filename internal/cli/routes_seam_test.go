package cli

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

// seamApplication is `forge new NAME` (without resolving dependencies) for
// database ("postgresql", "sqlite", ... or "none"), plus extra flags.
func seamApplication(t *testing.T, database string, extra ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "shop")
	args := append([]string{"new", "shop", "--module", "example.com/shop", "--dir", dir, "--skip-deps"}, extra...)
	if database != "none" {
		args = append(args, "--database", database)
	} else {
		args = append(args, "--skip-database")
	}
	var stdout, stderr bytes.Buffer
	if code := Run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("forge new: code=%d %s %s", code, stdout.String(), stderr.String())
	}
	return dir
}

var seamNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func seamGenerate(t *testing.T, dir string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runGenerateEnv(args, &out, dir, withoutDatabaseURL(), seamNow); err != nil {
		t.Fatalf("forge generate %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func seamDestroy(t *testing.T, dir string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runDestroyEnv(args, &out, dir, withoutDatabaseURL(), seamNow); err != nil {
		t.Fatalf("forge destroy %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func seamBlocks(t *testing.T, dir string) []string {
	t.Helper()
	blocks, err := scaffold.ParseRoutes([]byte(readFile(t, filepath.Join(dir, scaffold.RoutesFile))))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, block := range blocks {
		ids = append(ids, block.ID())
	}
	return ids
}

func TestScaffoldWiresItselfIntoTheSeamAndDestroyUnwiresIt(t *testing.T) {
	t.Parallel()
	dir := seamApplication(t, "postgresql")
	empty := readFile(t, filepath.Join(dir, scaffold.RoutesFile))
	bootstrap := readFile(t, filepath.Join(dir, "app", "bootstrap", "bootstrap.go"))

	output := seamGenerate(t, dir, "scaffold", "product", "name:string", "price:int", "--tenant-scoped")
	if !strings.Contains(output, "wire  app/bootstrap/routes_gen.go") || strings.Contains(output, "Forge did not edit") {
		t.Fatalf("the scaffold printed wiring to paste instead of wiring itself:\n%s", output)
	}
	if got, want := strings.Join(seamBlocks(t, dir), ","), "renderer renderer,scaffold products,guard protect,guard protectCookie"; got != want {
		t.Fatalf("seam blocks = %s, want %s", got, want)
	}
	seam := readFile(t, filepath.Join(dir, scaffold.RoutesFile))
	for _, want := range []string{
		"httpapi.RegisterProducts(app, productService, protect(repository, middleware))",
		"htmlui.RegisterProducts(app, productService, renderer, protectCookie(repository, middleware))",
		"\nfunc protect(repository *authpostgres.Repository, middleware *auth.Middleware) func(string, http.Handler) (http.Handler, error) {",
		"auth.RequireCSRF(guarded)", "middleware.AuthenticateCookie(checked)", `middleware.AuthenticateBrowser("/session/new", guarded)`,
		"\"example.com/shop/app/adapters/htmlui\"",
	} {
		if !strings.Contains(seam, want) {
			t.Errorf("the seam lacks %q:\n%s", want, seam)
		}
	}
	if readFile(t, filepath.Join(dir, "app", "bootstrap", "bootstrap.go")) != bootstrap {
		t.Fatal("the generator edited bootstrap.go")
	}
	// The migration grants the new permissions to the owner role setup creates.
	migrations, err := filepath.Glob(filepath.Join(dir, "db", "migrations", "*_create_products.up.sql"))
	if err != nil || len(migrations) != 1 {
		t.Fatalf("migrations: %v %v", migrations, err)
	}
	if up := readFile(t, migrations[0]); !strings.Contains(up, "WHERE r.name = 'owner'") || !strings.Contains(up, "('products:read'), ('products:write')") {
		t.Fatalf("the migration does not grant the permissions to the owner role:\n%s", up)
	}

	// Running the same command again changes nothing.
	before := fileTree(t, dir)
	again := seamGenerate(t, dir, "scaffold", "product", "name:string", "price:int", "--tenant-scoped")
	if strings.Contains(again, "  create  ") || !strings.Contains(again, "identical  app/bootstrap/routes_gen.go") || !strings.Contains(again, "identical  app/domain/product.go") {
		t.Fatalf("a second run did not report identical files:\n%s", again)
	}
	if !maps.Equal(fileTree(t, dir), before) {
		t.Fatal("a second run changed the application")
	}
	// The same name with other arguments is still refused.
	var refused bytes.Buffer
	if err := runGenerateEnv([]string{"scaffold", "product", "name:string", "--tenant-scoped"}, &refused, dir, withoutDatabaseURL(), seamNow); err == nil || !strings.Contains(err.Error(), "already creates the table products") {
		t.Fatalf("other arguments: %v", err)
	}

	// A global resource adds the platform guard its writes need.
	seamGenerate(t, dir, "resource", "category", "name:string", "--global")
	if got, want := strings.Join(seamBlocks(t, dir), ","), "renderer renderer,scaffold products,resource categories,guard protect,guard protectPlatformWrite,guard protectCookie"; got != want {
		t.Fatalf("seam blocks = %s, want %s", got, want)
	}

	// Destroy takes each block back out, and the shared blocks with the last
	// user: the seam ends as forge new wrote it.
	output = seamDestroy(t, dir, "scaffold", "product", "name:string", "price:int", "--tenant-scoped")
	if !strings.Contains(output, "unwire") || strings.Contains(output, "Forge does not edit") {
		t.Fatalf("destroy did not unwire:\n%s", output)
	}
	if got, want := strings.Join(seamBlocks(t, dir), ","), "resource categories,guard protect,guard protectPlatformWrite"; got != want {
		t.Fatalf("after destroying the scaffold, seam blocks = %s, want %s", got, want)
	}
	seamDestroy(t, dir, "resource", "category", "name:string", "--global")
	if got := readFile(t, filepath.Join(dir, scaffold.RoutesFile)); got != empty {
		t.Fatalf("destroying everything did not restore the empty seam:\n%s", got)
	}
	// Destroying again finds nothing to unwire and nothing to remove.
	var nothing bytes.Buffer
	if err := runDestroyEnv([]string{"resource", "category", "name:string", "--global"}, &nothing, dir, withoutDatabaseURL(), seamNow); err == nil || !strings.Contains(err.Error(), "nothing to destroy but the migrations") {
		t.Fatalf("a second destroy: %v\n%s", err, nothing.String())
	}
}

func TestSeamRefusesAHandEditAndWritesNothing(t *testing.T) {
	t.Parallel()
	dir := seamApplication(t, "sqlite")
	seamGenerate(t, dir, "scaffold", "product", "name:string", "--global")
	path := filepath.Join(dir, scaffold.RoutesFile)
	edited := strings.Replace(readFile(t, path), "return nil\n}", "// mine\n\treturn nil\n}", 1)
	writeFile(t, path, edited)
	before := fileTree(t, dir)
	for _, args := range [][]string{
		{"resource", "category", "name:string", "--global"},
		{"controller", "pages", "home"},
	} {
		var out bytes.Buffer
		err := runGenerateEnv(args, &out, dir, withoutDatabaseURL(), seamNow)
		if !errors.Is(err, scaffold.ErrRoutesEdited) || !strings.Contains(err.Error(), "edited by hand") {
			t.Fatalf("forge generate %v over an edited seam: %v", args, err)
		}
		if !maps.Equal(fileTree(t, dir), before) {
			t.Fatalf("a refused forge generate %v changed the application", args)
		}
	}
	var out bytes.Buffer
	if err := runDestroyEnv([]string{"scaffold", "product", "name:string", "--global"}, &out, dir, withoutDatabaseURL(), seamNow); !errors.Is(err, scaffold.ErrRoutesEdited) {
		t.Fatalf("destroy over an edited seam: %v", err)
	}
	if !maps.Equal(fileTree(t, dir), before) {
		t.Fatal("a refused destroy changed the application")
	}
}

func TestSeamWithoutIdentityIsClosedUnlessPublic(t *testing.T) {
	t.Parallel()
	dir := seamApplication(t, "sqlite")
	closed := seamGenerate(t, dir, "scaffold", "product", "name:string", "--global")
	seam := readFile(t, filepath.Join(dir, scaffold.RoutesFile))
	if !strings.Contains(seam, `fault.New("permission_denied"`) || strings.Contains(seam, "return next, nil") || !strings.Contains(closed, "--public") {
		t.Fatalf("the routes of an application without identity are not closed, or --public is not explained:\n%s\n%s", closed, seam)
	}
	public := seamGenerate(t, dir, "scaffold", "category", "name:string", "--global", "--public")
	seam = readFile(t, filepath.Join(dir, scaffold.RoutesFile))
	if !strings.Contains(public, "WARNING: --public") || !strings.Contains(seam, "htmlui.RegisterCategories(app, categoryService, renderer, public)") ||
		!strings.Contains(seam, "httpapi.RegisterCategories(app, categoryService, public)") || !strings.Contains(seam, "htmlui.RegisterProducts(app, productService, renderer, protect)") {
		t.Fatalf("--public:\n%s\n%s", public, seam)
	}
	for _, args := range [][]string{
		{"scaffold", "post", "title:string", "--tenant-scoped", "--public"},
		{"resource", "post", "title:string", "--global", "--public", "--public"},
		{"model", "post", "title:string", "--global", "--public"},
	} {
		var out bytes.Buffer
		if err := runGenerateEnv(args, &out, dir, withoutDatabaseURL(), seamNow); err == nil || !strings.Contains(err.Error(), "--public") {
			t.Errorf("forge generate %v: %v", args, err)
		}
	}
	// An application without the seam cannot take --public: its printed
	// wiring would still be the deny-all guard.
	legacy := resourceProject(t, "sqlite:storage/development.db")
	var legacyOut bytes.Buffer
	if err := runGenerateEnv([]string{"resource", "post", "title:string", "--global", "--public"}, &legacyOut, legacy, nil, seamNow); err == nil || !strings.Contains(err.Error(), "--public needs the routes seam") {
		t.Errorf("--public without the seam: %v", err)
	}
	// A guard the application declares itself replaces the seam's.
	writeFile(t, filepath.Join(dir, "app", "bootstrap", "guards.go"), "package bootstrap\n\nimport \"net/http\"\n\nfunc protect(_ string, next http.Handler) (http.Handler, error) { return next, nil }\n")
	seamGenerate(t, dir, "controller", "pages", "home")
	if blocks := seamBlocks(t, dir); slices.Contains(blocks, "guard protect") || !slices.Contains(blocks, "controller pages") {
		t.Fatalf("the application's own protect did not replace the seam's: %s", blocks)
	}
}

func TestSeamInAnIdentityApplicationRefusesPublic(t *testing.T) {
	t.Parallel()
	dir := seamApplication(t, "postgresql")
	var out bytes.Buffer
	err := runGenerateEnv([]string{"scaffold", "product", "name:string", "--global", "--public"}, &out, dir, withoutDatabaseURL(), seamNow)
	if err == nil || !strings.Contains(err.Error(), "applications without Forge identity") {
		t.Fatalf("--public with Forge identity: %v", err)
	}
	// A controller wires itself and its permission; again is a no-op.
	first := seamGenerate(t, dir, "controller", "reports", "index")
	if !strings.Contains(first, "wire  app/bootstrap/routes_gen.go") || !strings.Contains(first, "_create_reports_controller_permissions.up.sql") {
		t.Fatalf("controller:\n%s", first)
	}
	before := fileTree(t, dir)
	if again := seamGenerate(t, dir, "controller", "reports", "index"); strings.Contains(again, "  create  ") || !maps.Equal(fileTree(t, dir), before) {
		t.Fatalf("a second controller run changed the application:\n%s", again)
	}
	seamDestroy(t, dir, "controller", "reports", "index")
	if blocks := seamBlocks(t, dir); len(blocks) != 0 {
		t.Fatalf("destroy controller left %v", blocks)
	}
}

func TestSeamDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	dir := seamApplication(t, "none")
	before := fileTree(t, dir)
	output := seamGenerate(t, dir, "controller", "pages", "home", "--public", "--dry-run")
	if !strings.Contains(output, "would wire  app/bootstrap/routes_gen.go") || !maps.Equal(fileTree(t, dir), before) {
		t.Fatalf("--dry-run:\n%s", output)
	}
	seamGenerate(t, dir, "controller", "pages", "home", "--public")
	if blocks := strings.Join(seamBlocks(t, dir), ","); blocks != "renderer renderer,controller pages" {
		t.Fatalf("a public controller in an application without a database: %s", blocks)
	}
	output = seamDestroy(t, dir, "controller", "pages", "home", "--public", "--dry-run")
	if !strings.Contains(output, "would unwire") {
		t.Fatalf("destroy --dry-run:\n%s", output)
	}
	if blocks := seamBlocks(t, dir); len(blocks) != 2 {
		t.Fatalf("destroy --dry-run changed the seam: %v", blocks)
	}
}

func TestSeamRefusesAnEntryWiredDifferently(t *testing.T) {
	t.Parallel()
	dir := seamApplication(t, "sqlite")
	projectRoot, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = projectRoot.Close() }()
	seam := openRoutesSeam(projectRoot, "example.com/shop", "sqlite", false)
	offered := map[string]string{"guard protect": seamDenyAllGuard}
	content, verb, err := planRoutes(projectRoot, seam, routesChange{entry: scaffold.RouteBlock{Kind: "controller", Name: "pages", Body: "\tif err := htmlui.RegisterPagesController(app, renderer, protect); err != nil {\n\t\treturn err\n\t}\n"}, shared: offered})
	if err != nil || verb != "wire" {
		t.Fatalf("plan: %q %v", verb, err)
	}
	writeFile(t, filepath.Join(dir, scaffold.RoutesFile), string(content))
	if _, _, err := planRoutes(projectRoot, seam, routesChange{entry: scaffold.RouteBlock{Kind: "controller", Name: "pages", Body: "\tif err := htmlui.RegisterPagesController(app, renderer); err != nil {\n\t\treturn err\n\t}\n"}, shared: offered}); err == nil || !strings.Contains(err.Error(), "already wires the controller pages") {
		t.Fatalf("a different entry: %v", err)
	}
	// A shared block the seam needs but nobody offers is an error, not a
	// silent omission.
	if err := os.Remove(filepath.Join(dir, scaffold.RoutesFile)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := planRoutes(projectRoot, seam, routesChange{entry: scaffold.RouteBlock{Kind: "controller", Name: "about", Body: "\tif err := htmlui.RegisterAboutController(app, renderer, protect); err != nil {\n\t\treturn err\n\t}\n"}}); err == nil || !strings.Contains(err.Error(), "needs protect") {
		t.Fatalf("a missing shared block: %v", err)
	}
}

// A block that builds on another (as nested routes or a join between two
// resources will) keeps that one from being removed first.
func TestSeamRefusesToRemoveABlockAnotherUses(t *testing.T) {
	t.Parallel()
	dir := seamApplication(t, "sqlite")
	seamGenerate(t, dir, "resource", "post", "title:string", "--global", "--public")
	projectRoot, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = projectRoot.Close() }()
	seam := openRoutesSeam(projectRoot, "example.com/shop", "sqlite", false)
	nested := scaffold.RouteBlock{Kind: "nested", Name: "post_comments", Body: "\tif err := httpapi.RegisterPostComments(app, postService, public); err != nil {\n\t\treturn err\n\t}\n"}
	content, verb, err := planRoutes(projectRoot, seam, routesChange{entry: nested})
	if err != nil || verb != "wire" {
		t.Fatalf("an extra block for an existing resource: %q %v", verb, err)
	}
	writeFile(t, filepath.Join(dir, scaffold.RoutesFile), string(content))
	if _, verb, err := planRoutes(projectRoot, seam, routesChange{entry: nested}); err != nil || verb != "identical" {
		t.Fatalf("the same extra block again: %q %v", verb, err)
	}
	var out bytes.Buffer
	if err := runDestroyEnv([]string{"resource", "post", "title:string", "--global", "--public"}, &out, dir, withoutDatabaseURL(), seamNow); err == nil || !strings.Contains(err.Error(), "nested post_comments") {
		t.Fatalf("destroying the resource a nested block uses: %v", err)
	}
	if _, verb, err := planRoutes(projectRoot, seam, routesChange{entry: nested, remove: true}); err != nil || verb != "unwire" {
		t.Fatalf("removing the nested block: %q %v", verb, err)
	}
}

// `forge g` and `forge d` are `forge generate` and `forge destroy`, as in Rails.
func TestGenerateAndDestroyAliases(t *testing.T) {
	dir := seamApplication(t, "sqlite")
	t.Chdir(dir) // Run works in the current directory, so not in parallel
	t.Setenv("FORGE_DATABASE_URL", "sqlite:storage/development.db")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"g", "scaffold", "product", "name:string", "--global", "--public"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "wire  app/bootstrap/routes_gen.go") {
		t.Fatalf("forge g scaffold: code=%d %s %s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := Run([]string{"d", "scaffold", "product", "name:string", "--global", "--public"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "unwire") {
		t.Fatalf("forge d scaffold: code=%d %s %s", code, stdout.String(), stderr.String())
	}
	if blocks := seamBlocks(t, dir); len(blocks) != 0 {
		t.Fatalf("forge d left %v", blocks)
	}
	for alias, canonical := range map[string]string{"g": "generate", "d": "destroy"} {
		var help bytes.Buffer
		if code := Run([]string{alias, "--help"}, &help, &stderr); code != 0 || !strings.Contains(help.String(), "usage: forge "+canonical) || !strings.Contains(help.String(), "alias: forge "+alias) {
			t.Errorf("forge %s --help: code=%d\n%s", alias, code, help.String())
		}
		help.Reset()
		if err := runHelp([]string{alias}, &help); err != nil || !strings.Contains(help.String(), "usage: forge "+canonical) {
			t.Errorf("forge help %s: %v\n%s", alias, err, help.String())
		}
	}
	var usageText bytes.Buffer
	usage(&usageText)
	if !strings.Contains(usageText.String(), "g is short for generate") || !strings.Contains(usageText.String(), "d is short for destroy") {
		t.Errorf("forge help does not list the aliases:\n%s", usageText.String())
	}
}

// The seam is never read or written through a symlink, a directory in its
// place is an error, and files of package bootstrap that do not parse are
// skipped when looking for the application's own guards.
func TestSeamRefusesSymlinksAndOddFiles(t *testing.T) {
	t.Parallel()
	dir := seamApplication(t, "sqlite")
	path := filepath.Join(dir, scaffold.RoutesFile)
	writeFile(t, filepath.Join(dir, "app", "bootstrap", "broken.go"), "package bootstrap\n\nfunc (\n")
	if err := os.Mkdir(filepath.Join(dir, "app", "bootstrap", "dir.go"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("routes_gen.go.real", path); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runGenerateEnv([]string{"controller", "pages", "home", "--public"}, &out, dir, withoutDatabaseURL(), seamNow); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlinked seam: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := runGenerateEnv([]string{"controller", "pages", "home", "--public"}, &out, dir, withoutDatabaseURL(), seamNow); err == nil {
		t.Fatal("a directory in place of the seam was accepted")
	}
	if err := writeRoutes(filepath.Join(dir, "missing"), nil); err == nil {
		t.Fatal("writeRoutes outside an application succeeded")
	}
}

// publicOverride is the guard an application owner declares, as the seam's
// documentation says, to put authentication in front of --public routes.
const publicOverride = `package bootstrap

import "net/http"

// public is the application's own guard: here it refuses everyone.
func public(_ string, _ http.Handler) (http.Handler, error) {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "sign in first", http.StatusUnauthorized)
	}), nil
}
`

const publicOverrideTest = `package bootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
)

func TestTheApplicationsPublicGuardIsUsed(t *testing.T) {
	cfg := config.Default()
	cfg.Database = sqlitetest.Config(t)
	app, err := forge.New(forge.WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := Configure(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close(context.Background()) })
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/v1/products", nil),
		httptest.NewRequest(http.MethodPost, "/v1/products", strings.NewReader(` + "`" + `{"name":"x"}` + "`" + `)),
		httptest.NewRequest(http.MethodGet, "/products", nil),
	} {
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want the application's guard (401)", request.Method, request.URL.Path, recorder.Code)
		}
	}
}
`

// TestAGuardOverrideIsNeverSilentlyIgnored is the regression test of a
// fail-open: the seam declared its guards inside generatedRoutes, so an
// application's own func public (as documented) compiled and was shadowed,
// leaving the routes public. The seam's guards are package-level functions
// now: the override breaks the build ("redeclared") until a generator runs
// again, which then leaves the seam's out and mounts the routes behind the
// application's guard.
func TestAGuardOverrideIsNeverSilentlyIgnored(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on a generated project")
	}
	t.Parallel()
	scaffoldArgs := []string{"product", "name:string", "--global", "--public"}
	dir := matrixApplicationFor(t, "sqlite", "scaffold", [][]string{scaffoldArgs})
	writeFile(t, filepath.Join(dir, "app", "bootstrap", "auth.go"), publicOverride)
	vet := exec.Command("go", "vet", "./app/bootstrap")
	vet.Dir = dir
	vet.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
	output, err := vet.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "public redeclared") {
		t.Fatalf("the application's own public compiled next to the seam's, so one of them is silently ignored: %v\n%s", err, output)
	}
	// Any generator run (here the same command again) leaves the seam's
	// guard out and uses the application's.
	var out bytes.Buffer
	if err := runGenerateEnv(append([]string{"scaffold"}, scaffoldArgs...), &out, dir, withoutDatabaseURL(), seamNow); err != nil || !strings.Contains(out.String(), "wire  app/bootstrap/routes_gen.go") {
		t.Fatalf("generating again: %v\n%s", err, out.String())
	}
	if seam := readFile(t, filepath.Join(dir, scaffold.RoutesFile)); strings.Contains(seam, "func public(") {
		t.Fatalf("the seam still declares public:\n%s", seam)
	}
	writeFile(t, filepath.Join(dir, "app", "bootstrap", "public_override_test.go"), publicOverrideTest)
	runGo(t, dir, "go", "test", "-count=1", "-run", "TestTheApplicationsPublicGuardIsUsed", "./app/bootstrap")
}

// Generators running at the same time in one application never lose a
// block: each either wires its routes, or is refused by the generate lock.
func TestConcurrentGeneratorsNeverLoseABlock(t *testing.T) {
	t.Parallel()
	dir := seamApplication(t, "sqlite")
	names := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}
	errs := make([]error, len(names))
	done := make(chan int)
	for i, name := range names {
		go func() {
			var out bytes.Buffer
			errs[i] = runGenerateEnv([]string{"resource", name, "title:string", "--global", "--public"}, &out, dir, withoutDatabaseURL(), seamNow.Add(time.Duration(i)*time.Minute))
			done <- i
		}()
	}
	for range names {
		<-done
	}
	blocks := strings.Join(seamBlocks(t, dir), ",")
	wired := 0
	for i, name := range names {
		switch {
		case errs[i] == nil:
			wired++
			if !strings.Contains(blocks, "resource "+pluralize(name)) {
				t.Errorf("%s reported success but its block is missing: %s", name, blocks)
			}
		case !errors.Is(errs[i], errGenerateLocked):
			t.Errorf("%s: %v", name, errs[i])
		}
	}
	if wired == 0 {
		t.Fatal("no generator ran")
	}
	if _, err := os.Stat(filepath.Join(dir, ".forge", "generate.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the generate lock was left behind: %v", err)
	}
	// A lock left in place refuses, and writes nothing.
	writeFile(t, filepath.Join(dir, ".forge", "generate.lock"), "")
	before := fileTree(t, dir)
	var out bytes.Buffer
	if err := runGenerateEnv([]string{"resource", "golf", "title:string", "--global", "--public"}, &out, dir, withoutDatabaseURL(), seamNow); !errors.Is(err, errGenerateLocked) || !strings.Contains(err.Error(), ".forge/generate.lock") {
		t.Fatalf("a held lock: %v", err)
	}
	if err := runDestroyEnv([]string{"resource", "alpha", "title:string", "--global"}, &out, dir, withoutDatabaseURL(), seamNow); !errors.Is(err, errGenerateLocked) {
		t.Fatalf("destroy under a held lock: %v", err)
	}
	if !maps.Equal(fileTree(t, dir), before) {
		t.Fatal("a refused generator changed the application")
	}
}

// The --public warning says why the routes are open: no Forge identity, or,
// in an API-only PostgreSQL application, no sign-in at all.
func TestPublicWarningFitsTheApplication(t *testing.T) {
	t.Parallel()
	for database, want := range map[string]string{
		"sqlite":     "whose sign-in pages and `forge setup` are PostgreSQL-only",
		"postgresql": "this API-only application (forge new --api) signs nobody in",
	} {
		dir := seamApplication(t, database, "--api")
		output := seamGenerate(t, dir, "resource", "post", "title:string", "--global", "--public")
		if !strings.Contains(output, "WARNING: --public") || !strings.Contains(output, want) {
			t.Errorf("%s:\n%s", database, output)
		}
	}
}

// Seam names cannot name a resource, plural or controller.
func TestSeamNamesAreReserved(t *testing.T) {
	t.Parallel()
	dir := seamApplication(t, "sqlite")
	before := fileTree(t, dir)
	for _, args := range [][]string{
		{"resource", "protect", "name:string", "--global"},
		{"scaffold", "thing", "name:string", "--global", "--plural", "protect"},
		{"scaffold", "thing", "name:string", "--global", "--plural", "protect_platform_write"},
		{"resource", "renderer", "name:string", "--global"},
		{"controller", "public", "index"},
		{"controller", "middleware", "index"},
		{"model", "repository", "name:string", "--global"},
	} {
		var out bytes.Buffer
		if err := runGenerateEnv(args, &out, dir, withoutDatabaseURL(), seamNow); err == nil || !strings.Contains(err.Error(), "is reserved: app/bootstrap/routes_gen.go uses the names") {
			t.Errorf("forge generate %v: %v", args, err)
		}
	}
	if !maps.Equal(fileTree(t, dir), before) {
		t.Fatal("a refused generator changed the application")
	}
}

// The guard references are built into the wiring, never substituted into
// its text: a global resource whose plural reads like a guard keeps its
// writes behind protectPlatformWrite. (The plural is reserved now; the
// wiring must be right regardless.)
func TestGuardReferencesNeverRewriteTheWiringText(t *testing.T) {
	t.Parallel()
	for _, plural := range []string{"protect", "protect_cookie", "things"} {
		spec := resourceSpec{
			resourceRequest: resourceRequest{Name: "thing", Plural: plural, Scaffold: true},
			Module:          "example.com/shop", Database: "postgres", Identity: true, Seam: true, BrowserPipeline: true, I18nMiddleware: true,
		}
		usage := buildResourceWiring(spec).Usage
		if strings.Count(usage, fmt.Sprintf("if permission == %q {", plural+":write")) != 2 || strings.Count(usage, "return protectPlatformWrite(permission, next)") != 2 ||
			!strings.Contains(usage, "return protect(repository, middleware)(permission, next)") || !strings.Contains(usage, "return protectCookie(repository, middleware)(permission, next)") {
			t.Errorf("plural %s:\n%s", plural, usage)
		}
	}
}

// The lock directory must be a plain directory, an interrupted seam write is
// refused before any file is written, and the seam keeps its permissions.
func TestSeamLockAndWriteSafety(t *testing.T) {
	t.Parallel()
	dir := seamApplication(t, "sqlite")
	seam := filepath.Join(dir, scaffold.RoutesFile)
	if err := os.Chmod(seam, 0o644); err != nil {
		t.Fatal(err)
	}
	seamGenerate(t, dir, "controller", "pages", "home", "--public")
	if info, err := os.Stat(seam); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("the seam's mode changed: %v %v", info.Mode(), err)
	}
	writeFile(t, seam+".tmp", "partial")
	before := fileTree(t, dir)
	var out bytes.Buffer
	if err := runGenerateEnv([]string{"scaffold", "post", "title:string", "--global", "--public"}, &out, dir, withoutDatabaseURL(), seamNow); err == nil || !strings.Contains(err.Error(), "routes_gen.go.tmp exists") {
		t.Fatalf("a stale temporary seam: %v", err)
	}
	if !maps.Equal(fileTree(t, dir), before) {
		t.Fatal("a refused generator wrote files")
	}
	if err := os.Remove(seam + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, ".forge")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, ".forge")); err != nil {
		t.Fatal(err)
	}
	if err := runGenerateEnv([]string{"scaffold", "post", "title:string", "--global", "--public"}, &out, dir, withoutDatabaseURL(), seamNow); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("a symlinked .forge: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, ".forge")); err != nil {
		t.Fatal(err)
	}

	// A go.mod that is not a regular file refuses the run instead of
	// running it without the lock.
	goMod := filepath.Join(dir, "go.mod")
	moved := filepath.Join(t.TempDir(), "go.mod")
	if err := os.Rename(goMod, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, goMod); err != nil {
		t.Fatal(err)
	}
	before = fileTree(t, dir)
	if err := runGenerateEnv([]string{"scaffold", "post", "title:string", "--global", "--public"}, &out, dir, withoutDatabaseURL(), seamNow); err == nil || !strings.Contains(err.Error(), "go.mod is not a regular file") {
		t.Fatalf("a symlinked go.mod: %v", err)
	}
	if !maps.Equal(fileTree(t, dir), before) {
		t.Fatal("a generator refused for a symlinked go.mod wrote files")
	}
}
