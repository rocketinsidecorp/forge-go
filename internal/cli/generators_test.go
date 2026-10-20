package cli

import (
	"bytes"
	"context"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

// generatorNow is also the version of a framework migration
// (auth/postgres's login_throttle_pair_scope), so migrations generated at it
// land one second later, at 20261020120001.
var generatorNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// generate runs `forge generate GENERATOR ARGS...` in root.
func generate(root, generator string, args ...string) (string, error) {
	var out bytes.Buffer
	err := runGenerateEnv(append([]string{generator}, args...), &out, root, nil, generatorNow)
	return out.String(), err
}

// mustGenerate is generate for a command that must succeed.
func mustGenerate(t *testing.T, root, generator string, args ...string) string {
	t.Helper()
	output, err := generate(root, generator, args...)
	if err != nil {
		t.Fatalf("generate %s %v: %v", generator, args, err)
	}
	return output
}

// generatedGoParses requires every .go file under root to parse.
func generatedGoParses(t *testing.T, root string) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		if _, err := parser.ParseFile(fset, path, nil, parser.AllErrors); err != nil {
			t.Errorf("%s does not parse: %v", path, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func exists(root, path string) bool {
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
	return err == nil
}

func TestParseModelArgs(t *testing.T) {
	t.Parallel()
	request, err := parseModelArgs([]string{"post", "title:string", "body:text?", "--tenant-scoped"})
	if err != nil {
		t.Fatal(err)
	}
	if !request.Model || request.Name != "post" || request.Plural != "posts" || !request.TenantScoped || len(request.Fields) != 2 {
		t.Fatalf("request = %+v", request)
	}
	for name, args := range map[string][]string{
		"no scope":           {"post", "title:string"},
		"no field":           {"post", "--global"},
		"broadcast":          {"post", "title:string", "--global", "--broadcast"},
		"uppercase name":     {"Post", "title:string", "--global"},
		"path in name":       {"../post", "title:string", "--global"},
		"slash in name":      {"a/b", "title:string", "--global"},
		"sql in name":        {"post;drop", "title:string", "--global"},
		"reserved column":    {"post", "id:uuid", "--global"},
		"bad type":           {"post", "price:float", "--global"},
		"quote in field":     {`post`, `ti"tle:string`, "--global"},
		"duplicate field":    {"post", "title:string", "title:text", "--global"},
		"plural name":        {"posts", "title:string", "--global"},
		"unknown flag":       {"post", "title:string", "--global", "--force"},
		"go keyword":         {"func", "title:string", "--global"},
		"file name suffix":   {"post_test", "title:string", "--global"},
		"template injection": {"post", "{{.x}}:string", "--global"},
	} {
		if _, err := parseModelArgs(args); err == nil {
			t.Errorf("%s: %v was accepted", name, args)
		}
	}
}

func TestGenerateModelHasNoHTTPLayer(t *testing.T) {
	t.Parallel()
	for _, database := range resourceDatabases {
		t.Run(database.name, func(t *testing.T) {
			t.Parallel()
			root := resourceProject(t, database.url)
			output := mustGenerate(t, root, "model", "post", "title:string", "body:text?", "--tenant-scoped")
			for _, want := range []string{
				"create  db/migrations/20261020120001_create_posts.up.sql",
				"create  " + filepath.Join("app", "domain", "post.go"),
				"create  " + filepath.Join("app", "domain", "post_test.go"),
				"create  " + filepath.Join("app", "application", "post.go"),
				"create  " + filepath.Join("app", "adapters", "persistence", "post.go"),
				"create  " + filepath.Join("app", "adapters", "persistence", "post_test.go"),
				"application.NewPostService(persistence.NewPostRepository(database), time.Now, uuid.New)",
				"tenancy.WithContext",
			} {
				if !strings.Contains(output, want) {
					t.Errorf("output lacks %q:\n%s", want, output)
				}
			}
			for _, absent := range []string{"httpapi", "htmlui", "protect", "Register"} {
				if strings.Contains(output, absent) {
					t.Errorf("a model prints no HTTP wiring, but the output mentions %q:\n%s", absent, output)
				}
			}
			for _, path := range []string{"app/adapters/httpapi", "app/adapters/htmlui", "app/views"} {
				if exists(root, path) {
					t.Errorf("%s exists after generating a model", path)
				}
			}
			generatedGoParses(t, root)
			// The persistence layer is the one the resource generates.
			resourceRoot := resourceProject(t, database.url)
			mustGenerate(t, resourceRoot, "resource", "post", "title:string", "body:text?", "--tenant-scoped")
			for _, path := range []string{"app/domain/post.go", "app/application/post.go", "app/adapters/persistence/post.go", "app/adapters/persistence/post_test.go", "db/migrations/20261020120001_create_posts.up.sql"} {
				if readFile(t, filepath.Join(root, filepath.FromSlash(path))) != readFile(t, filepath.Join(resourceRoot, filepath.FromSlash(path))) {
					t.Errorf("%s differs from what `generate resource` writes", path)
				}
			}
		})
	}
}

func TestGenerateModelDryRunWritesNothingAndRefusesToOverwrite(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	before := snapshotTree(t, root)
	output := mustGenerate(t, root, "model", "post", "title:string", "--global", "--dry-run")
	if !strings.Contains(output, "would create  "+filepath.Join("app", "domain", "post.go")) || strings.Contains(output, "  create  ") {
		t.Errorf("dry-run output:\n%s", output)
	}
	if after := snapshotTree(t, root); !slices.Equal(before, after) {
		t.Fatalf("--dry-run changed the project:\n%v\n%v", before, after)
	}
	mustGenerate(t, root, "model", "post", "title:string", "--global")
	if _, err := generate(root, "model", "post", "title:string", "--global"); err == nil {
		t.Error("generating the same model twice overwrote it")
	}
	// A resource and a model of one name collide too, instead of mixing layers.
	if _, err := generate(root, "resource", "post", "title:string", "--global"); err == nil {
		t.Error("a resource was generated over a model")
	}
}

func TestParseControllerArgs(t *testing.T) {
	t.Parallel()
	request, err := parseControllerArgs([]string{"--api", "pages", "index", "about_us", "--public", "--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	if request.Name != "pages" || !slices.Equal(request.Actions, []string{"index", "about_us"}) || !request.API || !request.Public || !request.DryRun {
		t.Fatalf("request = %+v", request)
	}
	for name, args := range map[string][]string{
		"nothing":           {},
		"no action":         {"pages"},
		"uppercase":         {"Pages", "home"},
		"traversal":         {"../pages", "home"},
		"path":              {"a/b", "home"},
		"dot":               {"pages.go", "home"},
		"leading digit":     {"9pages", "home"},
		"injection":         {"pages;rm -rf", "home"},
		"template":          {"{{.}}", "home"},
		"too long":          {strings.Repeat("a", 41), "home"},
		"controller suffix": {"pages_controller", "home"},
		"reserved path":     {"assets", "home"},
		"reserved v1":       {"v1", "home"},
		"layouts":           {"layouts", "home"},
		"session":           {"session", "home"},
		"write action":      {"pages", "create"},
		"update action":     {"pages", "update"},
		"destroy action":    {"pages", "destroy"},
		"action traversal":  {"pages", "../home"},
		"action slash":      {"pages", "a/b"},
		"action uppercase":  {"pages", "Home"},
		"action dash":       {"pages", "about-us"},
		"action space":      {"pages", "about us"},
		"action empty":      {"pages", ""},
		"duplicate":         {"pages", "home", "home"},
		"same Go method":    {"pages", "id", "i_d"},
		"unknown flag":      {"pages", "home", "--force"},
		"flag with value":   {"pages", "home", "--api=true"},
		"repeated flag":     {"pages", "home", "--api", "--api"},
		"too many actions":  append([]string{"pages"}, manyActions(21)...),
		"action too long":   {"pages", strings.Repeat("a", 41)},
		"action quote":      {"pages", `a"b`},
		"action leading _":  {"pages", "_home"},
		"action double _":   {"pages", "a__b"},
		"action trailing _": {"pages", "a_"},
	} {
		if _, err := parseControllerArgs(args); err == nil {
			t.Errorf("%s: %q was accepted", name, args)
		}
	}
	// Names that are Go keywords are fine as actions: they become exported methods.
	if _, err := parseControllerArgs([]string{"pages", "type", "range", "func", "new", "err"}); err != nil {
		t.Errorf("keyword-like actions were refused: %v", err)
	}
}

func manyActions(n int) []string {
	var actions []string
	for i := range n {
		actions = append(actions, "a"+strings.Repeat("b", i+1))
	}
	return actions
}

func TestControllerRejectedNamesWriteNothing(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	before := snapshotTree(t, root)
	for _, args := range [][]string{{"../escape", "home"}, {"pages", "../../escape"}, {"/abs", "home"}, {"pages", "a/b"}} {
		if _, err := generate(root, "controller", args...); err == nil {
			t.Errorf("controller %v was accepted", args)
		}
	}
	if after := snapshotTree(t, root); !slices.Equal(before, after) {
		t.Fatalf("rejected names changed the project:\n%v\n%v", before, after)
	}
}

func TestGenerateControllerHTML(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	output := mustGenerate(t, root, "controller", "pages", "index", "about_us")
	for _, want := range []string{
		"create  " + filepath.Join("app", "adapters", "htmlui", "pages_controller.go"),
		"create  " + filepath.Join("app", "adapters", "htmlui", "pages_controller_test.go"),
		"create  " + filepath.Join("app", "views", "pages", "index.gohtml"),
		"create  " + filepath.Join("app", "views", "pages", "about_us.gohtml"),
		"create  " + filepath.Join("app", "views", "views.go"), // the views base the application lacked
		"htmlui.RegisterPagesController(app, renderer, protect)",
		"GET /pages, GET /pages/about_us",
		"deny-all guard",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
	source := readFile(t, filepath.Join(root, "app", "adapters", "htmlui", "pages_controller.go"))
	for _, want := range []string{
		`routes.Handle("GET /pages", `, `routes.Handle("GET /pages/about_us", `, `const pagesControllerReadPermission = "pages:read"`,
		"func (c pagesController) AboutUs(", "a protect function is required",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("the controller lacks %q:\n%s", want, source)
		}
	}
	if exists(root, "app/adapters/persistence") || exists(root, "app/domain") || exists(root, "app/adapters/httpapi") {
		t.Error("a controller has no persistence and no JSON layer")
	}
	if got := readFile(t, filepath.Join(root, "app", "views", "pages", "about_us.gohtml")); got != generatedView {
		t.Errorf("the action view is not the generated page view:\n%s", got)
	}
	generatedGoParses(t, root)
	// db/migrations holds no permission migration: the application has no Forge identity.
	if entries, _ := os.ReadDir(filepath.Join(root, "db", "migrations")); len(entries) != 1 {
		t.Errorf("unexpected migrations: %v", entries)
	}
}

func TestGenerateControllerAPIAndPublic(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	output := mustGenerate(t, root, "controller", "status_checks", "index", "detail", "--api", "--public")
	for _, want := range []string{
		"create  " + filepath.Join("app", "adapters", "httpapi", "status_checks_controller.go"),
		"create  " + filepath.Join("app", "adapters", "httpapi", "status_checks_controller_test.go"),
		"httpapi.RegisterStatusChecksController(app)",
		"WARNING: --public",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
	for _, absent := range []string{"protect", "renderer", "views"} {
		if strings.Contains(output, absent) {
			t.Errorf("a public JSON controller needs no %q:\n%s", absent, output)
		}
	}
	if exists(root, "app/views") {
		t.Error("a JSON controller has no views and needs no views base")
	}
	source := readFile(t, filepath.Join(root, "app", "adapters", "httpapi", "status_checks_controller.go"))
	for _, want := range []string{`routes.Handle("GET /v1/status_checks", `, `routes.Handle("GET /v1/status_checks/detail", `} {
		if !strings.Contains(source, want) {
			t.Errorf("the controller lacks %q:\n%s", want, source)
		}
	}
	if strings.Contains(source, "protect") {
		t.Errorf("a public controller has no guard:\n%s", source)
	}
	generatedGoParses(t, root)
}

func TestGenerateControllerInIdentityApplicationsRegistersItsPermission(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "postgres://u:p@localhost/db") // cmd/shop imports auth/postgres
	output := mustGenerate(t, root, "controller", "reports", "monthly")
	for _, want := range []string{
		"create  db/migrations/20261020120001_create_reports_controller_permissions.up.sql",
		"create  db/migrations/20261020120001_create_reports_controller_permissions.down.sql",
		"protectCookie :=", "htmlui.RegisterReportsController(app, renderer, protectCookie)", "reports:read",
		"Then: forge migrate && forge test",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
	up := readFile(t, filepath.Join(root, "db", "migrations", "20261020120001_create_reports_controller_permissions.up.sql"))
	down := readFile(t, filepath.Join(root, "db", "migrations", "20261020120001_create_reports_controller_permissions.down.sql"))
	if !strings.Contains(up, "'reports:read'") || strings.Contains(up, "reports:write") || !strings.Contains(down, "DELETE FROM forge_permissions WHERE name = 'reports:read'") {
		t.Errorf("permission migration:\n%s\n%s", up, down)
	}
	api := mustGenerate(t, root, "controller", "feeds", "latest", "--api")
	if !strings.Contains(api, "protect :=") || !strings.Contains(api, "httpapi.RegisterFeedsController(app, protect)") {
		t.Errorf("a JSON controller authenticates the bearer token:\n%s", api)
	}
	// A public controller registers nothing.
	public := mustGenerate(t, root, "controller", "landing", "index", "--public")
	if strings.Contains(public, "permissions") {
		t.Errorf("a public controller registers no permission:\n%s", public)
	}
}

// migrationsMatching lists the migration files in root whose names contain
// part.
func migrationsMatching(t *testing.T, root, part string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "db", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.Contains(entry.Name(), part) {
			names = append(names, entry.Name())
		}
	}
	return names
}

// A permission has one owner migration, whose rollback removes it: a
// controller whose NAME is the plural of a model, resource or scaffold reuses
// that resource's NAME:read instead of registering it a second time (rolling
// the controller's migration back would otherwise revoke the resource's
// permission from every role).
func TestGenerateControllerReusesAPermissionAMigrationRegisters(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		generator string
		args      []string
		migration string
	}{
		{"scaffold", []string{"widget", "title:string", "--global"}, "create_widgets"},
		{"model", []string{"widget", "title:string", "--tenant-scoped"}, "create_widgets"},
		{"migration", []string{"add_widget_permissions"}, "add_widget_permissions"},
	} {
		t.Run(c.generator, func(t *testing.T) {
			t.Parallel()
			root := resourceProject(t, "postgres://u:p@localhost/db")
			mustGenerate(t, root, c.generator, c.args...)
			owner := firstMatch(t, root, "*_"+c.migration+".up.sql")
			if c.generator == "migration" {
				// A hand-written migration that registers the permission owns it too.
				writeFile(t, owner, "INSERT INTO forge_permissions (name) VALUES ('widgets:read');\n")
			}
			for _, args := range [][]string{{"widgets", "dashboard"}, {"widgets", "stats", "--api"}} {
				output := mustGenerate(t, root, "controller", args...)
				ownerPath := "db/migrations/" + filepath.Base(owner)
				for _, want := range []string{"reuse the permission widgets:read, which " + ownerPath + " already registers", "Then: forge test"} {
					if !strings.Contains(output, want) {
						t.Errorf("output lacks %q:\n%s", want, output)
					}
				}
				if strings.Contains(output, "controller_permissions") || strings.Contains(output, "forge migrate") {
					t.Errorf("a controller reusing a permission generates no migration:\n%s", output)
				}
			}
			if found := migrationsMatching(t, root, "controller_permissions"); len(found) != 0 {
				t.Errorf("a controller registered a permission another migration owns: %v", found)
			}
			// Destroying the controller leaves the owner's migration alone.
			for _, args := range [][]string{{"widgets", "dashboard"}, {"widgets", "stats", "--api"}} {
				if output, err := destroy(root, append([]string{"controller"}, append(args, "--migrations")...)...); err != nil {
					t.Fatalf("destroy controller %v: %v\n%s", args, err, output)
				}
			}
			if !exists(root, "db/migrations/"+filepath.Base(owner)) || exists(root, "app/adapters/htmlui/widgets_controller.go") {
				t.Error("destroying the controller removed the wrong files")
			}
		})
	}
}

// The other direction: a model, resource or scaffold never registers a
// permission a controller's migration (or any other) already registers.
func TestGenerateResourceRefusesAPermissionAnotherMigrationRegisters(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "postgres://u:p@localhost/db")
	mustGenerate(t, root, "controller", "widgets", "dashboard")
	controllerMigration := "db/migrations/20261020120001_create_widgets_controller_permissions.up.sql"
	if !exists(root, controllerMigration) {
		t.Fatalf("the guarded controller registered no permission: %v", migrationsMatching(t, root, ""))
	}
	before := snapshotTree(t, root)
	for _, generator := range []string{"model", "resource", "scaffold"} {
		_, err := generate(root, generator, "widget", "title:string", "--global")
		if err == nil {
			t.Fatalf("%s widget was generated over the widgets controller's permission", generator)
		}
		for _, want := range []string{controllerMigration, "widgets:read", "the " + generator + "'s migration", "--plural", "forge rollback"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s refusal lacks %q: %v", generator, want, err)
			}
		}
		if after := snapshotTree(t, root); !slices.Equal(before, after) {
			t.Fatalf("a refused %s changed the project:\n%v\n%v", generator, before, after)
		}
	}
	// Another plural has its own permissions.
	mustGenerate(t, root, "resource", "widget", "title:string", "--global", "--plural", "widget_items")
	// A hand-written migration that names either permission owns it too.
	writeFile(t, filepath.Join(root, "db", "migrations", "20260101000000_seed_gadgets.up.sql"), "INSERT INTO forge_permissions (name) VALUES ('gadgets:write');\n")
	if _, err := generate(root, "resource", "gadget", "name:string", "--global"); err == nil || !strings.Contains(err.Error(), "gadgets:write") {
		t.Errorf("a resource registered a permission a hand-written migration owns: %v", err)
	}
	// Without Forge identity nothing registers permissions, so nothing collides.
	plain := resourceProject(t, "sqlite:storage/development.db")
	mustGenerate(t, plain, "controller", "widgets", "dashboard")
	mustGenerate(t, plain, "scaffold", "widget", "title:string", "--global")
}

func TestGenerateControllerRefusesToOverwriteAndNeedsAnApplication(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	mustGenerate(t, root, "controller", "pages", "home")
	before := snapshotTree(t, root)
	if _, err := generate(root, "controller", "pages", "home", "about"); err == nil {
		t.Error("a controller was generated over an existing one")
	}
	if after := snapshotTree(t, root); !slices.Equal(before, after) {
		t.Fatalf("a refused generation changed the project:\n%v\n%v", before, after)
	}
	if _, err := generate(t.TempDir(), "controller", "pages", "home"); err == nil {
		t.Error("a controller was generated outside an application")
	}
	// Dry runs write nothing.
	fresh := resourceProject(t, "sqlite:storage/development.db")
	snapshot := snapshotTree(t, fresh)
	mustGenerate(t, fresh, "controller", "pages", "home", "--dry-run")
	if after := snapshotTree(t, fresh); !slices.Equal(snapshot, after) {
		t.Fatalf("--dry-run changed the project:\n%v\n%v", snapshot, after)
	}
}

func TestGenerateControllerRefusesSymlinks(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "app", "adapters"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "app", "adapters", "htmlui")); err != nil {
		t.Skip("symlinks are not available:", err)
	}
	if _, err := generate(root, "controller", "pages", "home"); err == nil {
		t.Fatal("a controller was written through a symlink")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("files escaped the application: %v", entries)
	}
}

