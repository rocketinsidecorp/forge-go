package cli

import (
	"bytes"
	"errors"
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

// newApp generates the application `forge new shop` would, in a fresh
// directory, without resolving dependencies.
func newApp(t *testing.T, opts scaffold.Options) string {
	t.Helper()
	opts.Name, opts.Module = "shop", "example.com/shop"
	opts.Dir = filepath.Join(t.TempDir(), "shop")
	if _, err := scaffold.Generate(opts); err != nil {
		t.Fatal(err)
	}
	return opts.Dir
}

// TestGenerateAuthenticationAddsWhatForgeNewAuthGenerates is decision D31:
// `forge generate authentication` in an application created without --auth
// ends with exactly what `forge new --auth` generates (the identity files,
// app_gen.go and the routes seam), on every database, with nothing to edit.
// A second run refuses and changes nothing.
func TestGenerateAuthenticationAddsWhatForgeNewAuthGenerates(t *testing.T) {
	t.Parallel()
	for name, options := range map[string]scaffold.Options{
		"sqlite": {Database: "sqlite"}, "mysql": {Database: "mysql"}, "mariadb": {Database: "mariadb"},
		"postgresql": {Database: "postgresql"}, "sqlite --js": {Database: "sqlite", JS: true},
	} {
		database := options.Database
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plain := newApp(t, options)
			withAuth := options
			withAuth.Auth = true
			want := newApp(t, withAuth)
			output, err := runAuthentication(plain)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output, "WARNING") || strings.Contains(output, "Forge did not edit") {
				t.Errorf("a forge new application needs no manual step:\n%s", output)
			}
			files, err := scaffold.AuthenticationFiles(scaffold.Options{Name: "shop", Module: "example.com/shop", Database: database})
			if err != nil {
				t.Fatal(err)
			}
			// AGENTS.md is refreshed too: it now describes sign-in.
			if !strings.Contains(output, "rewrite  "+scaffold.AgentsFile) {
				t.Errorf("output does not list %s:\n%s", scaffold.AgentsFile, output)
			}
			paths := []string{scaffold.AppFile, scaffold.SetsFile, scaffold.RoutesFile, scaffold.AgentsFile}
			for _, file := range files {
				paths = append(paths, file.Path)
				if !strings.Contains(output, "create  "+file.Path) {
					t.Errorf("output does not list %s:\n%s", file.Path, output)
				}
			}
			for _, path := range paths {
				if got, wanted := readFile(t, filepath.Join(plain, path)), readFile(t, filepath.Join(want, path)); got != wanted {
					t.Errorf("%s differs from forge new --auth", path)
				}
			}
			if !strings.Contains(readFile(t, filepath.Join(plain, ".env.local")), "\nFORGE_AUTH_MFA_KEY=") {
				t.Error(".env.local has no MFA key")
			}
			before := snapshotTree(t, plain)
			if _, err := runAuthentication(plain); err == nil || !strings.Contains(err.Error(), "has Forge identity already") {
				t.Fatalf("second run: err = %v", err)
			}
			if after := snapshotTree(t, plain); !slices.Equal(before, after) {
				t.Fatal("a refused run changed files")
			}
		})
	}
}

// An AGENTS.md edited by hand is left as it is, with a note.
func TestGenerateAuthenticationLeavesAnEditedAgentsGuide(t *testing.T) {
	t.Parallel()
	dir := newApp(t, scaffold.Options{Database: "sqlite"})
	path := filepath.Join(dir, scaffold.AgentsFile)
	edited := readFile(t, path) + "\nOur own rule.\n"
	writeFile(t, path, edited)
	output, err := runAuthentication(dir)
	if err != nil {
		t.Fatal(err)
	}
	if readFile(t, path) != edited || !strings.Contains(output, "AGENTS.md was edited by hand, so it was left as it is") || strings.Contains(output, "rewrite  AGENTS.md") {
		t.Fatalf("an edited AGENTS.md:\n%s", output)
	}
}

