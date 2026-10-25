package forge_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/web"
)

func envLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

// handedOut holds every address freeAddress returned in this process.
var handedOut = struct {
	sync.Mutex
	addresses map[string]bool
}{addresses: map[string]bool{}}

// freeAddress returns a loopback address that was free a moment ago and
// that no other test of this process was given: the kernel may hand a
// just-closed port to the next caller, and two parallel tests sharing one
// would talk to each other's server.
func freeAddress(t *testing.T) string {
	t.Helper()
	handedOut.Lock()
	defer handedOut.Unlock()
	for range 100 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		if !handedOut.addresses[address] {
			handedOut.addresses[address] = true
			return address
		}
	}
	t.Fatal("no unused loopback port in 100 tries")
	return ""
}

func TestExecuteServeHealthcheckAndGracefulShutdown(t *testing.T) {
	lookup := envLookup(map[string]string{"FORGE_ENV": "test", "FORGE_HTTP_ADDR": freeAddress(t)})
	var closedResources []string
	configure := func(_ context.Context, app *forge.App) error {
		for _, name := range []string{"first", "second"} {
			if err := app.OnShutdown(func(context.Context) error { closedResources = append(closedResources, name); return nil }); err != nil {
				return err
			}
		}
		return app.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("pong")) })
	}

	ctx, cancel := context.WithCancel(context.Background())
	var serveOutput, serveErrors bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- forge.Execute(ctx, nil, lookup, &serveOutput, &serveErrors, configure) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var stdout, stderr bytes.Buffer
		if forge.Execute(context.Background(), []string{"healthcheck"}, lookup, &stdout, &stderr, configure) == 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("server never became healthy: %s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exit code=%d stderr=%s logs=%s", code, serveErrors.String(), serveOutput.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not shut down after cancellation")
	}
	if !strings.Contains(serveOutput.String(), "application stopped") {
		t.Fatalf("missing shutdown log: %s", serveOutput.String())
	}
	if strings.Join(closedResources, ",") != "second,first" {
		t.Fatalf("shutdown hooks ran as %v, want reverse registration order", closedResources)
	}
}

func TestExecuteHealthcheckFailsWhenNothingListens(t *testing.T) {
	t.Parallel()
	lookup := envLookup(map[string]string{"FORGE_HTTP_ADDR": freeAddress(t)})
	var stdout, stderr bytes.Buffer
	if code := forge.Execute(context.Background(), []string{"healthcheck"}, lookup, &stdout, &stderr, func(context.Context, *forge.App) error { return nil }); code != 1 {
		t.Fatalf("code=%d want 1", code)
	}
}

func TestExecuteRejectsInvalidInvocations(t *testing.T) {
	t.Parallel()
	noop := func(context.Context, *forge.App) error { return nil }
	tests := []struct {
		name      string
		args      []string
		env       map[string]string
		configure forge.Configure
		wantCode  int
	}{
		{name: "unknown command", args: []string{"migrate"}, configure: noop, wantCode: 2},
		{name: "extra arguments", args: []string{"serve", "now"}, configure: noop, wantCode: 2},
		{name: "insecure production", env: map[string]string{"FORGE_ENV": "production"}, configure: noop, wantCode: 1},
		{name: "configure failure", env: map[string]string{"FORGE_HTTP_ADDR": "127.0.0.1:0"}, configure: func(context.Context, *forge.App) error { return errors.New("boom") }, wantCode: 1},
		{name: "nil configure", wantCode: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if code := forge.Execute(context.Background(), test.args, envLookup(test.env), &stdout, &stderr, test.configure); code != test.wantCode {
				t.Fatalf("code=%d want=%d stderr=%s", code, test.wantCode, stderr.String())
			}
		})
	}
}