func TestParseMailerArgs(t *testing.T) {
	t.Parallel()
	request, err := parseMailerArgs([]string{"user", "welcome:name,reset_url", "goodbye", "--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	if request.Name != "user" || len(request.Methods) != 2 || !slices.Equal(request.Methods[0].Fields, []string{"name", "reset_url"}) || len(request.Methods[1].Fields) != 0 || !request.DryRun {
		t.Fatalf("request = %+v", request)
	}
	for name, args := range map[string][]string{
		"nothing":           {},
		"no method":         {"user"},
		"uppercase":         {"User", "welcome"},
		"traversal":         {"../user", "welcome"},
		"slash":             {"a/b", "welcome"},
		"injection":         {`user"; drop`, "welcome"},
		"template":          {"{{.}}", "welcome"},
		"mailer suffix":     {"user_mailer", "welcome"},
		"too long":          {strings.Repeat("a", 41), "welcome"},
		"method uppercase":  {"user", "Welcome"},
		"method traversal":  {"user", "../welcome"},
		"method dash":       {"user", "wel-come"},
		"method empty":      {"user", ""},
		"method only colon": {"user", ":name"},
		"duplicate method":  {"user", "welcome", "welcome"},
		"build collision":   {"user", "welcome", "build_welcome"},
		"empty field":       {"user", "welcome:"},
		"empty field entry": {"user", "welcome:name,"},
		"field uppercase":   {"user", "welcome:Name"},
		"field with type":   {"user", "welcome:name:string"},
		"field template":    {"user", "welcome:{{.}}"},
		"field quote":       {"user", `welcome:na"me`},
		"field traversal":   {"user", "welcome:../name"},
		"duplicate field":   {"user", "welcome:name,name"},
		"same Go field":     {"user", "welcome:id,i_d"},
		"unknown flag":      {"user", "welcome", "--force"},
		"too many methods":  append([]string{"user"}, manyActions(21)...),
		"too many fields":   {"user", "welcome:" + strings.Join(manyActions(21), ",")},
		"method too long":   {"user", strings.Repeat("a", 41)},
	} {
		if _, err := parseMailerArgs(args); err == nil {
			t.Errorf("%s: %q was accepted", name, args)
		}
	}
}

func TestGenerateMailer(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	output := mustGenerate(t, root, "mailer", "user", "welcome:name,reset_url", "goodbye")
	for _, want := range []string{
		"create  " + filepath.Join("app", "mailers", "user_mailer.go"),
		"create  " + filepath.Join("app", "mailers", "user_mailer_preview.go"),
		"create  " + filepath.Join("app", "mailers", "user_mailer_test.go"),
		"create  " + filepath.Join("app", "mailers", "templates", "user", "welcome.subject.tmpl"),
		"create  " + filepath.Join("app", "mailers", "templates", "user", "welcome.text.tmpl"),
		"create  " + filepath.Join("app", "mailers", "templates", "user", "welcome.html.gohtml"),
		"create  " + filepath.Join("app", "mailers", "templates", "user", "goodbye.html.gohtml"),
		"mailers.NewUserMailer(mailSender, mailFrom)", "mail.ErrNotConfigured", "FORGE_MAIL_PREVIEW=true",
		`mail.NewPreviewHandler(app.Config(), "/_mail/user"`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
	source := readFile(t, filepath.Join(root, "app", "mailers", "user_mailer.go"))
	for _, want := range []string{
		"//go:embed templates/user", "type UserMailer struct", "type UserWelcomeData struct", "ResetURL string",
		"type UserGoodbyeData struct{}", "func (m *UserMailer) Welcome(ctx context.Context, to mail.Address, data UserWelcomeData) error",
		"func (m *UserMailer) BuildGoodbye(",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("the mailer lacks %q:\n%s", want, source)
		}
	}
	// The HTML part is html/template (contextual escaping): the file is
	// .gohtml and prints every field with an action, never raw markup.
	html := readFile(t, filepath.Join(root, "app", "mailers", "templates", "user", "welcome.html.gohtml"))
	if !strings.Contains(html, "{{.Name}}") || !strings.Contains(html, "{{.ResetURL}}") || strings.Contains(html, "safeHTML") {
		t.Errorf("welcome.html.gohtml:\n%s", html)
	}
	text := readFile(t, filepath.Join(root, "app", "mailers", "templates", "user", "welcome.text.tmpl"))
	if !strings.Contains(text, "{{.Name}}") || !strings.Contains(text, "{{.ResetURL}}") {
		t.Errorf("welcome.text.tmpl:\n%s", text)
	}
	if subject := readFile(t, filepath.Join(root, "app", "mailers", "templates", "user", "welcome.subject.tmpl")); strings.TrimSpace(subject) != "Welcome" {
		t.Errorf("subject = %q", subject)
	}
	preview := readFile(t, filepath.Join(root, "app", "mailers", "user_mailer_preview.go"))
	if !strings.Contains(preview, `Name:        "welcome"`) || !strings.Contains(preview, `ResetURL: "Sample reset url"`) {
		t.Errorf("the preview does not use fixture data:\n%s", preview)
	}
	generatedGoParses(t, root)
	// A second mailer shares the package and the templates directory.
	mustGenerate(t, root, "mailer", "order_notice", "shipped")
	generatedGoParses(t, root)
	if _, err := generate(root, "mailer", "user", "welcome"); err == nil {
		t.Error("a mailer was generated over an existing one")
	}
}

func TestGenerateMailerDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	before := snapshotTree(t, root)
	output := mustGenerate(t, root, "mailer", "user", "welcome", "--dry-run")
	if !strings.Contains(output, "would create  "+filepath.Join("app", "mailers", "user_mailer.go")) || strings.Contains(output, "  create  ") {
		t.Errorf("dry-run output:\n%s", output)
	}
	if after := snapshotTree(t, root); !slices.Equal(before, after) {
		t.Fatalf("--dry-run changed the project:\n%v\n%v", before, after)
	}
}

