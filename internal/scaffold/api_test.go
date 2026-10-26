package scaffold_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

// apiOnlyFiles is the golden file list of `forge new shop --api` on
// PostgreSQL, without the observability stack (TestGenerateObservabilityStack
// covers it): no app/views, app/assets, app/javascript, config/locales or
// app/adapters/htmlauth.
var apiOnlyFiles = []string{
	".dockerignore",
	".env.development",
	".env.local",
	".github/dependabot.yml",
	".github/workflows/ci.yml",
	".github/workflows/release.yml",
	".gitignore",
	"Dockerfile",
	"README.md",
	"app/bootstrap/bootstrap.go",
	"app/bootstrap/bootstrap_test.go",
	"app/bootstrap/routes_gen.go",
	"app/bootstrap/welcome.go",
	"app/bootstrap/welcome_test.go",
	"cmd/shop/main.go",
	"compose.observability.yaml",
	"compose.yaml",
	"config/credentials/development.json.enc",
	"config/credentials/development.key",
	"db/db.go",
	"db/migrations/.keep",
	"db/seeds.go",
	"docker/postgres/init.sql",
	"go.mod",
}

// browserOnlyDirectories hold what only a full-stack application has.
var browserOnlyDirectories = []string{"app/views/", "app/assets/", "app/javascript/", "config/locales/", "app/adapters/htmlauth/"}

func generateAPIOnly(t *testing.T, database string) (string, []string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "shop")
	paths, err := scaffold.Generate(scaffold.Options{Name: "shop", Module: "example.com/shop", Dir: dir, Database: database, API: true})
	if err != nil {
		t.Fatal(err)
	}
	var generated []string
	for _, path := range paths {
		if path = filepath.ToSlash(path); !strings.HasPrefix(path, "observability/") {
			generated = append(generated, path)
		}
	}
	slices.Sort(generated)
	return dir, generated
}

func TestGenerateAPIOnlyApplicationFileList(t *testing.T) {
	t.Parallel()
	_, paths := generateAPIOnly(t, "postgresql")
	if !slices.Equal(paths, apiOnlyFiles) {
		t.Fatalf("forge new --api generated\n%s\nwant\n%s", strings.Join(paths, "\n"), strings.Join(apiOnlyFiles, "\n"))
	}
}

