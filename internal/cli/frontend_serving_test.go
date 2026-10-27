package cli

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/waittest"
)

var (
	// The digested script URL the layout loads, and the import map entry of a
	// controller, as the page renders them.
	entryScript      = regexp.MustCompile(`<script type="module" src="(/assets/application-[0-9a-f]{16}\.js)">`)
	helloPin         = regexp.MustCompile(`"controllers/hello_controller":"(/assets/controllers/hello_controller-[0-9a-f]{16}\.js)"`)
	stimulusPin      = regexp.MustCompile(`"@hotwired/stimulus":"(/assets/vendor/stimulus-[0-9a-f]{16}\.js)"`)
	typeScriptSyntax = []string{"declare ", ": HTMLInputElement", ": HTMLElement", "): void", "interface "}
)

// fetcher returns the body and content type of GET path, which must answer 200.
type fetcher func(path string) (body, contentType string)

// fetcherFor is a fetcher of the server at base, without cookies.
func fetcherFor(t *testing.T, base string) fetcher {
	return func(path string) (string, string) { return fetch(t, base, path) }
}

// fetch returns the body of GET base+path, which must answer 200.
func fetch(t *testing.T, base, path string) (body, contentType string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = response.Body.Close() }()
	content, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, %v", path, response.StatusCode, err)
	}
	return string(content), response.Header.Get("Content-Type")
}

// assertServesCompiledJavaScript reads the page at pagePath the way a browser
// does: the entry script, the controller pinned in the import map and the
// vendored Stimulus it imports. It returns the controller's body.
func assertServesCompiledJavaScript(t *testing.T, get fetcher, pagePath string) (application, hello string) {
	t.Helper()
	page, _ := get(pagePath)
	entry, pin, stimulus := entryScript.FindStringSubmatch(page), helloPin.FindStringSubmatch(page), stimulusPin.FindStringSubmatch(page)
	if entry == nil || pin == nil || stimulus == nil {
		t.Fatalf("%s has no entry script, controller pin or Stimulus pin in its import map:\n%s", pagePath, page)
	}
	application, contentType := get(entry[1])
	if !strings.HasPrefix(contentType, "text/javascript") && !strings.HasPrefix(contentType, "application/javascript") {
		t.Errorf("%s is served as %q", entry[1], contentType)
	}
	hello, _ = get(pin[1])
	for name, body := range map[string]string{"application": application, "hello_controller": hello} {
		for _, syntax := range typeScriptSyntax {
			if strings.Contains(body, syntax) {
				t.Errorf("%s served with TypeScript syntax %q:\n%s", name, syntax, body)
			}
		}
	}
	if !strings.Contains(hello, `import { Controller } from "@hotwired/stimulus"`) || !strings.Contains(hello, "static targets = [") || !strings.Contains(hello, "greet()") {
		t.Errorf("hello_controller is not the compiled controller:\n%s", hello)
	}
	if !strings.Contains(application, `import "@hotwired/turbo"`) || !strings.Contains(application, `import "controllers"`) {
		t.Errorf("application is not the compiled entry point:\n%s", application)
	}
	if stimulusJS, _ := get(stimulus[1]); !strings.Contains(stimulusJS, "class Controller") {
		t.Error("the vendored Stimulus is not served where the import map says")
	}
	return application, hello
}