func TestParseQueueJobArgs(t *testing.T) {
	t.Parallel()
	request, err := parseQueueJobArgs([]string{"send_welcome", "user_id:uuid", "note:string?", "--queue", "--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	if request.Name != "send_welcome" || len(request.Fields) != 2 || !request.Fields[1].Optional || !request.DryRun {
		t.Fatalf("request = %+v", request)
	}
	if _, err := parseQueueJobArgs([]string{"x", "id:uuid", "version:int", "--queue"}); err != nil {
		t.Errorf("a payload may carry id and version: %v", err)
	}
	for name, args := range map[string][]string{
		"no queue flag":   {"send_welcome"},
		"no name":         {"--queue"},
		"uppercase":       {"SendWelcome", "--queue"},
		"traversal":       {"../x", "--queue"},
		"slash":           {"a/b", "--queue"},
		"injection":       {`x"; drop`, "--queue"},
		"file suffix":     {"x_linux", "--queue"},
		"test suffix":     {"x_test", "--queue"},
		"keyword":         {"func", "--queue"},
		"too long":        {strings.Repeat("a", 41), "--queue"},
		"bad type":        {"x", "a:float", "--queue"},
		"no type":         {"x", "a", "--queue"},
		"field uppercase": {"x", "A:string", "--queue"},
		"field traversal": {"x", "../a:string", "--queue"},
		"field template":  {"x", "{{.}}:string", "--queue"},
		"duplicate field": {"x", "a:string", "a:int", "--queue"},
		"same Go field":   {"x", "id:uuid", "i_d:uuid", "--queue"},
		"unknown flag":    {"x", "--queue", "--force"},
		"too many fields": append(append([]string{"x"}, fieldsOf(21)...), "--queue"),
	} {
		if _, err := parseQueueJobArgs(args); err == nil {
			t.Errorf("%s: %q was accepted", name, args)
		}
	}
}

func fieldsOf(n int) []string {
	var fields []string
	for _, name := range manyActions(n) {
		fields = append(fields, name+":int")
	}
	return fields
}

func TestGenerateQueueJob(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	output := mustGenerate(t, root, "job", "send_welcome", "user_id:uuid", "note:string?", "--queue")
	for _, want := range []string{
		"create  " + filepath.Join("app", "jobs", "send_welcome.go"), "create  " + filepath.Join("app", "jobs", "send_welcome_test.go"),
		"jobs.RegisterSendWelcome(registry)", "jobs.EnqueueSendWelcome(ctx, store.In(tx), registry", "docs/JOBS.md",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
	source := readFile(t, filepath.Join(root, "app", "jobs", "send_welcome.go"))
	for _, want := range []string{
		`const SendWelcomeKind = "send_welcome"`, "UserID uuid.UUID `json:\"user_id\"`", "Note   *string   `json:\"note\"`",
		"queue.Register(registry, SendWelcomeKind, PerformSendWelcome", "queue.Enqueue(ctx, in, registry, SendWelcomeKind, payload, options...)",
		"queue.Permanent(ErrSendWelcomeNotImplemented)", "queue.HandlerTimeout(time.Minute)",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("the job lacks %q:\n%s", want, source)
		}
	}
	generatedGoParses(t, root)
	if _, err := generate(root, "job", "send_welcome", "--queue"); err == nil {
		t.Error("a job was generated over an existing one")
	}
	// The scheduled stub of the same name collides instead of mixing the two.
	if _, err := generate(root, "job", "send_welcome"); err == nil {
		t.Error("a scheduled job was generated over a queue job")
	}
	// And without --queue the existing generator is unchanged.
	if out := mustGenerate(t, root, "job", "cleanup"); !strings.Contains(out, "bootstrap.JobsConfigure") {
		t.Errorf("the scheduled job generator changed:\n%s", out)
	}
}

func TestGenerateQueueJobDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	before := snapshotTree(t, root)
	output := mustGenerate(t, root, "job", "nudge", "--queue", "--dry-run")
	if !strings.Contains(output, "would create  "+filepath.Join("app", "jobs", "nudge.go")) || strings.Contains(output, "  create  ") {
		t.Errorf("dry-run output:\n%s", output)
	}
	if after := snapshotTree(t, root); !slices.Equal(before, after) {
		t.Fatalf("--dry-run changed the project:\n%v\n%v", before, after)
	}
}

