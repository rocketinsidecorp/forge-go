package scaffold_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

func TestGenerateCreatesDockerizedApplication(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "billing-api")
	paths, err := scaffold.Generate(scaffold.Options{Name: "billing-api", Module: "example.com/acme/billing", Dir: dir, Database: "postgresql"})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"Dockerfile", ".dockerignore", "compose.yaml", ".github/workflows/ci.yml", ".github/dependabot.yml", "cmd/billing-api/main.go", "go.mod", "db/db.go", "db/migrations/.keep", ".env.development", ".env.local"} {
		if !contains(paths, filepath.FromSlash(required)) {
			t.Fatalf("missing %s in %v", required, paths)
		}
	}
	// The host port is derived from the name and must match in both files.
	compose, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`"127\.0\.0\.1:(2\d{4}):5432"`).FindSubmatch(compose)
	if match == nil {
		t.Fatalf("compose.yaml does not publish PostgreSQL on a 2xxxx host port:\n%s", compose)
	}
	port := string(match[1])
	assertContains(t, dir, map[string][]string{
		"Dockerfile": {
			"FROM gcr.io/distroless/static-debian12:nonroot",
			"USER nonroot:nonroot",
			"./cmd/billing-api",
			"FORGE_ENV=production",
			"FORGE_HTTP_TRANSPORT=trusted-proxy",
			`HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 CMD ["/app/server", "healthcheck"]`,
		},
		".dockerignore": {".git", ".env"},
		"compose.yaml": {
			"image: postgres:18-alpine",
			"env_file:\n    - .env.local",
			"POSTGRES_DB: billing_api_development",
			"postgres:/var/lib/postgresql",
			`command: ["migrate"]`,
			"condition: service_completed_successfully",
			`"127.0.0.1:8080:8080"`,
			`"127.0.0.1:` + port + `:5432"`,
		},
		".env.development": {"FORGE_DATABASE_URL=postgres://billing_api:development@127.0.0.1:" + port + "/billing_api_development?sslmode=disable", "FORGE_TEST_DATABASE_URL="},
		".env.local":       {"FORGE_AUTH_MFA_KEY="},
		".gitignore":       {".env.local"},
		"go.mod":           {"module example.com/acme/billing"},
		"cmd/billing-api/main.go": {
			`"example.com/acme/billing/app/bootstrap"`,
			`authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"`,
			"postgres.CommandsFromSets(authpostgres.Migrations(), db.Migrations())",
		},
		"app/bootstrap/bootstrap.go": {
			"postgres.Open(ctx, app.Config().Database)",
			`app.Readiness("database", database.Ping)`,
			"authpostgres.New(database)",
			"authpostgres.NewLoginThrottler(database, auth.DefaultLoginThrottleConfig())",
			"auth.NewLoginService(",
			"auth.WithSecurityAuditor(repository)",
			"password recovery delivery is not configured",
			"auth.NewAESGCMSecretCipherBase64(app.Config().Auth.MFAKey.Reveal())",
			"auth.NewMFAService(",
			"auth.WithMFAAuditor(repository)",
			`app.Handle("POST /auth/mfa/complete", mfaCompletion)`,
			`app.Handle("POST /auth/login", loginHandler)`,
			`app.Handle("POST /auth/logout", logoutHandler)`,
			`app.Handle("GET /v1/auth/session", middleware.Authenticate(http.HandlerFunc(currentSession)))`,
		},
		"db/db.go": {"//go:embed all:migrations"},
		"README.md": {
			"POST /auth/login",
			"POST /auth/logout",
			"`/auth/recovery/request`",
			"`/auth/recovery/reset`",
			"`/auth/cookie/login`",
			"GET /v1/auth/session",
			"POST /auth/mfa/complete",
			"FORGE_AUTH_MFA_KEY",
			"Forge's identity migrations",
		},
		".github/workflows/ci.yml": {"go test -race ./...", "tags: billing-api:ci", "image: postgres:18-alpine", "POSTGRES_PASSWORD: forge", "FORGE_TEST_DATABASE_URL: postgres://forge:forge@127.0.0.1:5432/postgres?sslmode=disable", `FORGE_TEST_REQUIRE_DATABASE: "true"`},
	})
	key := readEnvValue(t, filepath.Join(dir, ".env.local"), "FORGE_AUTH_MFA_KEY")
	if _, err := auth.NewAESGCMSecretCipherBase64(key); err != nil {
		t.Fatalf("generated MFA key is not accepted by the framework: %v", err)
	}
	localEnv, err := os.Stat(filepath.Join(dir, ".env.local"))
	if err != nil {
		t.Fatal(err)
	}
	if got := localEnv.Mode().Perm(); got != 0o600 {
		t.Fatalf(".env.local permissions = %04o, want 0600", got)
	}
}

