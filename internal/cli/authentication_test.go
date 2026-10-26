package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

func runAuthentication(root string, args ...string) (string, error) {
	var out bytes.Buffer
	err := runGenerateEnv(append([]string{"authentication"}, args...), &out, root, nil, time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC))
	return out.String(), err
}

func TestGenerateAuthenticationRefusesOtherDatabases(t *testing.T) {
	t.Parallel()
	for _, current := range []struct{ url, names string }{
		{"mysql://u:p@localhost/db", "MySQL or MariaDB"},
		{"sqlite:storage/development.db", "SQLite"},
		{"", "no database"},
	} {
		root := resourceProject(t, current.url)
		before := snapshotTree(t, root)
		_, err := runAuthentication(root)
		if err == nil || !strings.Contains(err.Error(), "needs PostgreSQL") || !strings.Contains(err.Error(), "only in PostgreSQL today") || !strings.Contains(err.Error(), current.names) {
			t.Fatalf("%q: err = %v", current.url, err)
		}
		if after := snapshotTree(t, root); !slices.Equal(before, after) {
			t.Fatalf("%q: a refused run wrote files", current.url)
		}
	}
	// PostgreSQL without the identity migrations has no identity tables.
	bare := resourceProject(t, "postgres://u:p@localhost/db")
	writeFile(t, filepath.Join(bare, "cmd", "shop", "main.go"), "package main\n\nfunc main() {}\n")
	if _, err := runAuthentication(bare); err == nil || !strings.Contains(err.Error(), "authpostgres.Migrations()") {
		t.Fatalf("no identity migrations: err = %v", err)
	}
	if _, err := runAuthentication(resourceProject(t, "postgres://u:p@localhost/db"), "--force"); err == nil || !strings.Contains(err.Error(), authenticationUsage) {
		t.Fatalf("unknown flag: err = %v", err)
	}
}

func TestGenerateAuthenticationDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "postgres://u:p@localhost/db")
	before := snapshotTree(t, root)
	output, err := runAuthentication(root, "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if after := snapshotTree(t, root); !slices.Equal(before, after) {
		t.Fatal("--dry-run wrote files")
	}
	for _, want := range []string{
		"would create  " + filepath.Join("app", "adapters", "htmlauth", "htmlauth.go"),
		"would create  " + filepath.Join("app", "views", "sessions", "new.gohtml"),
		"would create  " + filepath.Join("app", "views", "passwords", "edit.gohtml"),
		// The project has no views package yet: the base comes too.
		"would create  " + filepath.Join("app", "views", "views.go"),
		`"example.com/shop/app/adapters/htmlauth"`,
		"auth.NewBrowser(auth.BrowserOptions{",
		"htmlauth.Register(app, browser, renderer, middleware)",
		"middleware.AuthenticateBrowser(htmlauth.SignInPath, page)",
		"authpostgres.SetupCommand(),",
		filepath.Join("cmd", "shop", "main.go"),
		"WARNING: app/bootstrap/bootstrap.go does not appear to install csrf.Protection",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("dry-run output is missing %q:\n%s", want, output)
		}
	}
}

// TestGenerateAuthenticationAddsWhatForgeNewGenerates removes the pages from
// a fresh PostgreSQL application, as in one created before they existed, and
// regenerates them: the files are identical, and a second run refuses to
// overwrite anything.
func TestGenerateAuthenticationAddsWhatForgeNewGenerates(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "shop")
	if _, err := scaffold.Generate(scaffold.Options{Name: "shop", Module: "example.com/shop", Dir: dir, Database: "postgresql"}); err != nil {
		t.Fatal(err)
	}
	expected, err := scaffold.AuthenticationFiles("shop", "example.com/shop")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join("app", "adapters", "htmlauth"), filepath.Join("app", "views", "sessions"), filepath.Join("app", "views", "passwords"), filepath.Join("app", "views", "mfa"), filepath.Join("config", "locales", "authentication.en.json")} {
		if err := os.RemoveAll(filepath.Join(dir, path)); err != nil {
			t.Fatal(err)
		}
	}
	output, err := runAuthentication(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "WARNING") || strings.Contains(output, filepath.Join("app", "views", "views.go")) {
		t.Errorf("a forge new application has the browser pipeline and views package:\n%s", output)
	}
	for _, file := range expected {
		content, err := os.ReadFile(filepath.Join(dir, file.Path))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(content, file.Content) {
			t.Errorf("%s differs from forge new", file.Path)
		}
		if !strings.Contains(output, "create  "+file.Path) {
			t.Errorf("output does not list %s", file.Path)
		}
	}
	before := snapshotTree(t, dir)
	if _, err := runAuthentication(dir); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("second run: err = %v", err)
	}
	if after := snapshotTree(t, dir); !slices.Equal(before, after) {
		t.Fatal("a refused run changed files")
	}
}