// destroyCases are generators with their arguments, for the destroy tests.
var destroyCases = []struct {
	generator string
	args      []string
}{
	{"model", []string{"post", "title:string", "body:text?", "--tenant-scoped"}},
	{"resource", []string{"tag", "name:string", "--global"}},
	{"scaffold", []string{"comment", "text:text", "--global"}},
	{"controller", []string{"pages", "home", "about_us"}},
	{"controller", []string{"status_checks", "index", "--api", "--public"}},
	{"mailer", []string{"user", "welcome:name"}},
	{"job", []string{"send_welcome", "user_id:uuid", "--queue"}},
	{"job", []string{"cleanup"}},
	{"view", []string{"reports/summary"}},
}

func destroy(root string, args ...string) (string, error) {
	var out bytes.Buffer
	err := runDestroyEnv(args, &out, root, nil, generatorNow)
	return out.String(), err
}

func TestDestroyRemovesWhatTheGeneratorCreated(t *testing.T) {
	t.Parallel()
	for _, c := range destroyCases {
		t.Run(c.generator+" "+c.args[0], func(t *testing.T) {
			t.Parallel()
			root := resourceProject(t, "sqlite:storage/development.db")
			// An existing views base and an unrelated file must survive.
			mustGenerate(t, root, "view", "welcome")
			writeFile(t, filepath.Join(root, "app", "domain", "unrelated.go"), "package domain\n")
			before := fileTree(t, root)
			mustGenerate(t, root, c.generator, c.args...)
			if reflectEqual(before, fileTree(t, root)) {
				t.Fatal("the generator created nothing")
			}
			output, err := destroy(root, append([]string{c.generator}, append(slices.Clone(c.args), "--migrations")...)...)
			if err != nil {
				t.Fatalf("destroy: %v\n%s", err, output)
			}
			if !strings.Contains(output, "remove  ") || !strings.Contains(output, "Forge does not edit app/bootstrap/bootstrap.go") {
				t.Errorf("destroy output:\n%s", output)
			}
			after := fileTree(t, root)
			for path, content := range before {
				if after[path] != content {
					t.Errorf("%s changed or disappeared", path)
				}
			}
			for path := range after {
				if _, existed := before[path]; !existed {
					t.Errorf("%s was left behind", path)
				}
			}
			// Directories the generator made are gone too, the application's own stay.
			if c.generator == "mailer" && exists(root, "app/mailers") {
				t.Error("app/mailers is left empty")
			}
			if !exists(root, "app/views") || !exists(root, "db/migrations") || !exists(root, "app/domain") {
				t.Error("the application's own directories were removed")
			}
		})
	}
}

func reflectEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func TestDestroyLeavesTheSharedBroadcastAndViewsBase(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	mustGenerate(t, root, "scaffold", "note", "text:text", "--tenant-scoped", "--broadcast")
	mustGenerate(t, root, "scaffold", "memo", "text:text", "--global", "--broadcast")
	if _, err := destroy(root, "scaffold", "memo", "text:text", "--global", "--broadcast", "--migrations"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"app/adapters/htmlstreams/streams.go", "app/views/views.go", "app/views/layouts/application.gohtml", "app/adapters/htmlui/note.go", "app/views/notes/_item.gohtml"} {
		if !exists(root, path) {
			t.Errorf("%s was removed: it is shared or belongs to another scaffold", path)
		}
	}
	if exists(root, "app/adapters/htmlui/memo.go") || exists(root, "app/views/memos") {
		t.Error("the destroyed scaffold's files are still there")
	}
}

func TestDestroyKeepsMigrationsUnlessAsked(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	mustGenerate(t, root, "model", "post", "title:string", "--global")
	migration := "db/migrations/20261020120001_create_posts.up.sql"
	output, err := destroy(root, "model", "post", "title:string", "--global")
	if err != nil {
		t.Fatal(err)
	}
	if !exists(root, migration) || !exists(root, "db/migrations/20261020120001_create_posts.down.sql") {
		t.Error("a migration was removed without --migrations")
	}
	if !strings.Contains(output, "keep") || !strings.Contains(output, "forge rollback") || exists(root, "app/domain/post.go") {
		t.Errorf("destroy output:\n%s", output)
	}
	// The generator refuses to run again while the migration is there.
	if _, err := generate(root, "model", "post", "title:string", "--global"); err == nil {
		t.Error("the model was generated again over its kept migration")
	}
	// A second destroy points at what is left, the kept migrations.
	if _, err := destroy(root, "model", "post", "title:string", "--global"); err == nil || !strings.Contains(err.Error(), "--migrations") || !strings.Contains(err.Error(), migration) || strings.Contains(err.Error(), "check the arguments") {
		t.Errorf("a second destroy must point to --migrations: %v", err)
	}
	if _, err := destroy(root, "model", "post", "title:string", "--global", "--migrations"); err == nil {
		// Nothing but the migrations remain; they go now.
		if exists(root, migration) {
			t.Error("--migrations did not remove the migration")
		}
	} else {
		t.Fatalf("destroy --migrations: %v", err)
	}
}

