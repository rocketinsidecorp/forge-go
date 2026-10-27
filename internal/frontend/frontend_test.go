package frontend

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

const controllerSource = `import { Controller } from "@hotwired/stimulus"
import type { Application } from "@hotwired/stimulus"

interface Greeting { name: string }

export default class extends Controller {
  static targets = ["name"]
  declare readonly nameTarget: HTMLInputElement
  greet(greeting: Greeting): string { return ` + "`Hello, ${greeting.name}!`" + ` }
}

export type Unused = Application
`

func TestCompileStripsTypesAndKeepsImportsAsWritten(t *testing.T) {
	t.Parallel()
	output, err := Compile("controllers/hello_controller.ts", []byte(controllerSource), Production)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	for _, want := range []string{
		`import { Controller } from "@hotwired/stimulus";`, // a bare specifier: the import map resolves it
		`static targets = ["name"];`,                       // ES2022 class fields stay as they are
		"greet(greeting) {",
		"export {",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"interface", "declare", "HTMLInputElement", ": string", "import type", "Application", "sourceMappingURL", "require("} {
		if strings.Contains(text, forbidden) {
			t.Errorf("output still has %q:\n%s", forbidden, text)
		}
	}
	name, hash, ok := ParseHeader(output)
	if !ok || name != "controllers/hello_controller.ts" || hash != SourceHash([]byte(controllerSource)) {
		t.Errorf("header = %q %q %v", name, hash, ok)
	}
	again, err := Compile("controllers/hello_controller.ts", []byte(controllerSource), Production)
	if err != nil || string(again) != text {
		t.Errorf("production output is not deterministic (err %v)", err)
	}
}

// TestCompileKeepsImportsWithoutTypes pins verbatimModuleSyntax: tsc checks
// the sources under that option, so esbuild must keep the same imports it
// keeps. A side-effect import and a value import that is never used stay.
func TestCompileKeepsImportsWithoutTypes(t *testing.T) {
	t.Parallel()
	output, err := Compile("application.ts", []byte("import \"@hotwired/turbo\"\nimport \"controllers\"\nimport { session } from \"@hotwired/turbo\"\nimport type { Visit } from \"@hotwired/turbo\"\n"), Production)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`import "@hotwired/turbo";`, `import "controllers";`, `import { session } from "@hotwired/turbo";`} {
		if !strings.Contains(string(output), want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
	if strings.Contains(string(output), "Visit") {
		t.Errorf("a type-only import survived:\n%s", output)
	}
}

func TestCompileDevelopmentHasAnInlineSourceMapAndNoHeader(t *testing.T) {
	t.Parallel()
	output, err := Compile("controllers/hello_controller.ts", []byte(controllerSource), Development)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	if _, _, ok := ParseHeader(output); ok || strings.HasPrefix(text, "// Compiled") {
		t.Errorf("a development build must not carry the production header (its line would shift the map):\n%s", text)
	}
	if !strings.Contains(text, "//# sourceMappingURL=data:application/json;base64,") {
		t.Errorf("no inline source map:\n%s", text)
	}
}

func TestCompileReportsSyntaxErrorsWithTheirLocation(t *testing.T) {
	t.Parallel()
	_, err := Compile("controllers/broken.ts", []byte("export default class {\n  greet( {\n}\n"), Production)
	if err == nil {
		t.Fatal("a syntax error compiled")
	}
	if !strings.Contains(err.Error(), "app/javascript/controllers/broken.ts:") || !strings.Contains(err.Error(), "ERROR") {
		t.Errorf("the diagnostic does not name the file and the error: %v", err)
	}
}

func TestCompileRefusesAnOversizedSource(t *testing.T) {
	t.Parallel()
	_, err := Compile("big.ts", []byte(strings.Repeat("a", maxSourceBytes+1)), Production)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseHeaderRejectsOtherText(t *testing.T) {
	t.Parallel()
	for _, text := range []string{"", "export {}\n", "// Compiled from x.ts by someone\n", "// Compiled from x.ts by forge (esbuild 1.0); do not edit. source-sha256: ABC\n"} {
		if _, _, ok := ParseHeader([]byte(text)); ok {
			t.Errorf("%q parsed as a header", text)
		}
	}
	if got := DistName("controllers/a_controller.ts"); got != "controllers/a_controller.js" {
		t.Errorf("DistName = %q", got)
	}
}

func TestSourcesListsOnlyTheApplicationsTypeScript(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{}
	for _, name := range []string{
		"app/javascript/application.ts", "app/javascript/controllers/index.ts", "app/javascript/controllers/a_controller.ts",
		"app/javascript/shapes.d.ts",                    // declarations are not compiled
		"app/javascript/types/@hotwired/turbo/index.ts", // vendored declarations
		"app/javascript/dist/application.ts",            // output
		"app/javascript/node_modules/lib/index.ts",      // never read
		"app/javascript/.cache/x.ts",                    // hidden directories
		"app/javascript/controllers/notes.txt", "app/other/x.ts", "main.ts",
	} {
		files[name] = &fstest.MapFile{Data: []byte("export {}")}
	}
	got, err := Sources(files)
	if err != nil {
		t.Fatal(err)
	}
	want := "application.ts controllers/a_controller.ts controllers/index.ts"
	if strings.Join(got, " ") != want {
		t.Errorf("Sources = %v, want %s", got, want)
	}
	if none, err := Sources(fstest.MapFS{}); err != nil || len(none) != 0 {
		t.Errorf("an application without app/javascript: %v %v", none, err)
	}
}

func TestSourcesRefusesTooManyFiles(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{}
	for i := range maxSources + 1 {
		files[fmt.Sprintf("app/javascript/f%04d.ts", i)] = &fstest.MapFile{}
	}
	if _, err := Sources(files); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("err = %v", err)
	}
}

func TestIsTypeScriptNeedsTheEntryPoint(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		files fstest.MapFS
		want  bool
	}{
		"typescript":  {fstest.MapFS{"app/javascript/application.ts": {}}, true},
		"javascript":  {fstest.MapFS{"app/javascript/application.js": {}}, false},
		"none":        {fstest.MapFS{}, false},
		"a directory": {fstest.MapFS{"app/javascript/application.ts/x": {}}, false},
	} {
		if got := IsTypeScript(test.files); got != test.want {
			t.Errorf("%s: IsTypeScript = %v", name, got)
		}
	}
}

