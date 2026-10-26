package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// uncommentJobExample turns the commented scheduler.Register example in the
// generated JobsConfigure into code, so a change to the scheduler or the
// template cannot leave documentation that no longer compiles.
func uncommentJobExample(t *testing.T, source string) string {
	t.Helper()
	lines := strings.Split(source, "\n")
	start, end := -1, -1
	for i, line := range lines {
		switch {
		case start < 0 && strings.HasPrefix(line, "\t// if err := scheduler.Register("):
			start = i
		case start >= 0 && line == "\t// }":
			end = i
		}
		if end >= 0 {
			break
		}
	}
	if start < 0 || end < 0 {
		t.Fatalf("the JobsConfigure example is missing from bootstrap.go:\n%s", source)
	}
	for i := start; i <= end; i++ {
		lines[i] = "\t" + strings.TrimPrefix(strings.TrimPrefix(lines[i], "\t//"), " ")
	}
	return strings.Join(lines, "\n")
}

// TestGeneratedJobsConfigureExampleCompiles generates an application whose
// bootstrap carries JobsConfigure, requires its doc comment, then enables the
// commented scheduler.Register example and requires the package to vet.
func TestGeneratedJobsConfigureExampleCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	for _, database := range []string{"postgresql", "mysql"} {
		t.Run(database, func(t *testing.T) {
			t.Parallel()
			dir := matrixApplicationFor(t, database, "job", [][]string{{"cleanup"}})
			path := filepath.Join(dir, "app", "bootstrap", "bootstrap.go")
			source := readFile(t, path)
			if !strings.Contains(source, "}\n\n// JobsConfigure starts the in-process scheduler") {
				t.Errorf("JobsConfigure has no doc comment separated from Configure:\n%s", source)
			}
			if strings.Contains(source, "app.InTx") {
				t.Error("the example calls app.InTx, which forge.App does not have")
			}
			if !strings.Contains(source, "app.OnShutdown(scheduler.StopContext)") {
				t.Error("the shutdown hook does not use StopContext")
			}
			runGo(t, dir, "go", "vet", "./app/bootstrap/")

			enabled := uncommentJobExample(t, source)
			enabled = strings.Replace(enabled, "\t\"net/http\"\n", "\t\"net/http\"\n\t\"time\"\n", 1)
			if err := os.WriteFile(path, []byte(enabled), 0o600); err != nil {
				t.Fatal(err)
			}
			runGo(t, dir, "go", "vet", "./app/bootstrap/")
		})
	}
}