// TestGenerateAuthenticationRespectsAPIAuth: like forge new, the generator
// adds only the HTML flow unless --api-auth asks for the JSON authentication
// APIs too; without the flag it lists the JSON routes an older bootstrap.go
// still mounts, with the flag it prints their wiring unless all are there.
func TestGenerateAuthenticationRespectsAPIAuth(t *testing.T) {
	t.Parallel()
	removePages := func(t *testing.T, dir string) {
		t.Helper()
		for _, path := range []string{filepath.Join("app", "adapters", "htmlauth"), filepath.Join("app", "views", "sessions"), filepath.Join("app", "views", "passwords"), filepath.Join("app", "views", "mfa"), filepath.Join("config", "locales", "authentication.en.json")} {
			if err := os.RemoveAll(filepath.Join(dir, path)); err != nil {
				t.Fatal(err)
			}
		}
	}
	generate := func(t *testing.T, apiAuth bool) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "shop")
		if _, err := scaffold.Generate(scaffold.Options{Name: "shop", Module: "example.com/shop", Dir: dir, Database: "postgresql", APIAuth: apiAuth}); err != nil {
			t.Fatal(err)
		}
		removePages(t, dir)
		return dir
	}

	// A default application: no JSON routes, no JSON wiring, no note.
	output, err := runAuthentication(generate(t, false), "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "auth.NewLoginHandler") || strings.Contains(output, "NOTE:") || strings.Contains(output, "POST /auth/cookie/login") {
		t.Errorf("the HTML-only wiring mentions the JSON APIs:\n%s", output)
	}

	// --api-auth on a default application prints the JSON wiring.
	output, err = runAuthentication(generate(t, false), "--api-auth", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"JSON authentication APIs (--api-auth)", "auth.NewLoginHandler(login)", `app.Handle("POST /auth/cookie/login", cookieLoginHandler)`, "auth.NewMFACompletionHandler(mfaService)", "/auth/recovery/request", "forge new NAME --api-auth"} {
		if !strings.Contains(output, want) {
			t.Errorf("--api-auth wiring is missing %q:\n%s", want, output)
		}
	}

	// An application that already mounts them (from --api-auth, or from
	// before the flag existed).
	mounted := generate(t, true)
	output, err = runAuthentication(mounted, "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "NOTE: app/bootstrap/bootstrap.go also mounts JSON authentication routes (POST /auth/login, POST /auth/logout, POST /auth/mfa/complete, POST /auth/recovery/request, POST /auth/recovery/reset, POST /auth/cookie/login, POST /auth/cookie/mfa/complete, POST /auth/cookie/logout, GET /v1/auth/session)") {
		t.Errorf("the mounted JSON routes are not pointed out:\n%s", output)
	}
	output, err = runAuthentication(mounted, "--dry-run", "--api-auth")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "bootstrap.go already mounts them; keep them") || strings.Contains(output, "auth.NewLoginHandler(login)") {
		t.Errorf("--api-auth with the routes mounted:\n%s", output)
	}
	for _, args := range [][]string{{"--api-auth", "--api-auth"}, {"--dry-run", "--dry-run"}, {"--api-auth=true"}} {
		if _, err := runAuthentication(mounted, args...); err == nil || !strings.Contains(err.Error(), authenticationUsage) {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}

func TestScaffoldRefusesThePathsOfTheSignInPages(t *testing.T) {
	t.Parallel()
	for _, plural := range []string{"session", "sessions", "passwords"} {
		root := resourceProject(t, "postgres://u:p@localhost/db")
		var out bytes.Buffer
		err := runGenerateEnv([]string{"scaffold", "thing", "name:string", "--global", "--plural", plural}, &out, root, nil, time.Now())
		if err == nil || !strings.Contains(err.Error(), "already uses") {
			t.Fatalf("plural %q: err = %v", plural, err)
		}
	}
}
