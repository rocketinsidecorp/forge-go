package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/waittest"
)

// serveApp prints "ready" and then waits to be interrupted.
const serveApp = `package main

import (
	"fmt"
	"time"
)

func main() {
	fmt.Println("ready")
	time.Sleep(time.Hour)
}
`

// inProject runs fn with the working directory set to a fake application.
// It is not parallel: the working directory belongs to the process.
func inProject(t *testing.T, source string) string {
	t.Helper()
	root := fakeProject(t, "shop", source)
	t.Chdir(root)
	return root
}

func TestAppCommandsDelegateToTheApplicationBinary(t *testing.T) {
	inProject(t, echoApp)
	for name, tc := range map[string]struct {
		run  func(out, errOut *strings.Builder) error
		want string
	}{
		"migrate":        {func(o, e *strings.Builder) error { return runMigrate(nil, o, e) }, "args: migrate env:"},
		"migrate status": {func(o, e *strings.Builder) error { return runMigrate([]string{"status"}, o, e) }, "args: migrate status env:"},
		"rollback":       {func(o, e *strings.Builder) error { return runRollback([]string{"--step", "2"}, o, e) }, "args: rollback --step 2 env:"},
		"seed":           {func(o, e *strings.Builder) error { return runSeed(nil, o, e) }, "args: seed env:"},
		"setup":          {func(o, e *strings.Builder) error { return runSetup([]string{"--password-stdin"}, o, e) }, "args: setup --password-stdin env:"},
		"openapi":        {func(o, e *strings.Builder) error { return runOpenAPI(nil, o, e) }, "args: openapi env:"},
		"openapi output": {func(o, e *strings.Builder) error { return runOpenAPI([]string{"--output", "openapi.json"}, o, e) }, "args: openapi --output openapi.json env:"},
		"openapi check":  {func(o, e *strings.Builder) error { return runOpenAPI([]string{"--check=openapi.json"}, o, e) }, "args: openapi --check=openapi.json env:"},
	} {
		var out, errOut strings.Builder
		if err := tc.run(&out, &errOut); err != nil || !strings.HasPrefix(out.String(), tc.want) {
			t.Errorf("%s: err %v, output %q (stderr %q), want %q", name, err, out.String(), errOut.String(), tc.want)
		}
	}
}

func TestAppCommandsRejectBadArgumentsBeforeBuilding(t *testing.T) {
	t.Parallel()
	var out, errOut strings.Builder
	for name, run := range map[string]func() error{
		"migrate extra":        func() error { return runMigrate([]string{"up"}, &out, &errOut) },
		"seed extra":           func() error { return runSeed([]string{"x"}, &out, &errOut) },
		"openapi unknown":      func() error { return runOpenAPI([]string{"--bogus"}, &out, &errOut) },
		"openapi missing file": func() error { return runOpenAPI([]string{"--output"}, &out, &errOut) },
		"openapi two modes":    func() error { return runOpenAPI([]string{"--output", "a", "--check", "b"}, &out, &errOut) },
		"openapi positional":   func() error { return runOpenAPI([]string{"file.json"}, &out, &errOut) },
	} {
		if err := run(); err == nil || !strings.Contains(err.Error(), "usage: forge") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Errorf("a rejected command wrote output: %q %q", out.String(), errOut.String())
	}
}

