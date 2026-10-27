package scaffold

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/hotwire"
	"github.com/rocketinsidecorp/forge-go/internal/frontend"
)

// typesSHA256 is the SHA-256 over the declarations of Turbo and Stimulus in
// templates/typescript/types (each file's path, a NUL byte, its content and a
// NUL byte, in path order). They are copied unchanged from the npm packages
// types/README.md names, so an edit, a partial update or a new file fails here
// and the change must be a deliberate one: update the files, the README and
// this constant together (ADR 0049).
const typesSHA256 = "6305e4735fc6d331eeb0e5c40543af37bd40d38817561193e300dc34a14533f6"

func TestTypeDeclarationsArePinned(t *testing.T) {
	t.Parallel()
	hash := sha256.New()
	count := 0
	err := fs.WalkDir(templateFS, typescriptDir+"/types", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		content, err := templateFS.ReadFile(name)
		if err != nil {
			return err
		}
		hash.Write([]byte(strings.TrimPrefix(name, typescriptDir+"/types/")))
		hash.Write([]byte{0})
		hash.Write(content)
		hash.Write([]byte{0})
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != typesSHA256 {
		t.Fatalf("the type declarations changed (%d files): SHA-256 %s, pinned %s", count, got, typesSHA256)
	}
	readme, err := templateFS.ReadFile(typescriptDir + "/types/README.md")
	if err != nil {
		t.Fatal(err)
	}
	// The provenance names the versions the framework serves, so the
	// declarations cannot describe other code than the vendored modules.
	for _, want := range []string{"`@hotwired/stimulus` 3.2.2", "`@hotwired/turbo` 8.0.23", "@types/hotwired__turbo` 8.0.16"} {
		if !strings.Contains(string(readme), want) {
			t.Errorf("types/README.md does not say %q", want)
		}
	}
	if hotwire.StimulusVersion != "3.2.2" || hotwire.TurboVersion != "8.0.23" {
		t.Errorf("the vendored modules are Turbo %s and Stimulus %s: update the declarations and types/README.md", hotwire.TurboVersion, hotwire.StimulusVersion)
	}
	for _, licence := range []string{"@hotwired/stimulus/LICENSE.md", "@hotwired/turbo/LICENSE"} {
		if _, err := templateFS.ReadFile(path.Join(typescriptDir, "types", licence)); err != nil {
			t.Errorf("a licence text is missing: %v", err)
		}
	}
}

func generateApp(t *testing.T, opts Options) (string, []string) {
	t.Helper()
	opts.Name = "shop"
	opts.Dir = filepath.Join(t.TempDir(), "shop")
	paths, err := Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	return opts.Dir, paths
}

func TestGenerateWritesTypeScriptByDefault(t *testing.T) {
	t.Parallel()
	dir, paths := generateApp(t, Options{Database: "sqlite"})
	for _, want := range []string{
		"app/javascript/application.ts", "app/javascript/controllers/application.ts", "app/javascript/controllers/index.ts", "app/javascript/controllers/hello_controller.ts",
		"app/javascript/dist/application.js", "app/javascript/dist/controllers/application.js", "app/javascript/dist/controllers/index.js", "app/javascript/dist/controllers/hello_controller.js",
		"app/javascript/javascript.go", "app/javascript/javascript_test.go", "tsconfig.json",
		"app/javascript/types/README.md", "app/javascript/types/@hotwired/turbo/index.d.ts", "app/javascript/types/@hotwired/stimulus/index.d.ts",
	} {
		if !slices.Contains(paths, filepath.FromSlash(want)) {
			t.Errorf("missing %s", want)
		}
	}
	for _, forbidden := range []string{"app/javascript/application.js", "app/javascript/controllers/index.js", "app/javascript/controllers/hello_controller.js"} {
		if slices.Contains(paths, filepath.FromSlash(forbidden)) {
			t.Errorf("%s is JavaScript source: the default front end is TypeScript", forbidden)
		}
	}
	// Every dist file is what forge build writes for its source.
	project, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = project.Close() }()
	built, err := frontend.Build(project.FS(), frontend.Production)
	if err != nil {
		t.Fatal(err)
	}
	if len(built) != 4 {
		t.Fatalf("the application has %d TypeScript sources, want 4", len(built))
	}
	for name, want := range built {
		got, err := os.ReadFile(filepath.Join(dir, "app", "javascript", "dist", filepath.FromSlash(name)))
		if err != nil || string(got) != string(want) {
			t.Errorf("dist/%s is not what forge build writes (%v)", name, err)
		}
		if strings.Contains(string(got), ": HTMLInputElement") || strings.Contains(string(got), "declare ") {
			t.Errorf("dist/%s has type syntax:\n%s", name, got)
		}
	}
	// forge build on the fresh application changes nothing.
	if changed, err := frontend.Sync(dir); err != nil || len(changed) != 0 {
		t.Errorf("Sync on a fresh application = %v, %v", changed, err)
	}
	assertFilesContain(t, dir, map[string][]string{
		"app/javascript/dist/controllers/hello_controller.js": {`import { Controller } from "@hotwired/stimulus";`, "static targets = [", "greet() {"},
		".github/workflows/ci.yml":                            {"typescript@" + frontend.TypeScriptVersion, "--ignore-scripts", "tsc --noEmit"},
		".gitignore":                                          {"/node_modules/"},
		".dockerignore":                                       {"node_modules"},
		"tsconfig.json":                                       {`"strict": true`, `"noEmit": true`, `"moduleResolution": "bundler"`, `"@hotwired/stimulus": ["./app/javascript/types/@hotwired/stimulus/index.d.ts"]`, `"verbatimModuleSyntax": true`},
	})
}