// TestGenerateAuthenticationKeepsTheRoutesAlreadyGenerated: the seam gains
// the identity block first and keeps the routes generated before.
func TestGenerateAuthenticationKeepsTheRoutesAlreadyGenerated(t *testing.T) {
	t.Parallel()
	dir := newApp(t, scaffold.Options{Database: "sqlite"})
	var out bytes.Buffer
	if err := runGenerateEnv([]string{"scaffold", "note", "body:text", "--public"}, &out, dir, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := runAuthentication(dir); err != nil {
		t.Fatal(err)
	}
	blocks, err := scaffold.ParseRoutes([]byte(readFile(t, filepath.Join(dir, scaffold.RoutesFile))))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, block := range blocks {
		ids = append(ids, block.ID())
	}
	if len(ids) < 3 || ids[0] != "identity repository" || !slices.Contains(ids, "scaffold notes") || !slices.Contains(ids, "guard public") {
		t.Fatalf("seam blocks = %v", ids)
	}
	// Routes generated from now on are guarded by the session.
	out.Reset()
	if err := runGenerateEnv([]string{"scaffold", "product", "name:string", "price:integer"}, &out, dir, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if routes := readFile(t, filepath.Join(dir, scaffold.RoutesFile)); !strings.Contains(routes, "protectCookie(repository, middleware)") {
		t.Fatalf("the new scaffold is not guarded by the session:\n%s", routes)
	}
}

func TestGenerateAuthenticationDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	dir := newApp(t, scaffold.Options{Database: "sqlite"})
	before := snapshotTree(t, dir)
	output, err := runAuthentication(dir, "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if after := snapshotTree(t, dir); !slices.Equal(before, after) {
		t.Fatal("--dry-run wrote files")
	}
	for _, want := range []string{
		"would create  " + filepath.Join("app", "bootstrap", "identity.go"),
		"would create  " + filepath.Join("app", "adapters", "htmlauth", "htmlauth.go"),
		"would create  " + filepath.Join("app", "views", "sessions", "new.gohtml"),
		"would rewrite  " + scaffold.AppFile,
		"would wire  " + scaffold.RoutesFile,
		"would append  .env.local (FORGE_AUTH_MFA_KEY)",
		"forge migrate && forge setup --email you@example.com",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("dry-run output is missing %q:\n%s", want, output)
		}
	}
}