func TestGenerateWithoutDatabase(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "edge")
	paths, err := scaffold.Generate(scaffold.Options{Name: "edge", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if contains(paths, filepath.FromSlash("db/db.go")) {
		t.Fatalf("database files generated without Database: %v", paths)
	}
	for path, forbidden := range map[string]string{
		"compose.yaml":               "postgres",
		"cmd/edge/main.go":           "postgres",
		"app/bootstrap/bootstrap.go": "postgres",
		".github/workflows/ci.yml":   "postgres",
		".env.development":           "DATABASE",
	} {
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), forbidden) {
			t.Errorf("%s mentions %q without a database", path, forbidden)
		}
	}
}

func assertContains(t *testing.T, dir string, expectations map[string][]string) {
	t.Helper()
	for path, fragments := range expectations {
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		for _, fragment := range fragments {
			if !strings.Contains(string(content), fragment) {
				t.Errorf("%s missing %q", path, fragment)
			}
		}
	}
}

func readEnvValue(t *testing.T, path, key string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	prefix := key + "="
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	t.Fatalf("%s is missing from %s", key, path)
	return ""
}

func TestGeneratedReadinessUsesFrameworkRoute(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "api")
	if _, err := scaffold.Generate(scaffold.Options{
		Name:     "api",
		Dir:      dir,
		Database: "postgresql",
	}); err != nil {
		t.Fatal(err)
	}

	bootstrap, err := os.ReadFile(filepath.Join(dir, "app", "bootstrap", "bootstrap.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, duplicate := range []string{
		`app.HandleFunc("GET /health/ready"`,
		"func ready(",
	} {
		if strings.Contains(string(bootstrap), duplicate) {
			t.Errorf("generated bootstrap duplicates framework readiness route with %q", duplicate)
		}
	}
}

func TestGenerateRefusesNonEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(existing, []byte("user data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := scaffold.Generate(scaffold.Options{Name: "app", Dir: dir}); err == nil {
		t.Fatal("expected non-empty directory to be rejected")
	}
	content, err := os.ReadFile(existing)
	if err != nil || string(content) != "user data" {
		t.Fatalf("existing file changed: %q %v", content, err)
	}
}

func TestNormalizeValidatesInput(t *testing.T) {
	valid, err := scaffold.Normalize(scaffold.Options{Name: "my-app"})
	if err != nil {
		t.Fatal(err)
	}
	if valid.Module != "my-app" || valid.Dir != "my-app" {
		t.Fatalf("defaults not applied: %+v", valid)
	}
	invalid := []scaffold.Options{
		{Name: ""},
		{Name: "MyApp"},
		{Name: "../escape"},
		{Name: "app/nested"},
		{Name: "1app"},
		{Name: "app", Module: "example.com/../evil"},
		{Name: "app", Module: "/absolute"},
		{Name: "app", Module: "has space"},
		{Name: "app", Module: `quote"injection`},
	}
	for _, opts := range invalid {
		if _, err := scaffold.Normalize(opts); err == nil {
			t.Errorf("expected %+v to be rejected", opts)
		}
	}
}

