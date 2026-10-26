package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGeneratedHTMLApplicationBuildsVetsLintsAndTests generates applications
// without a database and with SQLite (which needs no server), then compiles,
// vets, lints (with this repository's golangci-lint configuration) and tests
// them. The generated tests render the application layout and prove that the
// wired middleware rejects a form POST without the CSRF token and accepts it
// with the token.
func TestGeneratedHTMLApplicationBuildsVetsLintsAndTests(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	for _, database := range []string{"none", "sqlite"} {
		t.Run(database, func(t *testing.T) {
			t.Parallel()
			dir := matrixApplication(t, database, nil)
			goInMatrix(t, dir, "vet", "./...")
			goInMatrix(t, dir, "build", "-o", os.DevNull, "./cmd/mx")
			command := exec.Command("go", "test", "-count=1", "-v", "./app/...")
			command.Dir = dir
			command.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("go test: %v\n%s", err, output)
			}
			for _, want := range []string{"--- PASS: TestBrowserPipelineRequiresCSRFToken", "--- PASS: TestLayoutRendersFlashAndCSRF", "--- PASS: TestNewParsesTheEmbeddedTemplates"} {
				if !strings.Contains(string(output), want) {
					t.Fatalf("generated test did not pass: missing %q\n%s", want, output)
				}
			}
			lint, err := exec.LookPath("golangci-lint")
			if err != nil {
				t.Log("golangci-lint is not on PATH; the lint check was skipped")
				return
			}
			golangciLint.Lock()
			defer golangciLint.Unlock()
			command = exec.Command(lint, "run", "./...")
			command.Dir = dir
			command.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("golangci-lint: %v\n%s", err, output)
			}
		})
	}
}

// TestGeneratedAPIAuthApplicationBuildsAndPasses generates a PostgreSQL
// application with --api-auth, which mounts the JSON authentication APIs
// beside the HTML pages, then vets, builds, lints and tests it. With a
// PostgreSQL test server its bootstrap test checks that every JSON route is
// routed and documented; the default application's (in
// TestGeneratedScaffoldBuildsWiresAndPasses) checks that none is.
func TestGeneratedAPIAuthApplicationBuildsAndPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	dir := matrixApplicationWith(t, "postgresql", []string{"--api-auth"}, "resource", nil)
	bootstrap := readFile(t, filepath.Join(dir, "app", "bootstrap", "bootstrap.go"))
	for _, want := range []string{"func apiAuthRoutes(", `"POST /auth/login"`, `"POST /auth/cookie/login"`, `"GET /v1/auth/session"`, "apiAuthRoutes(app, login, mfaService, recoveryService, repository, middleware)"} {
		if !strings.Contains(bootstrap, want) {
			t.Fatalf("bootstrap.go with --api-auth lacks %q", want)
		}
	}
	goInMatrix(t, dir, "vet", "./...")
	goInMatrix(t, dir, "build", "-o", os.DevNull, "./cmd/mx")
	packages := []string{"./app/views/...", "./config/..."}
	if databaseAvailable("postgresql") {
		packages = []string{"./..."}
	} else {
		t.Log("no PostgreSQL test server is configured; the bootstrap and sign-in tests were skipped")
	}
	command := exec.Command("go", append([]string{"test", "-count=1", "-v"}, packages...)...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go test: %v\n%s", err, output)
	}
	if databaseAvailable("postgresql") {
		for _, want := range []string{"--- PASS: TestConfigureConnectsToDatabase", "--- PASS: TestPagesAgainstPostgres"} {
			if !strings.Contains(string(output), want) {
				t.Fatalf("generated test did not pass: missing %q\n%s", want, output)
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
	runGo(t, dir, lint, "run", "./...")
}
