package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// golangciLint refuses to run twice at once in the same cache.
var golangciLint sync.Mutex

// matrixResources is every edge case the generator must turn into code that
// compiles, vets and lints: each field type alone (so no import is shared by
// accident), required and optional, tenant-scoped and global, plus names that
// are reserved words in SQL.
func matrixResources() [][]string {
	var resources [][]string
	for _, kind := range resourceFieldTypes {
		for _, optional := range []bool{false, true} {
			for _, scope := range []string{"tenant", "global"} {
				declaration := "value:" + kind
				name := "m_" + kind + "_req_" + scope
				if optional {
					declaration += "?"
					name = "m_" + kind + "_opt_" + scope
				}
				flag := "--global"
				if scope == "tenant" {
					flag = "--tenant-scoped"
				}
				resources = append(resources, []string{name, declaration, flag})
			}
		}
	}
	return append(append(resources, reservedWordResources()...),
		// Optional-only text was the field set that once imported an unused package.
		[]string{"memo", "body:text?", "--global"},
		// Field names that look like Go identifiers used by the generated code.
		[]string{"edge_go", "err:string", "ctx:int", "t:bool", "entity:time?", "input:uuid", "repository:text", "--tenant-scoped"},
		[]string{"all_optional", "a:string?", "b:text?", "c:int?", "d:bool?", "e:time?", "f:uuid?", "--tenant-scoped"},
	)
}

// reservedWordResources use column and table names that are reserved words
// in PostgreSQL, MySQL, MariaDB or SQLite.
func reservedWordResources() [][]string {
	return [][]string{
		{
			"reserved_word", "change:int", "collation:string", "freeze:bool", "char:string", "only:int", "order:time", "group:uuid?",
			"select:text", "user:int?", "--tenant-scoped",
		},
		{"group", "name:string", "--global"},
		{"order", "name:string", "--tenant-scoped"},
		{"row", "value:int", "--global"},
	}
}

// matrixApplication scaffolds an application without resolving dependencies,
// points it at this checkout through the module cache (so nothing is vendored
// or recompiled), and generates every matrix resource into it.
func matrixApplication(t *testing.T, database string, resources [][]string) string {
	t.Helper()
	return matrixApplicationFor(t, database, "resource", resources)
}

// matrixApplicationFor is matrixApplication for any generator that takes a
// resource command line: "resource" or "scaffold".
func matrixApplicationFor(t *testing.T, database, command string, resources [][]string) string {
	t.Helper()
	return matrixApplicationWith(t, database, nil, command, resources)
}

// matrixApplicationWith is matrixApplicationFor with extra `forge new` flags.
func matrixApplicationWith(t *testing.T, database string, newFlags []string, command string, resources [][]string) string {
	t.Helper()
	return matrixApplicationModule(t, "example.com/mx", database, newFlags, command, resources)
}

// matrixApplicationModule is matrixApplicationWith for a module path. An empty
// module leaves forge new to its default, the application name ("mx"): a
// module path without a dot.
func matrixApplicationModule(t *testing.T, module, database string, newFlags []string, command string, resources [][]string) string {
	t.Helper()
	moduleFlags := []string{"--module", module}
	if module == "" {
		module, moduleFlags = "mx", nil
	}
	frameworkPath, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "mx")
	var stdout, stderr bytes.Buffer
	args := append(append([]string{"new", "mx"}, moduleFlags...), "--dir", dir, "--database", database, "--skip-deps")
	args = append(args, newFlags...)
	if code := Run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("forge new: code=%d %s %s", code, stdout.String(), stderr.String())
	}
	// Same requirements as the framework, which already resolve offline from
	// the module cache.
	frameworkMod := readFile(t, filepath.Join(frameworkPath, "go.mod"))
	_, requirements, _ := strings.Cut(frameworkMod, "go 1.27.3")
	requirements = strings.Replace(requirements, "require (\n", "require (\n\t"+"github.com/rocketinsidecorp/forge-go v0.0.0\n", 1)
	writeFile(t, filepath.Join(dir, "go.mod"), "module "+module+"\n\ngo 1.27.3\n"+requirements+"\nreplace github.com/rocketinsidecorp/forge-go => "+frameworkPath+"\n")
	writeFile(t, filepath.Join(dir, "go.sum"), readFile(t, filepath.Join(frameworkPath, "go.sum")))
	writeFile(t, filepath.Join(dir, ".golangci.yml"), readFile(t, filepath.Join(frameworkPath, ".golangci.yml")))

	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "FORGE_DATABASE_URL=") {
			env = append(env, entry)
		}
	}
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for _, resource := range resources {
		var output bytes.Buffer
		if err := runGenerateEnv(append([]string{command}, resource...), &output, dir, env, now); err != nil {
			t.Fatalf("generate %v: %v", resource, err)
		}
	}
	return dir
}

// databaseAvailable reports whether the generated integration tests for
// database can reach a server (SQLite needs none).
func databaseAvailable(database string) bool {
	switch database {
	case "postgresql":
		return os.Getenv("FORGE_TEST_POSTGRES_URL") != "" || strings.HasPrefix(os.Getenv("FORGE_TEST_DATABASE_URL"), "postgres")
	case "mysql", "mariadb":
		return os.Getenv("FORGE_TEST_MYSQL_URL") != "" || strings.HasPrefix(os.Getenv("FORGE_TEST_DATABASE_URL"), "mysql")
	default:
		return true
	}
}

func goInMatrix(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("go", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, output)
	}
}

// TestGeneratedMatrixCompilesAndLints type-checks (go vet compiles the
// test files too) every edge-case resource for every database, and lints the
// generated packages with the repository's golangci-lint configuration, so
// code that does not compile or that this repository's linters reject can
// never ship from a template.
func TestGeneratedMatrixCompilesAndLints(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	for _, database := range []string{"postgresql", "mysql", "sqlite"} {
		t.Run(database, func(t *testing.T) {
			t.Parallel()
			dir := matrixApplication(t, database, matrixResources())
			goInMatrix(t, dir, "vet", "./app/...", "./db/...")
			// Names that are reserved words must also work against a real
			// server: migrate, insert, select, update, delete.
			if databaseAvailable(database) {
				// A small application keeps the per-test migration cost low.
				small := matrixApplication(t, database, reservedWordResources())
				goInMatrix(t, small, "test", "-count=1", "./app/...")
			} else {
				t.Logf("no %s test server is configured; the reserved-word run was skipped", database)
			}
			lint, err := exec.LookPath("golangci-lint")
			if err != nil {
				t.Log("golangci-lint is not on PATH; the lint check was skipped")
				return
			}
			golangciLint.Lock()
			defer golangciLint.Unlock()
			command := exec.Command(lint, "run", "./app/domain/...", "./app/application/...", "./app/adapters/...")
			command.Dir = dir
			command.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("golangci-lint: %v\n%s", err, output)
			}
		})
	}
}