func FuzzNormalizeNeverAcceptsPathEscapes(f *testing.F) {
	for _, seed := range []string{"app", "../x", "a/b", "a\\b", "ok-name"} {
		f.Add(seed, seed)
	}
	f.Fuzz(func(t *testing.T, name, module string) {
		opts, err := scaffold.Normalize(scaffold.Options{Name: name, Module: module})
		if err != nil {
			return
		}
		if strings.ContainsAny(opts.Name, `/\.`) || strings.Contains(opts.Module, "..") ||
			strings.ContainsAny(opts.Module, "\"`\\ \n") || strings.HasPrefix(opts.Module, "/") {
			t.Fatalf("unsafe options accepted: %+v", opts)
		}
	})
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestGenerateForEveryDatabase(t *testing.T) {
	tests := []struct {
		database  string
		want      map[string][]string
		forbidden map[string]string
	}{
		{
			database: "mysql",
			want: map[string][]string{
				"compose.yaml":                    {"image: mysql:8.4", "MYSQL_DATABASE: shop_development", "mysql:/var/lib/mysql", "FORGE_DATABASE_URL: mysql://shop:development@mysql:3306/shop_development", `"mysqladmin", "ping"`},
				".env.development":                {"FORGE_DATABASE_URL=mysql://shop:development@127.0.0.1:", "FORGE_TEST_DATABASE_URL=mysql://root:development@127.0.0.1:"},
				"cmd/shop/main.go":                {`"github.com/rocketinsidecorp/forge-go/mysql"`, "mysql.Commands(db.Migrations())"},
				"app/bootstrap/bootstrap.go":      {"mysql.Open(ctx, app.Config().Database)"},
				"app/bootstrap/bootstrap_test.go": {`"github.com/rocketinsidecorp/forge-go/mysql/mysqltest"`, "mysqltest.Config(t)"},
				".github/workflows/ci.yml":        {"image: mysql:8.4", "MYSQL_ROOT_PASSWORD: forge", "FORGE_TEST_DATABASE_URL: mysql://root:forge@127.0.0.1:3306/mysql"},
				"README.md":                       {"MySQL 8.4", "`tls=true`"},
			},
			forbidden: map[string]string{
				"compose.yaml":               "postgres",
				"Dockerfile":                 "storage",
				"app/bootstrap/bootstrap.go": "authpostgres",
				"cmd/shop/main.go":           "authpostgres",
			},
		},
		{
			database: "mariadb",
			want: map[string][]string{
				"compose.yaml":             {"image: mariadb:11.8", "MARIADB_DATABASE: shop_development", "FORGE_DATABASE_URL: mysql://shop:development@mariadb:3306/shop_development", "healthcheck.sh"},
				"cmd/shop/main.go":         {"mysql.Commands(db.Migrations())"},
				".github/workflows/ci.yml": {"image: mariadb:11.8", "MARIADB_ROOT_PASSWORD: forge", "healthcheck.sh --connect --innodb_initialized", "FORGE_TEST_DATABASE_URL: mysql://root:forge@127.0.0.1:3306/mysql"},
				"README.md":                {"MariaDB 11.8"},
			},
			forbidden: map[string]string{
				"compose.yaml":               "mysql:8.4",
				"app/bootstrap/bootstrap.go": "authpostgres",
				"cmd/shop/main.go":           "authpostgres",
			},
		},
		{
			database: "sqlite3", // alias
			want: map[string][]string{
				"compose.yaml":                    {"FORGE_DATABASE_URL: sqlite:///app/storage/development.db", "storage:/app/storage", `command: ["migrate"]`},
				".env.development":                {"FORGE_DATABASE_URL=sqlite:storage/development.db"},
				"Dockerfile":                      {"COPY --from=build --chown=nonroot:nonroot /out/storage /app/storage", "FORGE_DATABASE_URL=sqlite:///app/storage/production.db"},
				".gitignore":                      {"/storage/"},
				".dockerignore":                   {"storage"},
				"cmd/shop/main.go":                {"sqlite.Commands(db.Migrations())"},
				"app/bootstrap/bootstrap_test.go": {"sqlitetest.Config(t)"},
			},
			forbidden: map[string]string{
				"compose.yaml":               "image:",
				".github/workflows/ci.yml":   "services:",
				".env.development":           "FORGE_TEST_DATABASE_URL",
				"app/bootstrap/bootstrap.go": "authpostgres",
				"cmd/shop/main.go":           "authpostgres",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.database, func(t *testing.T) {
			// Use t.TempDir() directly for better isolation between subtests
			dir := t.TempDir()
			if _, err := scaffold.Generate(scaffold.Options{Name: "shop", Dir: dir, Database: test.database}); err != nil {
				t.Fatal(err)
			}
			assertContains(t, dir, test.want)
			for path, fragment := range test.forbidden {
				content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(content), fragment) {
					t.Errorf("%s contains %q", path, fragment)
				}
			}
		})
	}
}

func TestNormalizeDatabase(t *testing.T) {
	for input, want := range map[string]string{"postgres": "postgresql", "PostgreSQL": "postgresql", "pg": "postgresql", "sqlite3": "sqlite", "none": "", "": "", "mariadb": "mariadb"} {
		opts, err := scaffold.Normalize(scaffold.Options{Name: "app", Database: input})
		if err != nil || opts.Database != want {
			t.Errorf("%q: got %q err=%v, want %q", input, opts.Database, err, want)
		}
	}
	if _, err := scaffold.Normalize(scaffold.Options{Name: "app", Database: "oracle"}); err == nil {
		t.Error("unsupported database accepted")
	}
}

func TestLongNamesRespectDatabaseIdentifierLimits(t *testing.T) {
	name := "a" + strings.Repeat("b-", 31) // 63 characters, the maximum
	dir := filepath.Join(t.TempDir(), "long")
	if _, err := scaffold.Generate(scaffold.Options{Name: name, Dir: dir, Database: "mysql"}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	user := regexp.MustCompile(`MYSQL_USER: (\S+)`).FindSubmatch(content)
	database := regexp.MustCompile(`MYSQL_DATABASE: (\S+)`).FindSubmatch(content)
	if user == nil || database == nil || len(user[1]) > 32 || len(database[1]) > 63 {
		t.Fatalf("identifiers exceed limits: user=%q database=%q", user, database)
	}
}

// TestRootAppNameDoesNotCollideWithDatabaseRoot is a regression test: the
// MySQL and MariaDB images refuse MYSQL_USER=root.
func TestRootAppNameDoesNotCollideWithDatabaseRoot(t *testing.T) {
	t.Parallel()
	for _, database := range []string{"mysql", "mariadb"} {
		dir := filepath.Join(t.TempDir(), "root")
		if _, err := scaffold.Generate(scaffold.Options{Name: "root", Dir: dir, Database: database}); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if regexp.MustCompile(`(MYSQL|MARIADB)_USER: root\n`).Match(content) {
			t.Errorf("%s: compose creates a second root user", database)
		}
	}
}

// TestGenerateServerRenderedHTML checks the HTML layer every application
// gets, with and without a database: the layout, the flash partial, the
// stylesheet, the embedded views package and the middleware wiring.
func TestGenerateServerRenderedHTML(t *testing.T) {
	t.Parallel()
	for _, database := range []string{"", "sqlite", "postgresql"} {
		t.Run("database "+database, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "shop")
			paths, err := scaffold.Generate(scaffold.Options{Name: "shop", Dir: dir, Database: database})
			if err != nil {
				t.Fatal(err)
			}
			for _, required := range []string{"app/views/views.go", "app/views/views_test.go", "app/views/layouts/application.gohtml", "app/views/shared/_flash.gohtml", "app/bootstrap/assets/application.css"} {
				if !contains(paths, filepath.FromSlash(required)) {
					t.Fatalf("missing %s in %v", required, paths)
				}
			}
			assertContains(t, dir, map[string][]string{
				"app/views/layouts/application.gohtml": {
					`<html lang="en">`,
					`<title>{{block "title" .}}shop{{end}}</title>`,
					"{{csrfMeta}}",
					`<link rel="stylesheet" href="/assets/application.css">`,
					`<a class="skip-link" href="#main">`,
					`{{template "shared/_flash" .}}`,
					`{{block "content" .}}{{end}}`,
				},
				"app/views/shared/_flash.gohtml": {"{{range flashes}}", `role="{{if eq .Kind "alert"}}alert{{else}}status{{end}}"`, "{{.Text}}"},
				"app/views/views.go":             {"//go:embed */*.gohtml", "forgeviews.Load(files, forgeviews.Options{})"},
				"app/bootstrap/welcome.go":       {"//go:embed assets/forge-go-logo.png assets/application.css"},
				"app/bootstrap/bootstrap.go": {
					"if err := useBrowserPipeline(app); err != nil {",
					"csrf.New(csrf.Options{})",
					"[]func(http.Handler) http.Handler{web.MethodOverride, protection.Handler, web.Flash}",
					"app.Use(middleware)",
				},
				"app/bootstrap/bootstrap_test.go": {"func TestBrowserPipelineRequiresCSRFToken(t *testing.T) {"},
			})
			for _, path := range []string{"app/views/layouts/application.gohtml", "app/views/shared/_flash.gohtml"} {
				content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
				if err != nil {
					t.Fatal(err)
				}
				for _, forbidden := range []string{"<script", "<style", " style=", " on", "http://", "https://", "[["} {
					if strings.Contains(string(content), forbidden) {
						t.Errorf("%s contains %q: views must be CSP-compatible and use no external resources", path, forbidden)
					}
				}
			}
		})
	}
}