// A model inside a resource or scaffold, and a resource inside a scaffold,
// belong to that bigger generation: destroying them alone would leave
// adapters that no longer compile.
func TestDestroyRefusesAModelABiggerGeneratorStillNeeds(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		generator, destroyed, httpLayer string
	}{
		{"resource", "model", "app/adapters/httpapi/post.go"},
		{"scaffold", "model", "app/adapters/htmlui/post.go"},
		{"scaffold", "resource", "app/adapters/htmlui/post.go"},
	} {
		t.Run(c.destroyed+" in "+c.generator, func(t *testing.T) {
			t.Parallel()
			root := resourceProject(t, "sqlite:storage/development.db")
			mustGenerate(t, root, c.generator, "post", "title:string", "--global")
			before := fileTree(t, root)
			for _, extra := range [][]string{nil, {"--dry-run"}} {
				_, err := destroy(root, append([]string{c.destroyed, "post", "title:string", "--global"}, extra...)...)
				if err == nil {
					t.Fatalf("destroy %s %v removed part of a %s", c.destroyed, extra, c.generator)
				}
				for _, want := range []string{c.httpLayer, "forge destroy " + c.generator, "--force"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal lacks %q: %v", want, err)
					}
				}
			}
			if after := fileTree(t, root); !reflectEqual(before, after) {
				t.Fatal("a refused destroy removed files")
			}
			if output, err := destroy(root, c.destroyed, "post", "title:string", "--global", "--force"); err != nil {
				t.Fatalf("destroy --force: %v\n%s", err, output)
			}
			if exists(root, "app/domain/post.go") || !exists(root, c.httpLayer) {
				t.Error("--force must remove only the destroyed generator's files")
			}
		})
	}
	// Destroying the bigger generator itself is not refused.
	root := resourceProject(t, "sqlite:storage/development.db")
	mustGenerate(t, root, "scaffold", "post", "title:string", "--global")
	if output, err := destroy(root, "scaffold", "post", "title:string", "--global"); err != nil {
		t.Fatalf("destroy scaffold: %v\n%s", err, output)
	}
}