func TestBuildCompilesEverySourceAndReportsEveryError(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{
		"app/javascript/application.ts":        {Data: []byte("import \"controllers\"\n")},
		"app/javascript/controllers/index.ts":  {Data: []byte("export const x: number = 1\n")},
		"app/javascript/types/ignored.d.ts":    {Data: []byte("declare const y: number")},
		"app/javascript/controllers/broken.ts": {Data: []byte("let = = 1")},
		"app/javascript/other_broken.ts":       {Data: []byte("class {")},
	}
	_, err := Build(files, Production)
	if err == nil || !strings.Contains(err.Error(), "broken.ts") || !strings.Contains(err.Error(), "other_broken.ts") {
		t.Fatalf("both failing files must be reported: %v", err)
	}
	delete(files, "app/javascript/controllers/broken.ts")
	delete(files, "app/javascript/other_broken.ts")
	outputs, err := Build(files, Production)
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 2 || !strings.Contains(string(outputs["controllers/index.js"]), "const x = 1;") || outputs["application.js"] == nil {
		t.Errorf("outputs = %v", outputs)
	}
}

// writeApp writes files below a new temporary application root.
func writeApp(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
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

func read(t *testing.T, root, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func TestSyncWritesChangedFilesAndRemovesOrphans(t *testing.T) {
	t.Parallel()
	root := writeApp(t, map[string]string{
		"app/javascript/application.ts":       "import \"controllers\"\n",
		"app/javascript/controllers/index.ts": "export const x: number = 1\n",
		// Left over from a controller that was deleted, and a file Sync does not own.
		"app/javascript/dist/controllers/gone_controller.js": "export {}\n",
		"app/javascript/dist/notes.txt":                      "mine\n",
	})
	changed, err := Sync(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(changed, " ") != "application.js controllers/gone_controller.js controllers/index.js" {
		t.Errorf("changed = %v", changed)
	}
	if got := read(t, root, "app/javascript/dist/controllers/index.js"); !strings.Contains(got, "const x = 1;") || !strings.HasPrefix(got, "// Compiled from controllers/index.ts by forge") {
		t.Errorf("dist/controllers/index.js:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(root, "app", "javascript", "dist", "controllers", "gone_controller.js")); !os.IsNotExist(err) {
		t.Errorf("the orphan was kept: %v", err)
	}
	if got := read(t, root, "app/javascript/dist/notes.txt"); got != "mine\n" {
		t.Errorf("a file that is not compiled output was touched: %q", got)
	}

	// Nothing changed: nothing is written (the timestamp proves it).
	index := filepath.Join(root, "app", "javascript", "dist", "controllers", "index.js")
	before, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := Sync(root); err != nil || len(changed) != 0 {
		t.Fatalf("second Sync = %v, %v", changed, err)
	}
	if after, _ := os.Stat(index); !after.ModTime().Equal(before.ModTime()) {
		t.Error("an unchanged file was rewritten")
	}

	// A source change rewrites only its output.
	if err := os.WriteFile(filepath.Join(root, "app", "javascript", "controllers", "index.ts"), []byte("export const x: number = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := Sync(root); err != nil || strings.Join(changed, " ") != "controllers/index.js" {
		t.Fatalf("third Sync = %v, %v", changed, err)
	}
}

func TestSyncLeavesJavaScriptApplicationsAlone(t *testing.T) {
	t.Parallel()
	root := writeApp(t, map[string]string{"app/javascript/application.js": "import \"controllers\"\n", "app/javascript/stray.ts": "export {}"})
	changed, err := Sync(root)
	if err != nil || len(changed) != 0 {
		t.Fatalf("Sync = %v, %v", changed, err)
	}
	if _, err := os.Stat(filepath.Join(root, "app", "javascript", "dist")); !os.IsNotExist(err) {
		t.Errorf("dist was created for a JavaScript application: %v", err)
	}
}

func TestSyncReportsCompileAndFilesystemErrors(t *testing.T) {
	t.Parallel()
	if _, err := Sync(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing application directory was accepted")
	}
	broken := writeApp(t, map[string]string{"app/javascript/application.ts": "class {"})
	if _, err := Sync(broken); err == nil || !strings.Contains(err.Error(), "application.ts") {
		t.Errorf("a compile error: %v", err)
	}
	// dist is a file: the directory for the output cannot be made.
	blocked := writeApp(t, map[string]string{"app/javascript/application.ts": "export {}", "app/javascript/dist": "not a directory"})
	if _, err := Sync(blocked); err == nil {
		t.Error("Sync wrote below a file")
	}
}

func TestOverlayMapsDevelopmentOutputOverDistWithoutTouchingTheTree(t *testing.T) {
	t.Parallel()
	root := writeApp(t, map[string]string{
		"app/javascript/application.ts":                      "import \"controllers\"\n",
		"app/javascript/controllers/index.ts":                "export const x: number = 1\n",
		"app/javascript/dist/application.js":                 "// production\n",
		"app/javascript/dist/controllers/gone_controller.js": "export {}\n",
	})
	scratch := t.TempDir()
	overlay, err := Overlay(root, scratch)
	if err != nil {
		t.Fatal(err)
	}
	if overlay != filepath.Join(scratch, overlayFile) {
		t.Fatalf("overlay = %q", overlay)
	}
	var decoded struct{ Replace map[string]string }
	if err := jsonUnmarshal(read(t, scratch, overlayFile), &decoded); err != nil {
		t.Fatal(err)
	}
	absolute, _ := filepath.Abs(root)
	application := filepath.Join(absolute, "app", "javascript", "dist", "application.js")
	if decoded.Replace[application] != filepath.Join(scratch, "application.js") {
		t.Errorf("application.js is not mapped to the development output: %v", decoded.Replace)
	}
	if target, ok := decoded.Replace[filepath.Join(absolute, "app", "javascript", "dist", "controllers", "gone_controller.js")]; !ok || target != "" {
		t.Errorf("an orphan must be hidden from the build with an empty path: %v", decoded.Replace)
	}
	if got := read(t, scratch, "controllers/index.js"); !strings.Contains(got, "sourceMappingURL") {
		t.Errorf("the overlay output has no source map:\n%s", got)
	}
	if got := read(t, root, "app/javascript/dist/application.js"); got != "// production\n" {
		t.Errorf("the working tree changed: %q", got)
	}
}

func TestOverlayHandlesApplicationsWithoutTypeScriptAndErrors(t *testing.T) {
	t.Parallel()
	javascript := writeApp(t, map[string]string{"app/javascript/application.js": "export {}"})
	if overlay, err := Overlay(javascript, t.TempDir()); err != nil || overlay != "" {
		t.Errorf("a JavaScript application: %q, %v", overlay, err)
	}
	if _, err := Overlay(filepath.Join(t.TempDir(), "missing"), t.TempDir()); err == nil {
		t.Error("a missing application directory was accepted")
	}
	broken := writeApp(t, map[string]string{"app/javascript/application.ts": "class {"})
	if _, err := Overlay(broken, t.TempDir()); err == nil {
		t.Error("a syntax error was accepted")
	}
	good := writeApp(t, map[string]string{"app/javascript/application.ts": "export {}"})
	// The scratch path is a file: nothing can be written below it.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Overlay(good, file); err == nil {
		t.Error("an unwritable scratch directory was accepted")
	}
}

// TestPinnedVersionsMatchGoMod keeps the constants the documentation, the
// generated files and the CI quote equal to what is linked.
func TestPinnedVersionsMatchGoMod(t *testing.T) {
	t.Parallel()
	mod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^\s*github\.com/evanw/esbuild v(\S+)(.*)$`).FindSubmatch(mod)
	if len(match) < 3 || string(match[1]) != EsbuildVersion {
		t.Fatalf("go.mod pins esbuild %q, the package says %q", match, EsbuildVersion)
	}
	if strings.Contains(string(match[2]), "indirect") {
		t.Errorf("esbuild is imported directly, but go.mod marks it indirect: %s", match[0])
	}
}

func jsonUnmarshal(text string, into any) error { return json.Unmarshal([]byte(text), into) }

// brokenFS fails to open the named files or directories.
type brokenFS struct {
	fstest.MapFS
	broken map[string]bool
}

func (b brokenFS) Open(name string) (fs.File, error) {
	if b.broken[name] {
		return nil, errors.New("injected failure for " + name)
	}
	return b.MapFS.Open(name)
}

// ReadFile and ReadDir are overridden because MapFS implements them itself,
// so fs.ReadFile and fs.WalkDir would not reach Open.
func (b brokenFS) ReadFile(name string) ([]byte, error) {
	if b.broken[name] {
		return nil, errors.New("injected failure for " + name)
	}
	return b.MapFS.ReadFile(name)
}

func (b brokenFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if b.broken[name] {
		return nil, errors.New("injected failure for " + name)
	}
	return b.MapFS.ReadDir(name)
}

func TestFilesystemErrorsAreReported(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{
		"app/javascript/application.ts":       {Data: []byte("export {}")},
		"app/javascript/controllers/index.ts": {Data: []byte("export {}")},
	}
	unreadable := brokenFS{files, map[string]bool{"app/javascript/controllers/index.ts": true}}
	if _, err := Build(unreadable, Production); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Errorf("an unreadable source: %v", err)
	}
	unlistable := brokenFS{files, map[string]bool{"app/javascript/controllers": true}}
	if _, err := Sources(unlistable); err == nil {
		t.Error("an unreadable directory was accepted")
	}
	if _, err := Build(unlistable, Production); err == nil {
		t.Error("Build ignored a listing error")
	}
	files["app/javascript/dist/application.js"] = &fstest.MapFile{}
	brokenDist := brokenFS{files, map[string]bool{"app/javascript/dist": true}}
	if _, err := existingDist(brokenDist); err == nil {
		t.Error("an unreadable dist was accepted")
	}
	if found, err := existingDist(fstest.MapFS{}); err != nil || len(found) != 0 {
		t.Errorf("a missing dist: %v %v", found, err)
	}
}

func TestSyncAndOverlayReportWriteFailures(t *testing.T) {
	t.Parallel()
	// The output name is taken by a directory.
	root := writeApp(t, map[string]string{"app/javascript/application.ts": "export {}", "app/javascript/dist/application.js/keep": "x"})
	if _, err := Sync(root); err == nil {
		t.Error("Sync wrote over a directory")
	}
	good := writeApp(t, map[string]string{"app/javascript/application.ts": "export {}"})
	scratch := t.TempDir()
	if err := os.Mkdir(filepath.Join(scratch, "application.js"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Overlay(good, scratch); err == nil {
		t.Error("Overlay wrote over a directory")
	}
	scratch = t.TempDir()
	if err := os.Mkdir(filepath.Join(scratch, overlayFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Overlay(good, scratch); err == nil {
		t.Error("Overlay wrote its file over a directory")
	}
}

// TestGoBuildEmbedsTheOverlay is the contract `forge dev` relies on: with
// `go build -overlay`, a go:embed pattern sees the development output where
// dist is, adds files that are not on disk and hides the orphans, while the
// disk keeps the production output.
func TestGoBuildEmbedsTheOverlay(t *testing.T) {
	t.Parallel()
	root := writeApp(t, map[string]string{
		"go.mod": "module overlaytest\n\ngo 1.27.3\n",
		"main.go": `package main

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"
)

//go:embed app/javascript/dist
var files embed.FS

func main() {
	_ = fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			content, _ := files.ReadFile(name)
			first, _, _ := strings.Cut(string(content), "\n")
			fmt.Printf("%s: %s\n", name, first)
		}
		return nil
	})
}
`,
		"app/javascript/application.ts":                      "import \"controllers\"\n",
		"app/javascript/controllers/index.ts":                "export const x: number = 1\n",
		"app/javascript/dist/application.js":                 "// production output\n",
		"app/javascript/dist/controllers/gone_controller.js": "export {}\n",
	})
	scratch := t.TempDir()
	overlay, err := Overlay(root, scratch)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "go", "run", "-overlay", overlay, ".")
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go run -overlay: %v\n%s", err, output)
	}
	// The first line of each embedded file: the development output, not the
	// production text on disk, and no orphan.
	want := "app/javascript/dist/application.js: import \"controllers\";\napp/javascript/dist/controllers/index.js: const x = 1;\n"
	if string(output) != want {
		t.Errorf("embedded files:\n%s\nwant:\n%s", output, want)
	}
}
