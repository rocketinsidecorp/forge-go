package scaffold_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

// upgradeRewrite is the first shell block of the "Upgrading" section of
// docs/AUTHENTICATION.md: the rewrite of an application created before the
// HTTP side of auth moved to auth/authhttp (ADR 0052).
func upgradeRewrite(t *testing.T) string {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "AUTHENTICATION.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(doc), "\n## Upgrading\n")
	if !found {
		t.Fatal("docs/AUTHENTICATION.md has no Upgrading section")
	}
	_, block, found := strings.Cut(section, "\n```sh\n")
	if !found {
		t.Fatal("the Upgrading section has no sh block")
	}
	script, _, found := strings.Cut(block, "\n```\n")
	if !found {
		t.Fatal("the Upgrading sh block is not closed")
	}
	return script + "\n"
}

// oldProtect is the protect guard as forge wrote it before the move.
const oldProtect = `func protect(repository auth.SecurityAuditor, middleware *auth.Middleware) func(string, http.Handler) (http.Handler, error) {
	return func(permission string, next http.Handler) (http.Handler, error) {
		required, err := auth.NewPermission(permission)
		if err != nil {
			return nil, err
		}
		guarded, err := auth.RequireAudited(required, next, repository)
		if err != nil {
			return nil, err
		}
		return middleware.Authenticate(guarded), nil
	}
}
`

const oldIdentity = `package bootstrap

import (
	"github.com/rocketinsidecorp/forge-go/auth"
)

func identity(resolver auth.Resolver) (*auth.Middleware, *auth.Browser, error) {
	middleware, err := auth.NewMiddleware(resolver)
	return middleware, nil, err
}
`

// The documented rewrite turns an application of the previous release into
// one of this release, refuses a seam edited by hand without touching
// anything, does nothing the second time, and works without a seam.
func TestUpgradeRewriteOfTheAuthenticationDocs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the documented commands are GNU sed, grep -P and sha256sum")
	}
	for _, tool := range []string{"bash", "perl", "grep", "sed", "sha256sum", "cut"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	t.Parallel()
	script := upgradeRewrite(t)
	seam, err := scaffold.RenderRoutes(scaffold.RoutesApp{Module: "example.com/shop", Adapter: "sqlite", Identity: true},
		[]scaffold.RouteBlock{scaffold.IdentityBlock, {Kind: "guard", Name: "protect", Body: oldProtect}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scaffold.ParseRoutes(seam); err != nil {
		t.Fatalf("the old seam does not parse: %v", err)
	}
	app := func(t *testing.T, routes []byte) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "app", "bootstrap"), 0o755); err != nil {
			t.Fatal(err)
		}
		if routes != nil {
			if err := os.WriteFile(filepath.Join(dir, scaffold.RoutesFile), routes, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "app", "bootstrap", "identity.go"), []byte(oldIdentity), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	run := func(t *testing.T, dir string) (string, error) {
		t.Helper()
		command := exec.Command("bash", "-c", script)
		command.Dir = dir
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		err := command.Run()
		return output.String(), err
	}
	read := func(t *testing.T, path string) string {
		t.Helper()
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}
	const newImport = `"github.com/rocketinsidecorp/forge-go/auth/authhttp"`

	t.Run("an application of the previous release", func(t *testing.T) {
		t.Parallel()
		dir := app(t, seam)
		if output, err := run(t, dir); err != nil || !strings.Contains(output, "rewrote:") {
			t.Fatalf("rewrite: %v\n%s", err, output)
		}
		identity := read(t, filepath.Join(dir, "app", "bootstrap", "identity.go"))
		routes := read(t, filepath.Join(dir, scaffold.RoutesFile))
		for name, content := range map[string]string{"identity.go": identity, "routes_gen.go": routes} {
			if strings.Contains(content, "auth.Middleware") || !strings.Contains(content, "*authhttp.Middleware") || !strings.Contains(content, newImport) {
				t.Errorf("%s not rewritten:\n%s", name, content)
			}
		}
		if !strings.Contains(identity, "authhttp.NewMiddleware(resolver)") || !strings.Contains(identity, "*authhttp.Browser") || !strings.Contains(identity, "auth.Resolver") {
			t.Errorf("identity.go:\n%s", identity)
		}
		if !strings.Contains(routes, "authhttp.RequireAudited(") || !strings.Contains(routes, "auth.NewPermission(") {
			t.Errorf("routes_gen.go:\n%s", routes)
		}
		if _, err := scaffold.ParseRoutes([]byte(routes)); err != nil {
			t.Fatalf("the generators refuse the rewritten seam: %v", err)
		}
		// A second run finds nothing and changes nothing.
		if output, err := run(t, dir); err != nil || strings.TrimSpace(output) != "nothing to rewrite" {
			t.Fatalf("second run: %v\n%s", err, output)
		}
		if read(t, filepath.Join(dir, scaffold.RoutesFile)) != routes {
			t.Error("the second run changed the seam")
		}
	})

	t.Run("a seam edited by hand", func(t *testing.T) {
		t.Parallel()
		edited := bytes.Replace(seam, []byte("func protect("), []byte("// my change\nfunc protect("), 1)
		dir := app(t, edited)
		output, err := run(t, dir)
		if err == nil || !strings.Contains(output, "was edited by hand") {
			t.Fatalf("an edited seam was accepted: %v\n%s", err, output)
		}
		if read(t, filepath.Join(dir, scaffold.RoutesFile)) != string(edited) || read(t, filepath.Join(dir, "app", "bootstrap", "identity.go")) != oldIdentity {
			t.Error("a refused rewrite changed files")
		}
	})

	t.Run("an application without the seam", func(t *testing.T) {
		t.Parallel()
		dir := app(t, nil)
		if output, err := run(t, dir); err != nil || strings.Contains(output, "No such file") {
			t.Fatalf("rewrite: %v\n%s", err, output)
		}
		if identity := read(t, filepath.Join(dir, "app", "bootstrap", "identity.go")); !strings.Contains(identity, "authhttp.NewMiddleware") {
			t.Errorf("identity.go not rewritten:\n%s", identity)
		}
		if _, err := os.Stat(filepath.Join(dir, scaffold.RoutesFile)); !os.IsNotExist(err) {
			t.Errorf("a seam appeared: %v", err)
		}
	})
}
