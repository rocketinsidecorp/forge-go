package cli

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

// browserOnlyPrefixes are the paths only a full-stack application has.
var browserOnlyPrefixes = []string{"app/views/", "app/assets/", "app/javascript/", "config/locales/", "app/adapters/htmlauth/", "app/adapters/htmlui/"}

// assertNoBrowserFiles fails when dir holds a view, an asset, a translation or
// an HTML adapter.
func assertNoBrowserFiles(t *testing.T, dir string) {
	t.Helper()
	for path := range fileTree(t, dir) {
		if strings.HasPrefix(path, "vendor/") {
			continue
		}
		for _, prefix := range browserOnlyPrefixes {
			if strings.HasPrefix(path, prefix) {
				t.Errorf("an API-only application has %s", path)
			}
		}
		if strings.HasSuffix(path, ".gohtml") && !strings.HasPrefix(path, "app/mailers/templates/") {
			t.Errorf("an API-only application has the view %s", path)
		}
	}
}

// apiOnlyProject is `forge new shop --api -d DATABASE` without resolving
// dependencies, for the generators that only read and write files.
func apiOnlyProject(t *testing.T, database string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "shop")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"new", "shop", "--api", "--module", "example.com/shop", "--dir", dir, "-d", database, "--skip-deps"}, &stdout, &stderr); code != 0 {
		t.Fatalf("forge new --api: code=%d %s %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "curl http://127.0.0.1:8080/ and http://127.0.0.1:8080/health (JSON)") {
		t.Errorf("forge new --api prints the welcome screen in its next steps:\n%s", stdout.String())
	}
	return dir
}

// withoutDatabaseURL is the process environment without FORGE_DATABASE_URL,
// so the generators read the database from the application's env files.
func withoutDatabaseURL() []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "FORGE_DATABASE_URL=") {
			env = append(env, entry)
		}
	}
	return env
}

func TestNewAPIGeneratesNoViewsAssetsOrBrowserPipeline(t *testing.T) {
	t.Parallel()
	for _, database := range []string{"postgresql", "mysql", "mariadb", "sqlite", "none"} {
		t.Run(database, func(t *testing.T) {
			t.Parallel()
			dir := apiOnlyProject(t, database)
			assertNoBrowserFiles(t, dir)
			bootstrap := readFile(t, filepath.Join(dir, "app", "bootstrap", "bootstrap.go"))
			if !strings.Contains(bootstrap, scaffold.APIOnlyMarker) || strings.Contains(bootstrap, "csrf") || strings.Contains(bootstrap, "web.Flash") {
				t.Errorf("bootstrap.go is not API-only:\n%s", bootstrap)
			}
		})
	}
}

// TestNewAPIConflictsLikeForgeRust: Forge-Rust declares --api-auth and
// --locale conflicts_with = "api" (crates/forge-cli/src/cli.rs), so naming
// either with --api is a usage error (exit 2) that writes nothing, whatever
// the value.
func TestNewAPIConflictsLikeForgeRust(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"--api", "--api-auth"}, "--api-auth cannot be used with --api"},
		{[]string{"--api-auth", "--api"}, "--api-auth cannot be used with --api"},
		{[]string{"--api", "--locale", "pt-BR"}, "--locale cannot be used with --api"},
		{[]string{"--api", "--locale", "en"}, "--locale cannot be used with --api"},
		{[]string{"--api", "-d", "sqlite", "--api-auth"}, "--api-auth cannot be used with --api"},
	} {
		parent := t.TempDir()
		dir := filepath.Join(parent, "shop")
		var stdout, stderr bytes.Buffer
		code := Run(append([]string{"new", "shop", "--dir", dir, "--skip-deps"}, test.args...), &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), test.want) {
			t.Errorf("forge new %v: code=%d stderr=%q, want 2 and %q", test.args, code, stderr.String(), test.want)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("forge new %v created %s", test.args, dir)
		}
	}
}

// TestGeneratorsInAnAPIOnlyApplication mirrors Forge-Rust: the generators
// that write pages (view, scaffold, an HTML controller, authentication)
// refuse an API-only application without writing anything, also with
// --dry-run; the others write no view, asset or translation.
func TestGeneratorsInAnAPIOnlyApplication(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	env := withoutDatabaseURL()
	for _, database := range []string{"postgresql", "sqlite"} {
		t.Run(database, func(t *testing.T) {
			t.Parallel()
			dir := apiOnlyProject(t, database)
			before := fileTree(t, dir)
			for _, refused := range [][]string{
				{"view", "welcome"},
				{"view", "welcome", "--dry-run"},
				{"scaffold", "post", "title:string", "--global"},
				{"scaffold", "post", "title:string", "--global", "--dry-run"},
				{"controller", "pages", "index"},
				{"controller", "pages", "index", "--public"},
				{"authentication"},
				{"authentication", "--api-auth"},
				{"authentication", "--dry-run"},
			} {
				var output bytes.Buffer
				err := runGenerateEnv(refused, &output, dir, env, now)
				if err == nil || !strings.Contains(err.Error(), "it was created with `forge new --api`") {
					t.Errorf("forge generate %v = %v, want the --api refusal\n%s", refused, err, output.String())
				}
				if after := fileTree(t, dir); !maps.Equal(after, before) {
					t.Fatalf("the refused forge generate %v changed the application", refused)
				}
			}

			for _, args := range [][]string{
				{"resource", "product", "name:string"},
				{"model", "tag", "label:string", "--global"},
				{"controller", "status_checks", "index", "show", "--api"},
				{"controller", "pings", "index", "--api", "--public"},
				{"mailer", "user", "welcome:name"},
				{"job", "cleanup"},
				{"job", "send_welcome", "user_id:uuid", "--queue"},
				{"migration", "create_widgets"},
			} {
				var output bytes.Buffer
				if err := runGenerateEnv(args, &output, dir, env, now); err != nil {
					t.Fatalf("forge generate %v: %v\n%s", args, err, output.String())
				}
				// No Forge identity (decision D31): nothing wires it: no identityRoutes, no auth
				// middleware, no permission migration. (The mailer's wiring
				// names identityRoutes only for full-stack applications.)
				if args[0] != "mailer" && (strings.Contains(output.String(), "identityRoutes") || strings.Contains(output.String(), "_permissions.up.sql") || strings.Contains(output.String(), "authhttp.NewMiddleware")) {
					t.Errorf("forge generate %v prints identity wiring an API-only application lacks:\n%s", args, output.String())
				}
				if strings.Contains(output.String(), "useBrowserPipeline") || strings.Contains(output.String(), "views.New") {
					t.Errorf("forge generate %v prints browser wiring:\n%s", args, output.String())
				}
				if (args[0] == "resource" || args[0] == "controller") && !strings.Contains(output.String(), "wire  app/bootstrap/routes_gen.go") {
					t.Errorf("forge generate %v did not wire its routes into the seam:\n%s", args, output.String())
				}
			}
			assertNoBrowserFiles(t, dir)
		})
	}
}
