package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/devloop"
)

func TestRunDevRejectsWhatCannotRun(t *testing.T) {
	// Not parallel: the working directory.
	var out, errOut strings.Builder
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"bad flag":          {[]string{"--bogus"}, "flag provided but not defined"},
		"poll too small":    {[]string{"--poll-interval", "1ms"}, "--poll-interval must be between"},
		"debounce negative": {[]string{"--debounce=-1s"}, "--debounce must be between"},
		"two names":         {[]string{"a", "b"}, "usage: forge dev"},
		"name twice":        {[]string{"a", "--no-reload", "b"}, "usage: forge dev"},
	} {
		if err := runDev(tc.args, &out, &errOut); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	t.Chdir(t.TempDir())
	if err := runDev(nil, &out, &errOut); err == nil || !strings.Contains(err.Error(), "no go.mod here") {
		t.Errorf("outside an application: %v", err)
	}
	root := fakeProject(t, "shop", echoApp)
	t.Chdir(root)
	if err := runDev([]string{"nope"}, &out, &errOut); err == nil || !strings.Contains(err.Error(), "cmd/nope/main.go not found") {
		t.Errorf("an unknown command: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env.development"), []byte("not valid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runDev(nil, &out, &errOut); err == nil || !strings.Contains(err.Error(), ".env.development") {
		t.Errorf("a bad environment file: %v", err)
	}
}

func TestRunDevWithoutReloadRunsTheApplicationOnce(t *testing.T) {
	root := fakeProject(t, "shop", echoApp)
	t.Chdir(root)
	var out, errOut strings.Builder
	if err := runDev([]string{"shop", "--no-reload"}, &out, &errOut); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "args:  env: development") {
		t.Errorf("application output %q", out.String())
	}
	// The name may follow the flags, and one command needs no name.
	out.Reset()
	if err := runDev([]string{"--no-reload"}, &out, &errOut); err != nil || !strings.HasPrefix(out.String(), "args:") {
		t.Errorf("without a name: %v, %q", err, out.String())
	}
}