func TestAppCommandsNeedAnApplication(t *testing.T) {
	// Not parallel: the working directory.
	t.Chdir(t.TempDir())
	var out, errOut strings.Builder
	if err := runMigrate(nil, &out, &errOut); err == nil || !strings.Contains(err.Error(), "no go.mod here") {
		t.Errorf("migrate: %v", err)
	}
	if err := runTest(nil, &out, &errOut); err == nil || !strings.Contains(err.Error(), "no go.mod here") {
		t.Errorf("test: %v", err)
	}
	if err := runBuild(nil, &out, &errOut); err == nil || !strings.Contains(err.Error(), "no go.mod here") {
		t.Errorf("build: %v", err)
	}
	if err := os.WriteFile("go.mod", []byte("module example.com/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runMigrate(nil, &out, &errOut); err == nil || !strings.Contains(err.Error(), "no cmd/ directory") {
		t.Errorf("without cmd: %v", err)
	}
	if err := os.MkdirAll("cmd", 0o750); err != nil {
		t.Fatal(err)
	}
	if err := runBuild(nil, &out, &errOut); err == nil || !strings.Contains(err.Error(), "no cmd/<name>/main.go found") {
		t.Errorf("an empty cmd: %v", err)
	}
	if err := os.WriteFile(".env.development", []byte("not valid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runTest(nil, &out, &errOut); err == nil || !strings.Contains(err.Error(), ".env.development") {
		t.Errorf("test with a bad environment file: %v", err)
	}
	if err := runSeed(nil, &out, &errOut); err == nil {
		t.Error("seed with a bad environment file must fail")
	}
}

func TestRunTestPassesTheDevelopmentEnvironmentToGoTest(t *testing.T) {
	root := inProject(t, echoApp)
	if err := os.WriteFile(filepath.Join(root, "env_test.go"), []byte(`package main

import (
	"os"
	"testing"
)

func TestEnvironment(t *testing.T) {
	if os.Getenv("FORGE_TEST_MARKER") != "from-the-env-file" || os.Getenv("FORGE_ENV") != "development" {
		t.Fatalf("environment: %q %q", os.Getenv("FORGE_TEST_MARKER"), os.Getenv("FORGE_ENV"))
	}
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env.development"), []byte("FORGE_TEST_MARKER=from-the-env-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	// No arguments: ./... is tested.
	if err := runTest(nil, &out, &errOut); err != nil {
		t.Fatalf("forge test: %v\n%s%s", err, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "ok") {
		t.Errorf("go test output: %q", out.String())
	}
	out.Reset()
	if err := runTest([]string{"-run", "TestNothing", "./..."}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
}

func TestRunBuildWritesTheBinaryUnderBin(t *testing.T) {
	root := inProject(t, echoApp)
	var out, errOut strings.Builder
	if err := runBuild(nil, &out, &errOut); err != nil || out.String() != "built "+filepath.Join("bin", "shop")+"\n" {
		t.Fatalf("build: %v, %q", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "shop")); err != nil {
		t.Errorf("the binary is missing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "cmd", "shop", "main.go"), []byte("package main\n\nfunc main() { missing() }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runBuild([]string{"shop"}, &out, &errOut); err == nil || !strings.Contains(errOut.String(), "undefined: missing") {
		t.Errorf("a compile error: %v %q", err, errOut.String())
	}
}

func TestRunAppStopsTheApplicationWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	root := fakeProject(t, "shop", serveApp)
	var out lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runApp(ctx, root, "shop", nil, os.Environ(), &out, &out) }()
	if !waittest.Poll(60*time.Second, func() bool { return strings.Contains(out.String(), "ready") }) {
		t.Fatal("the application never started")
	}
	cancel()
	select {
	case err := <-done:
		// A Go program killed by the interrupt did not shut down cleanly.
		if err == nil || !strings.Contains(err.Error(), "application did not shut down cleanly") {
			t.Errorf("err = %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("runApp did not return after the context ended")
	}
}

func TestRunAppEndsQuietlyWhenTheContextEndedDuringTheBuild(t *testing.T) {
	t.Parallel()
	root := fakeProject(t, "shop", echoApp)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut strings.Builder
	if err := runApp(ctx, root, "shop", nil, os.Environ(), &out, &errOut); err != nil {
		t.Errorf("err = %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("the application ran: %q", out.String())
	}
	broken := fakeProject(t, "shop", "package main\n\nfunc main() { nope() }\n")
	if err := runApp(context.Background(), broken, "shop", nil, os.Environ(), &out, &errOut); err == nil || !strings.HasPrefix(err.Error(), "build: ") {
		t.Errorf("a build failure: %v", err)
	}
	if err := runApp(context.Background(), root, "shop", []string{"exit=5"}, os.Environ(), &out, &errOut); err == nil || err.Error() != "shop exited with status 5" {
		t.Errorf("an exit status: %v", err)
	}
}

func TestDevelopmentEnvironmentKeepsAnEnvironmentSetByAFile(t *testing.T) {
	t.Setenv("FORGE_ENV", "")
	if err := os.Unsetenv("FORGE_ENV"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env.development"), []byte("FORGE_ENV=staging\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := developmentEnvironment(root)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, "FORGE_ENV=") {
			count++
			if entry != "FORGE_ENV=staging" {
				t.Errorf("entry %q", entry)
			}
		}
	}
	if count != 1 || !hasKey(env, "FORGE_ENV") || hasKey(env, "FORGE_NOT_SET") {
		t.Errorf("FORGE_ENV entries: %d", count)
	}
	if _, err := developmentEnvironment(filepath.Join(root, "missing")); err == nil {
		t.Error("an unreadable root must fail")
	}
}