// A controller's pages are the stub `generate view` writes, byte for byte:
// they belong to `destroy controller`, not `destroy view`.
func TestDestroyViewRefusesAPageAControllerRenders(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	mustGenerate(t, root, "controller", "pages", "home", "about_us")
	mustGenerate(t, root, "view", "pages/extra") // the controller does not render it
	before := fileTree(t, root)
	_, err := destroy(root, "view", "pages/home")
	if err == nil {
		t.Fatal("destroy view removed a page the pages controller renders")
	}
	for _, want := range []string{"app/views/pages/home.gohtml", "app/adapters/htmlui/pages_controller.go", "forge destroy controller pages", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q: %v", want, err)
		}
	}
	if after := fileTree(t, root); !reflectEqual(before, after) {
		t.Fatal("a refused destroy removed files")
	}
	if output, err := destroy(root, "view", "pages/extra"); err != nil || exists(root, "app/views/pages/extra.gohtml") {
		t.Fatalf("destroy view of a page no controller renders: %v\n%s", err, output)
	}
	if output, err := destroy(root, "view", "pages/about_us", "--force"); err != nil || exists(root, "app/views/pages/about_us.gohtml") {
		t.Fatalf("destroy view --force: %v\n%s", err, output)
	}
	// The controller's own destroy is not refused, and skips the page that is gone.
	if output, err := destroy(root, "controller", "pages", "home", "about_us"); err != nil || exists(root, "app/adapters/htmlui/pages_controller.go") {
		t.Fatalf("destroy controller: %v\n%s", err, output)
	}
}

func TestDestroyRefusesModifiedFilesUnlessForced(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	mustGenerate(t, root, "controller", "pages", "home", "about_us")
	path := filepath.Join(root, "app", "views", "pages", "home.gohtml")
	writeFile(t, path, readFile(t, path)+"<p>mine</p>\n")
	before := fileTree(t, root)
	output, err := destroy(root, "controller", "pages", "home", "about_us")
	if err == nil || !strings.Contains(err.Error(), "app/views/pages/home.gohtml") || !strings.Contains(err.Error(), "--force") ||
		!strings.Contains(err.Error(), "differ from what this generator would write") || strings.Contains(err.Error(), "changed since") {
		t.Fatalf("destroy of a modified file: err=%v\n%s", err, output)
	}
	if after := fileTree(t, root); !reflectEqual(before, after) {
		t.Fatal("a refused destroy removed files")
	}
	// A different set of arguments describes different files: what was not
	// generated is not removed, and a modified file is still refused.
	if _, err := destroy(root, "controller", "pages", "home"); err == nil {
		t.Error("destroy removed a modified file")
	}
	output, err = destroy(root, "controller", "pages", "home", "about_us", "--force")
	if err != nil || !strings.Contains(output, "differs from what the generator writes; removed because of --force") {
		t.Fatalf("destroy --force: %v\n%s", err, output)
	}
	if exists(root, "app/views/pages/home.gohtml") || exists(root, "app/adapters/htmlui/pages_controller.go") {
		t.Error("--force left files behind")
	}
}

func TestDestroyDryRunRemovesNothing(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	mustGenerate(t, root, "mailer", "user", "welcome")
	before := fileTree(t, root)
	output, err := destroy(root, "mailer", "user", "welcome", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "would remove") || strings.Contains(output, "  remove  ") {
		t.Errorf("dry-run output:\n%s", output)
	}
	if after := fileTree(t, root); !reflectEqual(before, after) {
		t.Fatal("--dry-run removed files")
	}
}