func TestGenerateJSKeepsPlainJavaScript(t *testing.T) {
	t.Parallel()
	dir, paths := generateApp(t, Options{Database: "sqlite", JS: true})
	for _, want := range []string{"app/javascript/application.js", "app/javascript/controllers/index.js", "app/javascript/controllers/application.js", "app/javascript/controllers/hello_controller.js", "app/javascript/javascript.go"} {
		if !slices.Contains(paths, filepath.FromSlash(want)) {
			t.Errorf("missing %s", want)
		}
	}
	for _, path := range paths {
		slashed := filepath.ToSlash(path)
		if strings.HasSuffix(slashed, ".ts") || strings.Contains(slashed, "app/javascript/dist/") || strings.Contains(slashed, "app/javascript/types") || slashed == "tsconfig.json" || strings.HasSuffix(slashed, "javascript_test.go") {
			t.Errorf("--js generated %s", slashed)
		}
	}
	for file, forbidden := range map[string]string{".github/workflows/ci.yml": "tsc", ".gitignore": "node_modules", ".dockerignore": "node_modules"} {
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(file)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), forbidden) {
			t.Errorf("%s mentions %q with --js", file, forbidden)
		}
	}
	assertFilesContain(t, dir, map[string][]string{"app/javascript/javascript.go": {"//go:embed application.js controllers/*.js", "func Files() fs.FS { return files }"}})
	if changed, err := frontend.Sync(dir); err != nil || len(changed) != 0 {
		t.Errorf("Sync touched a JavaScript application: %v, %v", changed, err)
	}
}

func TestNormalizeRefusesJSWithAPI(t *testing.T) {
	t.Parallel()
	if _, err := Normalize(Options{Name: "svc", API: true, JS: true}); err == nil || !strings.Contains(err.Error(), "--js") {
		t.Fatalf("err = %v", err)
	}
	dir, paths := generateApp(t, Options{API: true})
	for _, path := range paths {
		if strings.Contains(filepath.ToSlash(path), "javascript") || path == "tsconfig.json" {
			t.Errorf("an API-only application got %s", path)
		}
	}
	for file, forbidden := range map[string]string{".github/workflows/ci.yml": "tsc", ".gitignore": "node_modules"} {
		content, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), forbidden) {
			t.Errorf("%s mentions %q in an API-only application", file, forbidden)
		}
	}
}

func TestViewBaseAddsTheTypeScriptFrontEnd(t *testing.T) {
	t.Parallel()
	base, err := ViewBase("shop", "shop")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]string{}
	for _, file := range base {
		have[filepath.ToSlash(file.Path)] = string(file.Content)
	}
	for _, want := range []string{"app/javascript/application.ts", "app/javascript/dist/application.js", "app/javascript/dist/controllers/hello_controller.js", "app/javascript/javascript_test.go", "tsconfig.json", "app/javascript/types/@hotwired/turbo/index.d.ts"} {
		if _, ok := have[want]; !ok {
			t.Errorf("ViewBase lacks %s", want)
		}
	}
	if _, ok := have["app/javascript/application.js"]; ok {
		t.Error("ViewBase adds JavaScript sources")
	}
	if !strings.HasPrefix(have["app/javascript/dist/application.js"], "// Compiled from application.ts by forge") {
		t.Errorf("dist/application.js has no header:\n%s", have["app/javascript/dist/application.js"])
	}
}

func TestCompiledFilesReportsCompileErrors(t *testing.T) {
	t.Parallel()
	if files, err := compiledFiles(Options{API: true}); err != nil || files != nil {
		t.Errorf("API: %v, %v", files, err)
	}
	if files, err := compiledFiles(Options{JS: true}); err != nil || files != nil {
		t.Errorf("JS: %v, %v", files, err)
	}
	files, err := compiledFiles(Options{})
	if err != nil || len(files) != 4 {
		t.Fatalf("%d files, %v", len(files), err)
	}
}

func TestTypeScriptVersionIsOneConstant(t *testing.T) {
	t.Parallel()
	if got := (templateData{}).TypeScriptVersion(); got != frontend.TypeScriptVersion || !strings.Contains(got, ".") {
		t.Errorf("TypeScriptVersion = %q", got)
	}
}

// assertFilesContain fails unless each file below dir contains every text.
func assertFilesContain(t *testing.T, dir string, expectations map[string][]string) {
	t.Helper()
	for file, texts := range expectations {
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(file)))
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range texts {
			if !strings.Contains(string(content), text) {
				t.Errorf("%s lacks %q", file, text)
			}
		}
	}
}
