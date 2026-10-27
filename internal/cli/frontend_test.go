package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/devloop"
	"github.com/rocketinsidecorp/forge-go/internal/frontend"
)

// typeScriptProject is an application root with a TypeScript front end (the
// entry point and one controller with type annotations) and, optionally, the
// tsconfig.json tsc needs.
func typeScriptProject(t *testing.T, tsconfig bool) string {
	t.Helper()
	root := qualityProject(t)
	files := map[string]string{
		"app/javascript/application.ts":                  "import \"controllers\"\n",
		"app/javascript/controllers/hello_controller.ts": "export default class { greet(name: string): string { return name } }\n",
	}
	if tsconfig {
		files["tsconfig.json"] = "{}\n"
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCompileFrontendReportsOnlyWhatChanged(t *testing.T) {
	t.Parallel()
	root := typeScriptProject(t, false)
	var out strings.Builder
	if err := compileFrontend(root, &out); err != nil {
		t.Fatal(err)
	}
	if want := "  compile app/javascript/*.ts -> app/javascript/dist (2 file(s) updated, esbuild " + frontend.EsbuildVersion + ")\n"; out.String() != want {
		t.Errorf("first run printed %q, want %q", out.String(), want)
	}
	if got := readFile(t, filepath.Join(root, "app", "javascript", "dist", "controllers", "hello_controller.js")); strings.Contains(got, "name: string") {
		t.Errorf("the types were not stripped:\n%s", got)
	}
	out.Reset()
	if err := compileFrontend(root, &out); err != nil || out.Len() != 0 {
		t.Errorf("an up-to-date application printed %q (%v)", out.String(), err)
	}
	// A JavaScript application has nothing to compile.
	javascript := t.TempDir()
	writeFile(t, filepath.Join(javascript, "app", "javascript", "application.js"), "import \"controllers\"\n")
	if err := compileFrontend(javascript, &out); err != nil || out.Len() != 0 {
		t.Errorf("a JavaScript application: %q, %v", out.String(), err)
	}
	// A syntax error names the file.
	writeFile(t, filepath.Join(root, "app", "javascript", "application.ts"), "class {")
	err := compileFrontend(root, &out)
	if err == nil || !strings.Contains(err.Error(), "compile app/javascript") || !strings.Contains(err.Error(), "application.ts") {
		t.Errorf("a syntax error: %v", err)
	}
}

func TestCheckCompilesTheTypeScriptThenTypeChecksIt(t *testing.T) {
	calls := toolsOnPath(t, map[string]string{"go": "", "tsc": `echo "typed"`})
	t.Chdir(typeScriptProject(t, true))
	var stdout, stderr strings.Builder
	if code := Run([]string{"check"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if got := strings.Join(recorded(t, calls), "|"); got != "go build ./...|go vet ./...|tsc --noEmit --project tsconfig.json" {
		t.Errorf("tools ran as %q", got)
	}
	// The TypeScript is compiled before go build embeds it, and tsc runs last.
	compile := strings.Index(stdout.String(), "  compile app/javascript")
	build := strings.Index(stdout.String(), "  run     go build")
	tsc := strings.Index(stdout.String(), "  run     tsc --noEmit")
	ok := strings.Index(stdout.String(), "check: ok")
	if compile < 0 || compile > build || build > tsc || tsc > ok {
		t.Errorf("stdout out of order:\n%s", stdout.String())
	}
}

func TestCheckWithoutTscSaysHowToGetItAndStillPasses(t *testing.T) {
	calls := toolsOnPath(t, map[string]string{"go": ""})
	t.Chdir(typeScriptProject(t, true))
	var stdout, stderr strings.Builder
	if code := Run([]string{"check"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if strings.Count(stdout.String(), "tsc not found") != 1 {
		t.Errorf("the note must appear exactly once:\n%s", stdout.String())
	}
	for _, want := range []string{"npm install --global typescript@" + frontend.TypeScriptVersion, "check: ok"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
		}
	}
	if got := recorded(t, calls); len(got) != 2 {
		t.Errorf("calls = %v", got)
	}
}

func TestCheckFailsWhenTscFindsATypeError(t *testing.T) {
	toolsOnPath(t, map[string]string{"go": "", "tsc": `echo "app/javascript/application.ts(1,1): error TS2322: Type 'string' is not assignable to type 'number'."; exit 2`})
	t.Chdir(typeScriptProject(t, true))
	var stdout, stderr strings.Builder
	if code := Run([]string{"check"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "error TS2322") || !strings.Contains(stderr.String(), "tsc exited with status 2") {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "check: ok") {
		t.Errorf("a failing type check reported ok: %q", stdout.String())
	}
}

func TestCheckStopsOnATypeScriptSyntaxErrorBeforeBuilding(t *testing.T) {
	calls := toolsOnPath(t, map[string]string{"go": "", "tsc": ""})
	root := typeScriptProject(t, true)
	writeFile(t, filepath.Join(root, "app", "javascript", "application.ts"), "class {")
	t.Chdir(root)
	var stdout, stderr strings.Builder
	if code := Run([]string{"check"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "application.ts") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if got := recorded(t, calls); len(got) != 0 {
		t.Errorf("tools ran after a compile error: %v", got)
	}
}

func TestTypeCheckNotesAMissingTsconfigAndSkipsJavaScriptApplications(t *testing.T) {
	toolsOnPath(t, map[string]string{"tsc": `exit 9`})
	var out strings.Builder
	if err := typeCheckFrontend(context.Background(), typeScriptProject(t, false), &out, &out); err != nil || !strings.Contains(out.String(), "tsconfig.json is missing") {
		t.Errorf("without tsconfig.json: %q, %v", out.String(), err)
	}
	out.Reset()
	javascript := t.TempDir()
	writeFile(t, filepath.Join(javascript, "app", "javascript", "application.js"), "")
	if err := typeCheckFrontend(context.Background(), javascript, &out, &out); err != nil || out.Len() != 0 {
		t.Errorf("a JavaScript application: %q, %v", out.String(), err)
	}
	if err := typeCheckFrontend(context.Background(), filepath.Join(t.TempDir(), "missing"), &out, &out); err == nil {
		t.Error("a missing application directory was accepted")
	}
}

func TestBuildAndTestCompileTheTypeScriptFirst(t *testing.T) {
	calls := toolsOnPath(t, map[string]string{"go": ""})
	root := typeScriptProject(t, true)
	t.Chdir(root)
	var stdout, stderr strings.Builder
	if err := runBuild(nil, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "  compile app/javascript") || !strings.HasSuffix(stdout.String(), "built bin/shop\n") {
		t.Errorf("forge build printed %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(root, "app", "javascript", "dist", "application.js")); err != nil {
		t.Errorf("forge build did not compile: %v", err)
	}
	// forge test: edit a source, and the tests see its compiled output.
	writeFile(t, filepath.Join(root, "app", "javascript", "application.ts"), "import \"controllers\"\nimport \"more\"\n")
	stdout.Reset()
	if err := runTest(nil, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "  compile app/javascript") || !strings.Contains(readFile(t, filepath.Join(root, "app", "javascript", "dist", "application.js")), `import "more";`) {
		t.Errorf("forge test did not compile first: %q", stdout.String())
	}
	writeFile(t, filepath.Join(root, "app", "javascript", "application.ts"), "class {")
	if err := runBuild(nil, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "application.ts") {
		t.Errorf("forge build with a syntax error: %v", err)
	}
	if err := runTest(nil, &stdout, &stderr); err == nil {
		t.Error("forge test ran with a syntax error")
	}
	if got := recorded(t, calls); len(got) != 2 || !strings.HasPrefix(got[0], "go build") || !strings.HasPrefix(got[1], "go test") {
		t.Errorf("calls = %v", got)
	}
}

func TestRunAppCompilesTheTypeScriptFirst(t *testing.T) {
	root := typeScriptProject(t, true)
	if err := os.WriteFile(filepath.Join(root, "cmd", "shop", "main.go"), []byte(echoApp), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if err := runApp(context.Background(), root, "shop", []string{"hello"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "  compile app/javascript") || !strings.Contains(stdout.String(), "args: hello") {
		t.Errorf("stdout %q", stdout.String())
	}
	writeFile(t, filepath.Join(root, "app", "javascript", "application.ts"), "class {")
	if err := runApp(context.Background(), root, "shop", nil, nil, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "application.ts") {
		t.Errorf("a syntax error: %v", err)
	}
}

// embedPackage is app/javascript of the test application: it embeds dist as
// the generated one does.
const embedPackage = `package javascript

import "embed"

// Files is the embedded dist directory.
//
//go:embed dist
var Files embed.FS
`

// embedApp prints the first line of every compiled file it embeds.
const embedApp = `package main

import (
	"fmt"
	"io/fs"
	"strings"

	"example.com/shop/app/javascript"
)

func main() {
	_ = fs.WalkDir(javascript.Files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			content, _ := javascript.Files.ReadFile(name)
			first, _, _ := strings.Cut(string(content), "\n")
			fmt.Printf("%s: %s\n", name, first)
		}
		return nil
	})
	content, _ := javascript.Files.ReadFile("dist/controllers/hello_controller.js")
	fmt.Println(strings.Contains(string(content), "sourceMappingURL"))
}
`

func TestDevBuildEmbedsDevelopmentOutputAndLeavesTheTreeAlone(t *testing.T) {
	t.Parallel()
	root := typeScriptProject(t, true)
	if err := os.WriteFile(filepath.Join(root, "cmd", "shop", "main.go"), []byte(embedApp), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "app", "javascript", "javascript.go"), embedPackage)
	// The committed production output.
	if changed, err := frontend.Sync(root); err != nil || len(changed) != 2 {
		t.Fatalf("Sync = %v, %v", changed, err)
	}
	production := readFile(t, filepath.Join(root, "app", "javascript", "dist", "application.js"))

	app, _ := newDevApp(t, root)
	app.proxy = nil
	// A new controller exists only in the overlay until forge build runs.
	writeFile(t, filepath.Join(root, "app", "javascript", "controllers", "extra_controller.ts"), "export const extra: number = 1\n")
	output := filepath.Join(t.TempDir(), "shop")
	if err := app.build(context.Background(), output); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), output)
	ran, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"dist/application.js: import \"controllers\";", // development output has no header
		"dist/controllers/extra_controller.js: const extra = 1;",
		"\ntrue\n", // the controller carries an inline source map
	} {
		if !strings.Contains(string(ran), want) {
			t.Errorf("the dev binary embeds:\n%s\nwant %q", ran, want)
		}
	}
	if got := readFile(t, filepath.Join(root, "app", "javascript", "dist", "application.js")); got != production {
		t.Errorf("forge dev rewrote the committed production output:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(root, "app", "javascript", "dist", "controllers", "extra_controller.js")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("forge dev wrote into the working tree: %v", err)
	}

	// A TypeScript syntax error is a build failure with the compiler's text.
	writeFile(t, filepath.Join(root, "app", "javascript", "application.ts"), "class {")
	var failure *devloop.BuildError
	if err := app.build(context.Background(), filepath.Join(t.TempDir(), "shop")); !errors.As(err, &failure) || !strings.Contains(failure.Output, "application.ts") {
		t.Errorf("a syntax error: %v", err)
	}
}

func TestNewJSFlagAndItsConflicts(t *testing.T) {
	t.Parallel()
	var out, errOut strings.Builder
	dir := filepath.Join(t.TempDir(), "plain")
	if err := runNew([]string{"plain", "--js", "--skip-deps", "--dir", dir, "-d", "sqlite3"}, &out, &errOut); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "app", "javascript", "application.js")); err != nil {
		t.Errorf("--js has no application.js: %v", err)
	}
	for _, absent := range []string{"tsconfig.json", filepath.Join("app", "javascript", "application.ts"), filepath.Join("app", "javascript", "dist")} {
		if _, err := os.Stat(filepath.Join(dir, absent)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("--js generated %s (%v)", absent, err)
		}
	}

	// The default is TypeScript, and the many declaration files are listed as one line.
	out.Reset()
	ts := filepath.Join(t.TempDir(), "typed")
	if err := runNew([]string{"typed", "--skip-deps", "--dir", ts, "-d", "sqlite3"}, &out, &errOut); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	for _, want := range []string{"app/javascript/application.ts", "app/javascript/dist/controllers/hello_controller.js", "tsconfig.json", "app/javascript/types/ (", "files: type declarations for Turbo and Stimulus)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("forge new printed no %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "app/javascript/types/@hotwired") {
		t.Errorf("the declaration files were listed one by one:\n%s", out.String())
	}
	// --js has no meaning for an API-only application.
	err := runNew([]string{"svc", "--api", "--js", "--skip-deps", "--dir", filepath.Join(t.TempDir(), "svc")}, &out, &errOut)
	var usage usageError
	if !errors.As(err, &usage) || !strings.Contains(err.Error(), "--js cannot be used with --api") {
		t.Errorf("--api --js: %v", err)
	}
}

func TestDeployCompilesTheTypeScriptBeforeBuildingTheImage(t *testing.T) {
	for args, want := range map[string]bool{
		"":                      true,
		"--version 1.2.3":       true,
		"--skip-build":          false,
		"--dry-run":             false,
		"--skip-build=true":     false,
		"--help":                false,
		"setup":                 false,
		"rollback --dry-run":    false,
		"--version v --json":    true,
		"details":               false,
		"--version v -h":        false,
		"--version v --dry-run": false,
	} {
		if got := deployBuilds(strings.Fields(args)); got != want {
			t.Errorf("deployBuilds(%q) = %v, want %v", args, got, want)
		}
	}
	root := typeScriptProject(t, true)
	t.Chdir(root)
	var stdout, stderr strings.Builder
	// The image is built from the working tree: a source error stops the
	// deploy before anything else runs, and the compiled files are current.
	writeFile(t, filepath.Join(root, "app", "javascript", "application.ts"), "class {")
	if code := Run([]string{"deploy"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "compile app/javascript") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	writeFile(t, filepath.Join(root, "app", "javascript", "application.ts"), "import \"controllers\"\n")
	stderr.Reset()
	// No config/deploy.yml: deploy itself fails, after the compilation.
	if code := Run([]string{"deploy"}, &stdout, &stderr); code == 0 || strings.Contains(stderr.String(), "compile app/javascript") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(readFile(t, filepath.Join(root, "app", "javascript", "dist", "application.js")), `import "controllers";`) {
		t.Error("forge deploy did not compile first")
	}
}