func TestDestroyRefusesWhatItCannotDoSafely(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	for name, args := range map[string][]string{
		"nothing":          {},
		"unknown":          {"widget", "x"},
		"migration":        {"migration", "create_x"},
		"nothing exists":   {"controller", "pages", "home"},
		"bad name":         {"controller", "../x", "home"},
		"missing scope":    {"scaffold", "post", "title:string"},
		"queue job flag":   {"job", "x", "--queue", "--bogus"},
		"view bad name":    {"view", "../x"},
		"view arguments":   {"view"},
		"job bad name":     {"job", "Bad"},
		"job no arguments": {"job"},
	} {
		if _, err := destroy(root, args...); err == nil {
			t.Errorf("%s: destroy %v succeeded", name, args)
		}
	}
	// Outside an application nothing is read or removed.
	if _, err := destroy(t.TempDir(), "controller", "pages", "home"); err == nil {
		t.Error("destroy ran outside an application")
	}
}

func TestDestroyNeverFollowsSymlinks(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	mustGenerate(t, root, "mailer", "user", "welcome")
	target := filepath.Join(root, "app", "mailers", "user_mailer.go")
	outside := filepath.Join(t.TempDir(), "keep.go")
	writeFile(t, outside, readFile(t, target))
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, target); err != nil {
		t.Skip("symlinks are not available:", err)
	}
	if _, err := destroy(root, "mailer", "user", "welcome"); err == nil {
		t.Error("destroy removed through a symlink")
	}
	if !exists(filepath.Dir(outside), "keep.go") {
		t.Error("the symlink target was removed")
	}
}

func TestGeneratedHelpListsEveryGenerator(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	usage(&out)
	for _, want := range []string{"generate model", "generate controller", "generate mailer", "generate job NAME [field:type...] --queue", "destroy "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage lacks %q", want)
		}
	}
	out.Reset()
	if err := runHelp([]string{"--json"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"name":"destroy"`, "forge generate model", "forge generate controller", "forge generate mailer"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the command catalog lacks %q", want)
		}
	}
}

// TestControllerPermissionMigrationRunsOnPostgreSQL applies a guarded
// controller's permission migration after `setup` and rolls it back: the up
// grants the permission to the owner role and runs again without failing; the
// down removes only what the up added, and refuses (changing nothing) while
// another role still holds the permission.
func TestControllerPermissionMigrationRunsOnPostgreSQL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, databaseConfig := postgrestest.NewWithConfig(t)
	root := resourceProject(t, "postgres://u:p@localhost/db")
	// generatorNow is the version of a Forge identity migration
	// (login_throttle_pair_scope): the generator moves the controller's
	// migration to the next second, so both sets load and run together.
	if _, err := generate(root, "controller", "reports", "monthly"); err != nil {
		t.Fatal(err)
	}
	up := readFile(t, filepath.Join(root, "db", "migrations", "20261020120001_create_reports_controller_permissions.up.sql"))

	migrator := func(sets ...fs.FS) *migrate.Migrator {
		t.Helper()
		loaded, err := migrate.LoadMigrationSets(sets...)
		if err != nil {
			t.Fatal(err)
		}
		m, err := postgres.NewMigrator(databaseConfig, loaded, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Close() })
		return m
	}
	// The identity tables and the owner role exist before the controller's
	// migration runs, as in an application that ran `setup` earlier.
	if _, err := migrator(authpostgres.Migrations()).Up(ctx); err != nil {
		t.Fatal(err)
	}
	repository, err := authpostgres.New(database)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := repository.ProvisionOwner(ctx, authpostgres.OwnerAccount{Organization: "Acme", TenantSlug: "acme", Email: "owner@example.com", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}
	application := migrator(authpostgres.Migrations(), os.DirFS(filepath.Join(root, "db", "migrations")))
	if _, err := application.Up(ctx); err != nil {
		t.Fatal(err)
	}
	count := func(query string) int {
		t.Helper()
		var n int
		if err := database.QueryRow(ctx, query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	const permissions = "SELECT count(*) FROM forge_permissions WHERE name = 'reports:read'"
	const grants = "SELECT count(*) FROM forge_role_permissions WHERE permission = 'reports:read'"
	if count(permissions) != 1 || count(grants) != 1 {
		t.Fatalf("after up: %d permission(s), %d grant(s); want the permission and the owner's grant", count(permissions), count(grants))
	}
	// The up is idempotent.
	if _, err := database.Exec(ctx, up); err != nil {
		t.Fatalf("running the up again: %v", err)
	}
	if count(permissions) != 1 || count(grants) != 1 {
		t.Fatal("running the up again changed the grants")
	}

	// A grant an administrator made is never revoked silently: the rollback
	// fails and changes nothing until it is revoked.
	if _, err := database.Exec(ctx, "INSERT INTO forge_roles (tenant_id, name) VALUES ($1, 'auditor')", owner.TenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, "INSERT INTO forge_role_permissions (role_id, permission) SELECT id, 'reports:read' FROM forge_roles WHERE name = 'auditor'"); err != nil {
		t.Fatal(err)
	}
	if _, err := application.Down(ctx, 1); err == nil {
		t.Fatal("the rollback removed a permission another role holds")
	}
	if count(permissions) != 1 || count(grants) != 2 {
		t.Fatalf("a failed rollback changed data: %d permission(s), %d grant(s)", count(permissions), count(grants))
	}
	if _, err := database.Exec(ctx, "DELETE FROM forge_role_permissions WHERE permission = 'reports:read' AND role_id IN (SELECT id FROM forge_roles WHERE name = 'auditor')"); err != nil {
		t.Fatal(err)
	}
	if _, err := application.Down(ctx, 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if count(permissions) != 0 || count(grants) != 0 {
		t.Fatalf("after down: %d permission(s), %d grant(s); want none", count(permissions), count(grants))
	}
	// Other permissions and the owner role's other grants are untouched.
	if count("SELECT count(*) FROM forge_role_permissions rp JOIN forge_roles r ON r.id = rp.role_id WHERE r.name = 'owner'") != count("SELECT count(*) FROM forge_permissions") {
		t.Error("the rollback changed the owner role's other grants")
	}
}