// TestGenerateAuthenticationPagesForPostgreSQLOnly: PostgreSQL applications,
// whose identity Forge persists, get the sign-in pages wired by default;
// every database application gets an empty, valid seed function and the seed
// command; nothing else changes for other databases.
func TestGenerateAuthenticationPagesForPostgreSQLOnly(t *testing.T) {
	t.Parallel()
	authFiles := []string{
		"app/adapters/htmlauth/htmlauth.go", "app/adapters/htmlauth/htmlauth_test.go",
		"app/views/sessions/new.gohtml", "app/views/sessions/mfa.gohtml", "app/views/sessions/_sign_out.gohtml",
		"app/views/passwords/new.gohtml", "app/views/passwords/edit.gohtml",
	}
	for _, database := range []string{"", "sqlite", "mysql", "postgresql"} {
		t.Run("database "+database, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "shop")
			paths, err := scaffold.Generate(scaffold.Options{Name: "shop", Module: "example.com/shop", Dir: dir, Database: database})
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range authFiles {
				if contains(paths, filepath.FromSlash(path)) != (database == "postgresql") {
					t.Errorf("%s generated=%v for database %q", path, contains(paths, filepath.FromSlash(path)), database)
				}
			}
			if contains(paths, filepath.FromSlash("db/seeds.go")) != (database != "") {
				t.Errorf("db/seeds.go generated for database %q: %v", database, contains(paths, filepath.FromSlash("db/seeds.go")))
			}
			switch database {
			case "postgresql":
				assertContains(t, dir, map[string][]string{
					"app/bootstrap/bootstrap.go": {
						`"example.com/shop/app/adapters/htmlauth"`, "auth.NewBrowser(browserOptions)", "htmlauth.Register(app, browser, renderer)",
						"if mailConfigured {\n\t\tbrowserOptions.Recovery = recoveryService", "recoverymail.New(sender, recoverymail.Config{", "FORGE_AUTH_PASSWORD_RESET_URL",
						"middleware.AuthenticateBrowser(htmlauth.SignInPath, page)",
					},
					"cmd/shop/main.go":         {"authpostgres.SetupCommand()", "postgres.SeedCommand(db.Seed)"},
					"db/seeds.go":              {"func Seed(ctx context.Context, tx pgx.Tx) error {", "`shop setup`"},
					"app/bootstrap/welcome.go": {`href="/session/new"`},
					"README.md":                {"forge setup --tenant acme --email you@example.com", "/session/new"},
				})
			case "sqlite", "mysql":
				assertContains(t, dir, map[string][]string{
					"cmd/shop/main.go": {database + ".SeedCommand(db.Seed)"},
					"db/seeds.go":      {"func Seed(ctx context.Context, tx *sql.Tx) error {"},
				})
			}
			for _, path := range paths {
				if !strings.HasPrefix(filepath.ToSlash(path), "app/views/") || !strings.HasSuffix(path, ".gohtml") {
					continue
				}
				content, err := os.ReadFile(filepath.Join(dir, path))
				if err != nil {
					t.Fatal(err)
				}
				for _, forbidden := range []string{"<script", "<style", " style=", " onclick", "http://", "https://", "[["} {
					if strings.Contains(string(content), forbidden) {
						t.Errorf("%s contains %q: views must be CSP-compatible and use no external resources", path, forbidden)
					}
				}
			}
		})
	}
}

func TestAuthenticationFilesMatchForgeNew(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "shop")
	if _, err := scaffold.Generate(scaffold.Options{Name: "shop", Module: "example.com/shop", Dir: dir, Database: "postgresql"}); err != nil {
		t.Fatal(err)
	}
	files, err := scaffold.AuthenticationFiles("shop", "example.com/shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 7 {
		t.Fatalf("got %d authentication files", len(files))
	}
	for _, file := range files {
		generated, err := os.ReadFile(filepath.Join(dir, file.Path))
		if err != nil {
			t.Fatal(err)
		}
		if string(generated) != string(file.Content) {
			t.Errorf("%s differs between forge new and AuthenticationFiles", file.Path)
		}
	}
	base, err := scaffold.ViewBase("shop")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range base {
		if strings.Contains(filepath.ToSlash(file.Path), "sessions/") || strings.Contains(filepath.ToSlash(file.Path), "passwords/") {
			t.Errorf("the views base must not include authentication pages: %s", file.Path)
		}
	}
}
