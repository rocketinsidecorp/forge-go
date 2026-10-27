package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runArchitectureTest runs the generated application's architecture test and
// returns its output and whether it passed.
func runArchitectureTest(t *testing.T, dir string) (string, bool) {
	t.Helper()
	command := exec.Command("go", "test", "-count=1", "-run", "^TestArchitecture", "./app/")
	command.Dir = dir
	command.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
	output, err := command.CombinedOutput()
	return string(output), err == nil
}

// TestGeneratedArchitectureTestPassesAndCatchesViolations generates an
// application, adds every kind of generated code to it, requires app/architecture_test.go
// to pass, then injects one forbidden import per ring rule and requires the
// test to fail naming the file, the line and the import. The module path
// without a dot ("mx") is the case in which a module import looks like a
// standard library one.
func TestGeneratedArchitectureTestPassesAndCatchesViolations(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	// An empty module leaves `forge new` its default, the application name.
	for _, c := range []struct{ name, newModule, module string }{
		{"dotted module", "example.com/mx", "example.com/mx"},
		{"dotless module", "", "mx"},
	} {
		module := c.module
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := matrixApplicationModule(t, c.newModule, "sqlite", nil, "scaffold", [][]string{{"product", "name:string", "--global"}})
			var env []string
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "FORGE_DATABASE_URL=") {
					env = append(env, entry)
				}
			}
			now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
			for _, args := range [][]string{
				{"model", "note", "title:string", "--tenant-scoped"},
				{"controller", "pages", "home", "--public"},
				{"mailer", "user", "welcome:name"},
				{"job", "send_welcome", "user_id:uuid", "--queue"},
				{"job", "cleanup"},
			} {
				var output bytes.Buffer
				now = now.Add(time.Minute)
				if err := runGenerateEnv(args, &output, dir, env, now); err != nil {
					t.Fatalf("generate %v: %v", args, err)
				}
			}
			if output, ok := runArchitectureTest(t, dir); !ok {
				t.Fatalf("the architecture test fails on a freshly generated application:\n%s", output)
			}
			// A comment after the module path, and a quoted path, are valid go.mod.
			goMod := filepath.Join(dir, "go.mod")
			original := readFile(t, goMod)
			for _, line := range []string{"module " + module + " // the application", "module \"" + module + "\""} {
				writeFile(t, goMod, strings.Replace(original, "module "+module, line, 1))
				if output, ok := runArchitectureTest(t, dir); !ok {
					t.Fatalf("the architecture test fails with %q in go.mod:\n%s", line, output)
				}
			}
			writeFile(t, goMod, original)

			for _, violation := range []struct{ file, imports, want string }{
				{"app/domain/leak.go", "net/http", "the domain ring must not do I/O or speak HTTP"},
				{"app/domain/leak.go", "database/sql", "the domain ring must not do I/O or speak HTTP"},
				{"app/domain/leak.go", "io/fs", "the domain ring must not do I/O or speak HTTP"},
				{"app/domain/leak.go", "io/ioutil", "the domain ring must not do I/O or speak HTTP"},
				{"app/domain/leak.go", "path/filepath", "the domain ring must not do I/O or speak HTTP"},
				{"app/domain/leak.go", "unsafe", "the domain ring must not do I/O or speak HTTP"},
				{"app/domain/leak.go", "C", "the domain ring must not do I/O or speak HTTP"},
				{"app/domain/leak_test.go", "os", "the domain ring must not do I/O or speak HTTP"},
				{"app/application/leak.go", "path/filepath", "the application ring must not do I/O or speak HTTP"},
				{"app/domain/leak.go", "github.com/rocketinsidecorp/forge-go/web", "the domain ring may import only the framework's uuid, fault packages"},
				{"app/domain/leak.go", module + "/app/application", "the domain ring may import only app/domain of this module"},
				{"app/application/leak.go", module + "/app/adapters/persistence", "the application ring may import only app/domain, app/application of this module"},
				{"app/application/leak.go", "github.com/rocketinsidecorp/forge-go/sqldb", "the application ring may import only the framework's"},
				{"app/adapters/httpapi/leak.go", module + "/app/bootstrap", "the adapters ring must not import app/bootstrap: only app/bootstrap composes the application"},
				{"app/jobs/leak.go", module + "/app/bootstrap", "the support package ring must not import app/bootstrap"},
				{"db/leak.go", module + "/app/bootstrap", "the support package ring must not import app/bootstrap"},
			} {
				file := filepath.Join(dir, filepath.FromSlash(violation.file))
				writeFile(t, file, "package "+filepath.Base(filepath.Dir(file))+"\n\nimport _ \""+violation.imports+"\"\n")
				output, ok := runArchitectureTest(t, dir)
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if ok {
					t.Errorf("%s importing %s: the architecture test passed", violation.file, violation.imports)
					continue
				}
				if want := violation.file + `:3: imports "` + violation.imports + `": ` + violation.want; !strings.Contains(output, want) {
					t.Errorf("%s importing %s: the failure does not say %q:\n%s", violation.file, violation.imports, want, output)
				}
			}
			if output, ok := runArchitectureTest(t, dir); !ok {
				t.Fatalf("the architecture test fails after the injected files were removed:\n%s", output)
			}
		})
	}
}