func TestDevLoopReportsWhatPreventsWatching(t *testing.T) {
	// Not parallel: it changes TMPDIR.
	var out, errOut strings.Builder
	// Outside development there is no reload proxy; only the watch fails.
	env := []string{"FORGE_ENV=staging"}
	err := devLoop(context.Background(), filepath.Join(t.TempDir(), "missing"), "shop", env, devOptions{pollInterval: time.Second}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "watch ") {
		t.Errorf("a missing source tree: %v", err)
	}
	if !strings.Contains(errOut.String(), "browser reload is off") {
		t.Errorf("the log lacks the reload notice: %q", errOut.String())
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does-not-exist"))
	if err := devLoop(context.Background(), t.TempDir(), "shop", env, devOptions{pollInterval: time.Second}, &out, &errOut); err == nil {
		t.Error("without a temporary directory for the builds devLoop must fail")
	}
}

func TestStartReloadProxyNeedsItsAddress(t *testing.T) {
	t.Parallel()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	logf := func(string, ...any) {}
	proxy, listener, err := startReloadProxy([]string{"FORGE_ENV=development", "FORGE_HTTP_ADDR=" + taken.Addr().String()}, logf)
	if err == nil || proxy != nil || listener != nil || !strings.Contains(err.Error(), "listen on "+taken.Addr().String()+" (FORGE_HTTP_ADDR)") {
		t.Errorf("an address in use: %v", err)
	}
	proxy, listener, err = startReloadProxy([]string{"FORGE_ENV=development", "FORGE_HTTP_ADDR=127.0.0.1:0"}, logf)
	if err != nil || proxy == nil || listener == nil {
		t.Fatalf("proxy %v listener %v err %v", proxy, listener, err)
	}
	_ = listener.Close()
	proxy.Close()
}

// --- the application as the runner sees it ---

type devInstance struct{ done chan struct{} }

func (d devInstance) Done() <-chan struct{}  { return d.done }
func (devInstance) Stop(time.Duration) error { return nil }

func newDevApp(t *testing.T, root string) (*devApp, *devloop.Proxy) {
	t.Helper()
	proxy, err := devloop.NewProxy(devloop.ProxyOptions{Environment: "development", ShowBuildErrors: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.Close)
	return &devApp{root: root, name: "shop", proxy: proxy, stdout: &strings.Builder{}, stderr: &strings.Builder{}, logf: func(string, ...any) {}}, proxy
}

func pageOf(t *testing.T, proxy *devloop.Proxy) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	proxy.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://localhost/", nil))
	return recorder.Body.String()
}

func TestDevObserverTellsTheBrowserWhatHappened(t *testing.T) {
	t.Parallel()
	app, proxy := newDevApp(t, t.TempDir())
	obs := app.observer()

	obs.BuildFailed("main.go:3: boom")
	if page := pageOf(t, proxy); !strings.Contains(page, "The build failed") || !strings.Contains(page, "main.go:3: boom") {
		t.Errorf("before the application ran, a failed build shows the error: %s", page)
	}
	app.upstream = "127.0.0.1:1"
	obs.Started(devInstance{})
	if !app.running {
		t.Error("Started must mark the application running")
	}
	obs.BuildFailed("main.go:4: still broken") // a running application keeps serving
	obs.Stopping()
	if app.running {
		t.Error("Stopping must clear running")
	}
	if page := pageOf(t, proxy); !strings.Contains(page, "Restarting the application") {
		t.Errorf("Stopping: %s", page)
	}
	obs.Crashed()
	if page := pageOf(t, proxy); !strings.Contains(page, "The application exited") {
		t.Errorf("Crashed: %s", page)
	}
	app.running = true
	obs.StartFailed(errors.New("boom"))
	if app.running {
		t.Error("StartFailed must clear running")
	}
	if page := pageOf(t, proxy); !strings.Contains(page, "The application did not start") {
		t.Errorf("StartFailed: %s", page)
	}

	// Without a proxy the observer only tracks whether the application runs.
	plain := &devApp{}
	plainObs := plain.observer()
	plainObs.Started(devInstance{})
	if !plain.running || plainObs.Crashed != nil || plainObs.BuildFailed != nil {
		t.Errorf("plain observer: running %v", plain.running)
	}
	plainObs.Stopping()
	if plain.running {
		t.Error("Stopping must clear running without a proxy too")
	}
}

func TestDevAppReadyWaitsForTheApplicationToListen(t *testing.T) {
	t.Parallel()
	if err := (&devApp{}).ready(context.Background(), devInstance{}); err != nil {
		t.Errorf("without a proxy there is nothing to wait for: %v", err)
	}
	app, _ := newDevApp(t, t.TempDir())
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	app.upstream = l.Addr().String()
	if err := app.ready(context.Background(), devInstance{done: make(chan struct{})}); err != nil {
		t.Errorf("a listening application: %v", err)
	}
	_ = l.Close()

	app.upstream = "127.0.0.1:1" // nothing listens there
	exited := make(chan struct{})
	close(exited)
	if err := app.ready(context.Background(), devInstance{done: exited}); err == nil || !strings.Contains(err.Error(), "exited before it served") {
		t.Errorf("an application that exited: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := app.ready(ctx, devInstance{done: make(chan struct{})}); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait: %v", err)
	}
}

func TestDevAppBuildAndStart(t *testing.T) {
	t.Parallel()
	root := fakeProject(t, "shop", echoApp)
	app, _ := newDevApp(t, root)
	app.proxy = nil
	output := filepath.Join(t.TempDir(), "shop")
	if err := app.build(context.Background(), output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := app.build(ctx, filepath.Join(t.TempDir(), "x")); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled build: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "cmd", "shop", "main.go"), []byte("package main\n\nfunc main() { oops() }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var build *devloop.BuildError
	if err := app.build(context.Background(), filepath.Join(t.TempDir(), "y")); !errors.As(err, &build) || !strings.Contains(build.Output, "undefined: oops") {
		t.Errorf("a compile error: %v", err)
	}

	// start reads the environment files again, so a broken one is reported.
	if err := os.WriteFile(filepath.Join(root, ".env.development"), []byte("not valid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := app.start(context.Background(), output); err == nil || !strings.Contains(err.Error(), ".env.development") {
		t.Errorf("start with a broken environment file: %v", err)
	}
	if err := os.Remove(filepath.Join(root, ".env.development")); err != nil {
		t.Fatal(err)
	}
	withProxy, _ := newDevApp(t, root)
	instance, err := withProxy.start(context.Background(), output)
	if err != nil {
		t.Fatal(err)
	}
	<-instance.Done()
	if !strings.HasPrefix(withProxy.upstream, "127.0.0.1:") {
		t.Errorf("the application got the private address %q", withProxy.upstream)
	}
}

func TestCappedBufferKeepsTheStartAndNeverFailsAWrite(t *testing.T) {
	t.Parallel()
	var buffer cappedBuffer
	chunk := strings.Repeat("x", buildOutputLimit-10)
	if n, err := buffer.Write([]byte(chunk)); n != len(chunk) || err != nil {
		t.Fatalf("first write: %d %v", n, err)
	}
	if got := buffer.String(); got != chunk {
		t.Error("output below the limit must be returned as written")
	}
	more := strings.Repeat("y", 100)
	if n, err := buffer.Write([]byte(more)); n != len(more) || err != nil {
		t.Fatalf("a write past the limit must claim success: %d %v", n, err)
	}
	if n, err := buffer.Write([]byte("z")); n != 1 || err != nil {
		t.Fatalf("a write after the limit: %d %v", n, err)
	}
	got := buffer.String()
	if !strings.HasSuffix(got, strings.Repeat("y", 10)+"\n... output truncated") || strings.Contains(got, "z") {
		t.Errorf("truncated output ends with %q", got[len(got)-40:])
	}
}