func TestGenerateAuthenticationRefusesWhatItCannotWire(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		setup func(t *testing.T) string
		want  string
	}{
		"no such directory": {func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") }, "go.mod not found"},
		"no go.mod":         {func(t *testing.T) string { return t.TempDir() }, "go.mod not found"},
		"go.mod without a module": {func(t *testing.T) string {
			dir := newApp(t, scaffold.Options{Database: "sqlite"})
			writeFile(t, filepath.Join(dir, "go.mod"), "go 1.27.1\n")
			return dir
		}, "go.mod has no module declaration"},
		"broken environment file": {func(t *testing.T) string {
			dir := newApp(t, scaffold.Options{Database: "sqlite"})
			writeFile(t, filepath.Join(dir, ".env.development"), "bad line\n")
			return dir
		}, "environment files could not be loaded"},
		"no database":                           {func(t *testing.T) string { return newApp(t, scaffold.Options{}) }, "needs a database"},
		"an API-only app":                       {func(t *testing.T) string { return newApp(t, scaffold.Options{Database: "sqlite", API: true}) }, "no views"},
		"an application from before app_gen.go": {func(t *testing.T) string { return plainProject(t, "sqlite:storage/development.db") }, "app_gen.go not found"},
		"an edited app_gen.go": {func(t *testing.T) string {
			dir := newApp(t, scaffold.Options{Database: "sqlite"})
			path := filepath.Join(dir, scaffold.AppFile)
			writeFile(t, path, readFile(t, path)+"\n// mine\n")
			return dir
		}, "edited by hand"},
		"a main.go without bootstrap.Commands": {func(t *testing.T) string {
			dir := newApp(t, scaffold.Options{Database: "sqlite"})
			writeFile(t, filepath.Join(dir, "cmd", "shop", "main.go"), "package main\n\nfunc main() {}\n")
			return dir
		}, "bootstrap.Commands()"},
		"a bootstrap.go without the seam": {func(t *testing.T) string {
			dir := newApp(t, scaffold.Options{Database: "sqlite"})
			path := filepath.Join(dir, "app", "bootstrap", "bootstrap.go")
			writeFile(t, path, strings.ReplaceAll(readFile(t, path), "generatedRoutes(", "otherRoutes("))
			return dir
		}, "does not call generatedRoutes"},
		"a file in the way": {func(t *testing.T) string {
			dir := newApp(t, scaffold.Options{Database: "sqlite"})
			writeFile(t, filepath.Join(dir, "app", "views", "sessions", "new.gohtml"), "mine")
			return dir
		}, "refusing to overwrite"},
		"a pages directory that is a symlink": {func(t *testing.T) string {
			dir := newApp(t, scaffold.Options{Database: "sqlite"})
			if err := os.MkdirAll(filepath.Join(dir, "app", "adapters"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(dir, "app", "adapters", "htmlauth")); err != nil {
				t.Skipf("symlinks are not available: %v", err)
			}
			return dir
		}, "symlink"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := tc.setup(t)
			var before []string
			if info, err := os.Stat(root); err == nil && info.IsDir() {
				before = snapshotTree(t, root)
			}
			_, err := runAuthentication(root)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if before != nil && !slices.Equal(before, snapshotTree(t, root)) {
				t.Error("a refused run wrote files")
			}
		})
	}
	for _, args := range [][]string{{"--api-auth", "--api-auth"}, {"--dry-run", "--dry-run"}, {"--api-auth=true"}, {"--force"}} {
		if _, err := runAuthentication(t.TempDir(), args...); err == nil || !strings.Contains(err.Error(), authenticationUsage) {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}

// TestGenerateAuthenticationAPIAuthOnAnApplicationWithIdentity: an
// application with Forge identity gets the JSON API wiring printed for its
// identity.go, or is told to keep the routes it mounts.
func TestGenerateAuthenticationAPIAuthOnAnApplicationWithIdentity(t *testing.T) {
	t.Parallel()
	dir := newApp(t, scaffold.Options{Database: "sqlite", Auth: true})
	before := snapshotTree(t, dir)
	output, err := runAuthentication(dir, "--api-auth")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"nothing was written", "Mount the JSON authentication APIs there", "auth.NewLoginHandler(login)", `app.Handle("POST /auth/cookie/login", cookieLoginHandler)`} {
		if !strings.Contains(output, want) {
			t.Errorf("output is missing %q:\n%s", want, output)
		}
	}
	if after := snapshotTree(t, dir); !slices.Equal(before, after) {
		t.Fatal("files changed")
	}
	mounted := newApp(t, scaffold.Options{Database: "sqlite", APIAuth: true})
	output, err = runAuthentication(mounted, "--api-auth")
	if err != nil || !strings.Contains(output, "already mounts them; keep them") || strings.Contains(output, "auth.NewLoginHandler(login)") {
		t.Fatalf("--api-auth with the routes mounted: err=%v\n%s", err, output)
	}
	// Some routes mounted: only the others are asked for.
	identity := filepath.Join(mounted, "app", "bootstrap", "identity.go")
	writeFile(t, identity, strings.ReplaceAll(readFile(t, identity), `"POST /auth/cookie/logout"`, `"POST /auth/cookie/signout"`))
	output, err = runAuthentication(mounted, "--api-auth")
	if err != nil || !strings.Contains(output, "add only the others below") {
		t.Fatalf("partly mounted: err=%v\n%s", err, output)
	}
}

// TestGenerateAuthenticationFollowsTheApplicationsLocales: the page text in
// the other locales the application has and Forge ships.
func TestGenerateAuthenticationFollowsTheApplicationsLocales(t *testing.T) {
	t.Parallel()
	dir := newApp(t, scaffold.Options{Database: "sqlite", Locale: "es"})
	writeFile(t, filepath.Join(dir, "config", "locales", "fr.json"), `{"fr":{}}`)
	output, err := runAuthentication(dir, "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, filepath.Join("config", "locales", "authentication.es.json")) {
		t.Errorf("the Spanish page text is not planned:\n%s", output)
	}
	if strings.Contains(output, "authentication.fr.json") || strings.Contains(output, "authentication.pt-BR.json") {
		t.Errorf("a locale the application lacks (or Forge does not ship) is planned:\n%s", output)
	}
}

func TestGenerateAuthenticationReportsAFailingOutput(t *testing.T) {
	t.Parallel()
	dir := newApp(t, scaffold.Options{Database: "sqlite"})
	if err := runGenerateAuthentication([]string{"--dry-run"}, failingWriter{}, dir, nil); !errors.Is(err, errBrokenPipe) {
		t.Errorf("err = %v", err)
	}
	if err := runGenerateAuthentication([]string{"--api-auth"}, failingWriter{}, newApp(t, scaffold.Options{Database: "sqlite", Auth: true}), nil); !errors.Is(err, errBrokenPipe) {
		t.Errorf("err = %v", err)
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

const postgresURL = "postgres://u:p@localhost/db"
