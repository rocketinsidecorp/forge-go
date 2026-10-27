package cli

import (
	"context"
	"errors"
	"io"
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
	if n, err := compileFrontend(root, &out); err != nil || n != 2 {
		t.Fatalf("compileFrontend = %d, %v", n, err)
	}
	if want := "  compile app/javascript/*.ts -> app/javascript/dist (2 file(s) updated, esbuild " + frontend.EsbuildVersion + ")\n"; out.String() != want {
		t.Errorf("first run printed %q, want %q", out.String(), want)
	}
	if got := readFile(t, filepath.Join(root, "app", "javascript", "dist", "controllers", "hello_controller.js")); strings.Contains(got, "name: string") {
		t.Errorf("the types were not stripped:\n%s", got)
	}
	out.Reset()
	if n, err := compileFrontend(root, &out); err != nil || n != 0 || out.Len() != 0 {
		t.Errorf("an up-to-date application printed %q (%d, %v)", out.String(), n, err)
	}
	// A JavaScript application has nothing to compile.
	javascript := t.TempDir()
	writeFile(t, filepath.Join(javascript, "app", "javascript", "application.js"), "import \"controllers\"\n")
	if _, err := compileFrontend(javascript, &out); err != nil || out.Len() != 0 {
		t.Errorf("a JavaScript application: %q, %v", out.String(), err)
	}
	// A syntax error names the file.
	writeFile(t, filepath.Join(root, "app", "javascript", "application.ts"), "class {")
	_, err := compileFrontend(root, &out)
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
	// The TypeScript is compiled before go build embeds it (the notice is on
	// stderr: stdout is the command's), and tsc runs last.
	if !strings.Contains(stderr.String(), "  compile app/javascript") || strings.Contains(stdout.String(), "compile app/javascript") {
		t.Errorf("the compile notice belongs on stderr:\nstdout %q\nstderr %q", stdout.String(), stderr.String())
	}
	build := strings.Index(stdout.String(), "  run     go build")
	tsc := strings.Index(stdout.String(), "  run     tsc --noEmit")
	ok := strings.Index(stdout.String(), "check: ok")
	if build < 0 || build > tsc || tsc > ok {
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
	for _, want := range []string{"npm install --global --ignore-scripts typescript@" + frontend.TypeScriptVersion, "check: ok"} {
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
	if !strings.Contains(stderr.String(), "  compile app/javascript") || stdout.String() != "built bin/shop\n" {
		t.Errorf("forge build printed %q, notices %q", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "app", "javascript", "dist", "application.js")); err != nil {
		t.Errorf("forge build did not compile: %v", err)
	}
	// forge test: edit a source, and the tests see its compiled output.
	writeFile(t, filepath.Join(root, "app", "javascript", "application.ts"), "import \"controllers\"\nimport \"more\"\n")
	stdout.Reset()
	stderr.Reset()
	if err := runTest(nil, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "  compile app/javascript") || strings.Contains(stdout.String(), "compile app") || !strings.Contains(readFile(t, filepath.Join(root, "app", "javascript", "dist", "application.js")), `import "more";`) {
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

// TestApplicationCommandsPrintOnlyWhatTheApplicationPrints is the regression
// for a compile notice written in front of the JSON of `forge openapi` (and
// the table of `forge routes`, and the output of migrate, seed and setup): after
// a .ts edit, stdout of the commands that run the application is exactly the
// application's, nothing is written to the working tree, and nothing else is
// printed. They embed the committed dist as it is: they serve no page.
func TestApplicationCommandsPrintOnlyWhatTheApplicationPrints(t *testing.T) {
	root := typeScriptProject(t, true)
	writeFile(t, filepath.Join(root, "cmd", "shop", "main.go"), echoApp)
	if _, err := frontend.Sync(root); err != nil {
		t.Fatal(err)
	}
	committed := readFile(t, filepath.Join(root, "app", "javascript", "dist", "application.js"))
	// A source edit that dist does not have yet.
	writeFile(t, filepath.Join(root, "app", "javascript", "application.ts"), "import \"controllers\"\nimport \"more\"\n")
	writeFile(t, filepath.Join(root, "app", "javascript", "lib", "format.ts"), "export const x: number = 1\n")

	var out, errOut strings.Builder
	if err := runRoutesAt(nil, &out, &errOut, root); err != nil {
		t.Fatalf("forge routes: %v\n%s", err, errOut.String())
	}
	if out.String() != "args: routes env: development\n" || errOut.Len() != 0 {
		t.Errorf("forge routes printed %q (stderr %q), want only the application's output", out.String(), errOut.String())
	}

	t.Chdir(root)
	for command, run := range map[string]func([]string, io.Writer, io.Writer) error{
		"openapi": runOpenAPI,
		"seed":    runSeed,
		"migrate": runMigrate,
	} {
		out.Reset()
		errOut.Reset()
		if err := run(nil, &out, &errOut); err != nil {
			t.Fatalf("forge %s: %v\n%s", command, err, errOut.String())
		}
		if want := "args: " + command + " env: development\n"; out.String() != want || errOut.Len() != 0 {
			t.Errorf("forge %s printed %q (stderr %q), want %q", command, out.String(), errOut.String(), want)
		}
	}
	if got := readFile(t, filepath.Join(root, "app", "javascript", "dist", "application.js")); got != committed {
		t.Errorf("an application command rewrote dist:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(root, "app", "javascript", "dist", "lib", "format.js")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an application command compiled into the working tree: %v", err)
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

// TestRunAppServesTheDevelopmentBuildLikeForgeDev keeps `forge dev --no-reload`
// consistent with the reloading mode: the binary embeds the TypeScript compiled
// for development, with inline source maps, through the overlay, and the
// committed production output in the working tree is not rewritten.
func TestRunAppServesTheDevelopmentBuildLikeForgeDev(t *testing.T) {
	t.Parallel()
	root := typeScriptProject(t, true)
	writeFile(t, filepath.Join(root, "cmd", "shop", "main.go"), embedApp)
	writeFile(t, filepath.Join(root, "app", "javascript", "javascript.go"), embedPackage)
	if _, err := frontend.Sync(root); err != nil {
		t.Fatal(err)
	}
	production := readFile(t, filepath.Join(root, "app", "javascript", "dist", "controllers", "hello_controller.js"))
	var stdout, stderr strings.Builder
	if err := runApp(context.Background(), root, "shop", nil, nil, &stdout, &stderr); err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "\ntrue\n") || strings.Contains(stdout.String(), "Compiled from") || stderr.Len() != 0 {
		t.Errorf("stdout %q stderr %q: want the development build (source map, no header)", stdout.String(), stderr.String())
	}
	if got := readFile(t, filepath.Join(root, "app", "javascript", "dist", "controllers", "hello_controller.js")); got != production {
		t.Error("forge dev --no-reload rewrote the committed production output")
	}
	writeFile(t, filepath.Join(root, "app", "javascript", "application.ts"), "class {")
	if err := runApp(context.Background(), root, "shop", nil, nil, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "application.ts") {
		t.Errorf("a syntax error: %v", err)
	}
}

const deployTestConfig = `service: shop
image: registry.example.com/shop
registry:
  server: registry.example.com
proxy:
  image: registry.example.com/forge-proxy:1
ssh:
  user: deploy
servers:
  web:
    hosts: [127.0.0.1]
`

// gitIn runs git in dir with a fixed identity.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

// TestDeployCompilesAfterTheConfigurationAndExplainsTheRefusalItCaused: a
// missing or invalid configuration stops forge deploy before anything is
// compiled; when the compilation wrote files, only the uncommitted-changes
// refusal (which the written files cause) says so.
func TestDeployCompilesAfterTheConfigurationAndExplainsTheRefusalItCaused(t *testing.T) {
	root := typeScriptProject(t, true)
	if _, err := frontend.Sync(root); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "config", "deploy.yml"), deployTestConfig)
	secrets := filepath.Join(t.TempDir(), "secrets")
	writeFile(t, secrets, "")
	gitIn(t, root, "init", "-q")
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "initial")
	t.Chdir(root)
	dist := filepath.Join(root, "app", "javascript", "dist", "application.js")
	committed := readFile(t, dist)

	// A source edit that dist does not have yet, and no configuration: the
	// configuration error comes first and nothing is compiled.
	writeFile(t, filepath.Join(root, "app", "javascript", "application.ts"), "import \"controllers\"\nimport \"more\"\n")
	var stdout, stderr strings.Builder
	if code := Run([]string{"deploy", "--config", "absent.yml", "--secrets", secrets}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "absent.yml") || strings.Contains(stderr.String(), "compile") || strings.Contains(stderr.String(), "recompiled") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if readFile(t, dist) != committed {
		t.Error("forge deploy compiled before it read its configuration")
	}

	// A valid configuration: it compiles, the tree is dirty, and the refusal
	// carries the explanation.
	stderr.Reset()
	if code := Run([]string{"deploy", "--secrets", secrets}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "uncommitted changes") || !strings.Contains(stderr.String(), "forge recompiled app/javascript/dist before deploying") || !strings.Contains(stderr.String(), "commit the compiled files") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(readFile(t, dist), `import "more";`) {
		t.Error("forge deploy did not compile")
	}

	// --dry-run and --skip-build compile nothing.
	writeFile(t, filepath.Join(root, "app", "javascript", "application.ts"), "import \"controllers\"\nimport \"other\"\n")
	for _, flag := range []string{"--dry-run", "--skip-build"} {
		stderr.Reset()
		_ = Run([]string{"deploy", flag, "--version", "v1", "--secrets", secrets}, &stdout, &stderr)
		if strings.Contains(readFile(t, dist), `import "other";`) || strings.Contains(stderr.String(), "recompiled") {
			t.Errorf("forge deploy %s compiled", flag)
		}
	}

	// An unrelated uncommitted change, with nothing to compile: the refusal is
	// the plain one, without the explanation.
	if _, err := frontend.Sync(root); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "recompiled")
	writeFile(t, filepath.Join(root, "cmd", "shop", "main.go"), "package main\n\nfunc main() {}\n")
	stderr.Reset()
	if code := Run([]string{"deploy", "--secrets", secrets}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "uncommitted changes") || strings.Contains(stderr.String(), "recompiled") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}