// TestGeneratedTypeScriptApplicationServesPrecompiledJavaScript builds the
// generated application with plain `go build`, as its Dockerfile does (no
// forge, no esbuild, no overlay), runs it, and loads its page like a browser:
// the digested scripts are exactly the files committed in app/javascript/dist.
func TestGeneratedTypeScriptApplicationServesPrecompiledJavaScript(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on a generated project")
	}
	t.Parallel()
	dir := matrixApplication(t, "sqlite", nil)
	if err := runGenerateAt([]string{"controller", "pages", "home", "--public"}, io.Discard, dir); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "mx")
	goInMatrix(t, dir, "build", "-trimpath", "-o", binary, "./cmd/mx")

	address := freeLoopbackAddress(t)
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "FORGE_") {
			env = append(env, entry)
		}
	}
	env = append(env, "FORGE_ENV=development", "FORGE_HTTP_ADDR="+address, "FORGE_DATABASE_URL=sqlite:"+filepath.Join(t.TempDir(), "mx.db"))
	ctx, cancel := context.WithCancel(context.Background())
	server := exec.CommandContext(ctx, binary, "serve")
	server.Dir, server.Env = dir, env
	var logs lockedBuffer
	server.Stdout, server.Stderr = &logs, &logs
	server.Cancel = func() error { return server.Process.Signal(os.Interrupt) }
	server.WaitDelay = 30 * time.Second
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = server.Wait()
		if t.Failed() {
			t.Logf("server output:\n%s", logs.String())
		}
	})
	base := "http://" + address
	waittest.Until(t, time.Minute, "the application to serve "+base+"/health/ready", func() bool {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health/ready", nil)
		if err != nil {
			return false
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	})

	application, hello := assertServesCompiledJavaScript(t, fetcherFor(t, base), "/pages/home")
	for served, file := range map[string]string{application: "application.js", hello: "controllers/hello_controller.js"} {
		if want := readFile(t, filepath.Join(dir, "app", "javascript", "dist", filepath.FromSlash(file))); served != want {
			t.Errorf("the binary serves something other than the committed dist/%s:\nserved:\n%s\ncommitted:\n%s", file, served, want)
		}
	}
	if strings.Contains(hello, "sourceMappingURL") {
		t.Error("the production build serves a source map")
	}
}

// TestGeneratedTypeScriptTypeChecksAndCatchesTypeErrors runs the real tsc on a
// generated application with its own tsconfig.json and the vendored
// declarations: the generated sources pass, and a type error and a call that
// Stimulus's declarations reject fail. It needs tsc (npm install --global
// typescript@<frontend.TypeScriptVersion>); CI sets FORGE_TEST_REQUIRE_TSC so
// a missing tsc fails there instead of skipping.
func TestGeneratedTypeScriptTypeChecksAndCatchesTypeErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("runs tsc on a generated project")
	}
	t.Parallel()
	if _, err := exec.LookPath("tsc"); err != nil {
		if os.Getenv("FORGE_TEST_REQUIRE_TSC") == "true" {
			t.Fatal("tsc is not on PATH but FORGE_TEST_REQUIRE_TSC=true")
		}
		t.Skip("tsc is not on PATH (npm install --global typescript)")
	}
	dir := matrixApplication(t, "sqlite", nil)
	var out strings.Builder
	if err := typeCheckFrontend(context.Background(), dir, &out, &out); err != nil {
		t.Fatalf("the generated TypeScript does not type-check: %v\n%s", err, out.String())
	}
	controller := filepath.Join(dir, "app", "javascript", "controllers", "hello_controller.ts")
	original := readFile(t, controller)
	for name, test := range map[string]struct{ old, new, want string }{
		"a type error":                  {"greet(): void {", "greet(): number {", "hello_controller.ts(17"},
		"an unknown member of a target": {"this.nameTarget.value", "this.nameTarget.valuee", "valuee"},
		"a wrong Stimulus call":         {`import { Controller } from "@hotwired/stimulus"`, "import { Controller, Missing } from \"@hotwired/stimulus\"", "Missing"},
	} {
		if !strings.Contains(original, test.old) {
			t.Fatalf("hello_controller.ts has no %q", test.old)
		}
		writeFile(t, controller, strings.Replace(original, test.old, test.new, 1))
		out.Reset()
		err := typeCheckFrontend(context.Background(), dir, &out, &out)
		if err == nil || !strings.Contains(out.String(), "error TS") || !strings.Contains(out.String(), test.want) {
			t.Errorf("%s: err %v, output:\n%s\nwant error TS and %q", name, err, out.String(), test.want)
		}
	}
}