func TestExecuteRunsExtraCommands(t *testing.T) {
	t.Parallel()
	noop := func(context.Context, *forge.App) error { return nil }
	var gotArgs []string
	var gotEnv forge.CommandEnv
	migrate := forge.Command{Name: "migrate", Summary: "apply migrations", Run: func(_ context.Context, env forge.CommandEnv, args []string) error {
		gotArgs, gotEnv = args, env
		return nil
	}}
	failing := forge.Command{Name: "fail", Run: func(context.Context, forge.CommandEnv, []string) error { return errors.New("nope") }}
	misused := forge.Command{Name: "misused", Run: func(context.Context, forge.CommandEnv, []string) error {
		return fmt.Errorf("%w: misused takes no flags", forge.ErrUsage)
	}}
	lookup := envLookup(map[string]string{"FORGE_ENV": "test"})

	var stdout, stderr bytes.Buffer
	if code := forge.Execute(context.Background(), []string{"migrate", "status"}, lookup, &stdout, &stderr, noop, migrate, failing); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if strings.Join(gotArgs, " ") != "status" || gotEnv.Config.Environment != "test" || gotEnv.Logger == nil || gotEnv.Stdout != &stdout {
		t.Fatalf("args=%v env=%+v", gotArgs, gotEnv)
	}
	if code := forge.Execute(context.Background(), []string{"fail"}, lookup, &stdout, &stderr, noop, migrate, failing); code != 1 {
		t.Fatalf("failing command code=%d", code)
	}
	if code := forge.Execute(context.Background(), []string{"misused"}, lookup, &stdout, &stderr, noop, misused); code != 2 {
		t.Fatalf("usage error code=%d want 2", code)
	}
	stdout.Reset()
	if code := forge.Execute(context.Background(), []string{"help"}, lookup, &stdout, &stderr, noop, migrate); code != 0 || !strings.Contains(stdout.String(), "apply migrations") {
		t.Fatalf("help code=%d output=%s", code, stdout.String())
	}
	for name, commands := range map[string][]forge.Command{
		"reserved":  {{Name: "serve", Run: migrate.Run}},
		"duplicate": {migrate, migrate},
		"no run":    {{Name: "x"}},
	} {
		if code := forge.Execute(context.Background(), []string{"migrate"}, lookup, &stdout, &stderr, noop, commands...); code != 2 {
			t.Errorf("%s: code=%d want 2", name, code)
		}
	}
}

func TestConfigureFailureReleasesAcquiredResources(t *testing.T) {
	t.Parallel()
	released := false
	configure := func(_ context.Context, app *forge.App) error {
		if err := app.OnShutdown(func(context.Context) error { released = true; return nil }); err != nil {
			return err
		}
		return errors.New("route registration failed")
	}
	var stdout, stderr bytes.Buffer
	if code := forge.Execute(context.Background(), nil, envLookup(map[string]string{"FORGE_ENV": "test"}), &stdout, &stderr, configure); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if !released {
		t.Fatal("shutdown hooks did not run after Configure failed")
	}
}

// TestShutdownBudgetCancelsStuckRequests is a regression test: when draining
// timed out, handlers kept running with live contexts, and shutdown hooks got
// a second full timeout on top of the drain.
func TestShutdownBudgetCancelsStuckRequests(t *testing.T) {
	cfg := config.Default()
	cfg.Environment = config.Test
	cfg.HTTP.Address = freeAddress(t)
	cfg.HTTP.ShutdownTimeout = 200 * time.Millisecond
	app, err := forge.New(forge.WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	handlerCanceled := make(chan struct{})
	if err := app.HandleFunc("GET /stuck", func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(handlerCanceled)
	}); err != nil {
		t.Fatal(err)
	}
	hookDeadline := make(chan time.Time, 1)
	if err := app.OnShutdown(func(ctx context.Context) error {
		deadline, _ := ctx.Deadline()
		hookDeadline <- deadline
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	go func() {
		for {
			response, err := http.Get("http://" + cfg.HTTP.Address + "/stuck")
			if err == nil {
				_ = response.Body.Close()
				return
			}
			select {
			case <-entered:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the handler")
	}

	stoppedAt := time.Now()
	cancel()
	select {
	case <-handlerCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("stuck handler's context was never canceled after the drain deadline")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run must report the missed drain deadline")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
	if deadline := <-hookDeadline; deadline.After(stoppedAt.Add(cfg.HTTP.ShutdownTimeout + 100*time.Millisecond)) {
		t.Fatalf("hooks got a deadline %v after stop; drain and hooks must share one budget", deadline.Sub(stoppedAt))
	}
}

func TestWithHTTPMiddlewareWrapsApplicationHandler(t *testing.T) {
	t.Parallel()
	var requestID string
	middleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID = web.RequestID(r.Context())
			next.ServeHTTP(w, r)
		})
	}
	app, err := forge.New(forge.WithHTTPMiddleware(middleware))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.HandleFunc("GET /middleware", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/middleware", nil))
	requestIDMatches := recorder.Header().Get("X-Request-ID") == requestID
	if recorder.Code != http.StatusNoContent || requestID == "" || !requestIDMatches {
		t.Fatalf("status=%d request_id=%q response_request_id=%q", recorder.Code, requestID, recorder.Header().Get("X-Request-ID"))
	}
}

func TestWithHTTPMiddlewareRejectsNil(t *testing.T) {
	t.Parallel()
	if _, err := forge.New(forge.WithHTTPMiddleware(nil)); err == nil {
		t.Fatal("nil HTTP middleware was accepted")
	}
}
