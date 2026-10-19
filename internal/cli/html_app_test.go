package cli

import (
	"os"
	"os/exec"
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