func TestGenerateAPIOnlyApplicationForEveryDatabase(t *testing.T) {
	t.Parallel()
	for _, database := range []string{"postgresql", "mysql", "mariadb", "sqlite", ""} {
		t.Run("database "+database, func(t *testing.T) {
			t.Parallel()
			dir, paths := generateAPIOnly(t, database)
			for _, path := range paths {
				for _, prefix := range browserOnlyDirectories {
					if strings.HasPrefix(path, prefix) {
						t.Errorf("forge new --api generated %s", path)
					}
				}
				if strings.HasSuffix(path, ".gohtml") || strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".css") || strings.HasSuffix(path, ".png") {
					t.Errorf("forge new --api generated the view or asset %s", path)
				}
			}
			for _, required := range []string{"app/bootstrap/bootstrap.go", "app/bootstrap/welcome.go", "app/bootstrap/welcome_test.go", "cmd/shop/main.go", "README.md"} {
				if !slices.Contains(paths, required) {
					t.Errorf("forge new --api did not generate %s", required)
				}
			}
			read := func(path string) string {
				t.Helper()
				content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
				if err != nil {
					t.Fatal(err)
				}
				return string(content)
			}
			bootstrap := read("app/bootstrap/bootstrap.go")
			if !strings.Contains(bootstrap, scaffold.APIOnlyMarker) {
				t.Errorf("bootstrap.go does not carry %q", scaffold.APIOnlyMarker)
			}
			for _, forbidden := range []string{"csrf", "useBrowserPipeline", "web.MethodOverride", "web.Flash", "appassets", "/assets/", "config/locales", "translator", "htmlauth", "identityRoutes", "auth.NewBrowser"} {
				if strings.Contains(bootstrap, forbidden) {
					t.Errorf("bootstrap.go of an API-only application mentions %q", forbidden)
				}
			}
			for _, want := range []string{`app.Handle("GET /{$}", openapi.Typed(`, `app.Handle("GET /v1/status", openapi.Typed(`, "func routes(_ context.Context, app *forge.App) error"} {
				if !strings.Contains(bootstrap, want) {
					t.Errorf("bootstrap.go lacks %q", want)
				}
			}
			welcome := read("app/bootstrap/welcome.go")
			if !strings.Contains(welcome, "type welcomeResponse struct") || strings.Contains(welcome, "<html") || strings.Contains(welcome, "i18n") {
				t.Errorf("welcome.go is not the JSON welcome:\n%s", welcome)
			}
			tests := read("app/bootstrap/bootstrap_test.go")
			if !strings.Contains(tests, "func TestNoBrowserPipeline(") || strings.Contains(tests, "TestBrowserPipelineRequiresCSRFToken") {
				t.Errorf("bootstrap_test.go does not test the absent browser pipeline")
			}
			// Decision D15: on PostgreSQL the identity, audit and session
			// schema is composed as in Forge-Rust's --api applications, but
			// no setup command (no sign-in pages to set up).
			main := read("cmd/shop/main.go")
			if strings.Contains(main, "SetupCommand") {
				t.Errorf("main.go of an API-only application has the setup command:\n%s", main)
			}
			if composes := strings.Contains(main, "postgres.CommandsFromSets(authpostgres.Migrations(), jobspostgres.Migrations(), db.Migrations())"); composes != (database == "postgresql") {
				t.Errorf("main.go composes the identity schema = %v on %q:\n%s", composes, database, main)
			}
			if env := read(".env.development"); strings.Contains(env, "FORGE_AUTH_") || strings.Contains(env, "PASSWORD_RESET") {
				t.Errorf(".env.development of an API-only application configures the sign-in pages:\n%s", env)
			}
			readme := read("README.md")
			for _, forbidden := range []string{`src="app/assets/`, "## HTML pages", "## Translations", "## JavaScript and styles", "## Sign in", "forge generate view", "forge setup", "sign-in\nthrottles"} {
				if strings.Contains(readme, forbidden) {
					t.Errorf("README.md of an API-only application mentions %q", forbidden)
				}
			}
			for _, want := range []string{"## API only", "curl http://127.0.0.1:8080/health", "forge generate controller status_checks index --api"} {
				if !strings.Contains(readme, want) {
					t.Errorf("README.md lacks %q", want)
				}
			}
			if database != "" {
				if env := read(".env.local"); strings.Contains(env, "FORGE_AUTH_MFA_KEY") {
					t.Errorf(".env.local holds an MFA key without sign-in pages:\n%s", env)
				}
			}
		})
	}
}

// TestFullStackApplicationsDoNotCarryTheAPIOnlyMarker: the generators add
// the views base to an application whose bootstrap.go lacks the marker, so a
// full-stack application must never carry it.
func TestFullStackApplicationsDoNotCarryTheAPIOnlyMarker(t *testing.T) {
	t.Parallel()
	for _, opts := range []scaffold.Options{{Database: "postgresql"}, {Database: "postgresql", APIAuth: true}, {Database: "sqlite"}, {}} {
		opts.Name, opts.Dir = "shop", filepath.Join(t.TempDir(), "shop")
		if _, err := scaffold.Generate(opts); err != nil {
			t.Fatal(err)
		}
		bootstrap, err := os.ReadFile(filepath.Join(opts.Dir, "app", "bootstrap", "bootstrap.go"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(bootstrap), scaffold.APIOnlyMarker) || strings.Contains(string(bootstrap), "(forge new --api)") {
			t.Errorf("%+v: a full-stack bootstrap.go carries the API-only marker", opts)
		}
	}
}

// TestNormalizeRefusesWhatConflictsWithAPI mirrors Forge-Rust's
// conflicts_with = "api" on --api-auth and --locale.
func TestNormalizeRefusesWhatConflictsWithAPI(t *testing.T) {
	t.Parallel()
	for _, opts := range []scaffold.Options{
		{Name: "shop", Database: "postgresql", API: true, APIAuth: true},
		{Name: "shop", Database: "postgresql", API: true, Locale: "pt-BR"},
		{Name: "shop", API: true, Locale: "es"},
	} {
		if _, err := scaffold.Normalize(opts); err == nil || !strings.Contains(err.Error(), "--api") {
			t.Errorf("Normalize(%+v) = %v, want a refusal naming --api", opts, err)
		}
	}
	for _, opts := range []scaffold.Options{{Name: "shop", Database: "postgresql", API: true}, {Name: "shop", API: true, Locale: "en"}} {
		if _, err := scaffold.Normalize(opts); err != nil {
			t.Errorf("Normalize(%+v) = %v", opts, err)
		}
	}
}
