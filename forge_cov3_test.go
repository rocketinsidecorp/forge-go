package forge_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/openapi"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.LoadWithLookup(envLookup(map[string]string{"FORGE_ENV": "test", "FORGE_HTTP_ADDR": anyAddress}))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A description that cannot be built is a startup error, not a 500 on the
// first request to /openapi.json, and the shutdown hooks still run.
func TestRunRefusesToStartWithAnInaccurateOpenAPIDescription(t *testing.T) {
	t.Parallel()
	app, err := forge.New(forge.WithConfig(testConfig(t)), forge.WithTelemetry(nil))
	if err != nil {
		t.Fatal(err)
	}
	var closed atomic.Bool
	if err := app.OnShutdown(func(context.Context) error { closed.Store(true); return nil }); err != nil {
		t.Fatal(err)
	}
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	// An operation that declares no response cannot be described.
	if err := app.Handle("GET /a", openapi.Describe(ok, openapi.Operation{ID: "noResponse", Summary: "one"})); err != nil {
		t.Fatal(err)
	}
	if _, err := app.OpenAPI(); err == nil {
		t.Fatal("an operation without a response produced a document")
	}
	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "GET /a") {
			t.Fatalf("Run = %v, want the route that cannot be described", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run kept serving an application it could not describe")
	}
	if !closed.Load() {
		t.Error("the shutdown hooks did not run after the failed start")
	}
}

func TestRunFailsWhenTheMetricsAddressIsTaken(t *testing.T) {
	t.Parallel()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	cfg := testConfig(t)
	cfg.Telemetry.MetricsAddress = taken.Addr().String()
	app, err := forge.New(forge.WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var closed atomic.Bool
	if err := app.OnShutdown(func(context.Context) error { closed.Store(true); return nil }); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "listen for metrics") {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not report the unavailable metrics address")
	}
	if !closed.Load() {
		t.Error("the shutdown hooks did not run")
	}
}

func TestServeReleasesWhatConfigureAcquiredWhenItFails(t *testing.T) {
	t.Parallel()
	lookup := envLookup(map[string]string{"FORGE_ENV": "test", "FORGE_HTTP_ADDR": anyAddress})
	var stdout, stderr bytes.Buffer
	var released atomic.Bool
	code := forge.Execute(context.Background(), nil, lookup, &stdout, &stderr, func(_ context.Context, app *forge.App) error {
		if err := app.OnShutdown(func(context.Context) error { released.Store(true); return errors.New("pool close failed") }); err != nil {
			return err
		}
		return errors.New("no database")
	})
	if code != 1 || !released.Load() {
		t.Fatalf("code=%d released=%v", code, released.Load())
	}
	logs := stdout.String()
	for _, want := range []string{"configure application", "no database", "release resources", "pool close failed"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
}

// The probe of a TLS server skips certificate verification, since it only
// reads a status code from its own process over loopback.
func TestHealthcheckProbesATLSServer(t *testing.T) {
	t.Parallel()
	var path atomic.Value
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	lookup := envLookup(map[string]string{
		"FORGE_ENV":                "test",
		"FORGE_HTTP_ADDR":          strings.TrimPrefix(server.URL, "https://"),
		"FORGE_HTTP_TRANSPORT":     "tls",
		"FORGE_HTTP_TLS_CERT_FILE": "cert.pem",
		"FORGE_HTTP_TLS_KEY_FILE":  "key.pem",
	})
	var stderr bytes.Buffer
	if code := forge.Execute(context.Background(), []string{"healthcheck"}, lookup, &bytes.Buffer{}, &stderr, func(context.Context, *forge.App) error { return nil }); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if got, _ := path.Load().(string); got != "/health/live" {
		t.Errorf("probed %q, want /health/live", got)
	}
}

func TestOpenAPIDocumentIsBuiltOnceAndAnUnbuildableOneIsAServerError(t *testing.T) {
	t.Parallel()
	app, err := forge.New(forge.WithConfig(testConfig(t)), forge.WithTelemetry(nil))
	if err != nil {
		t.Fatal(err)
	}
	greet := openapi.Typed(http.StatusOK, openapi.Operation{ID: "greet"}, func(*http.Request, openapi.NoBody) (struct{ Hello string }, error) {
		return struct{ Hello string }{"world"}, nil
	})
	if err := app.Handle("GET /v1/greet", greet); err != nil {
		t.Fatal(err)
	}
	get := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, forge.OpenAPIPath, nil))
		return recorder
	}
	first, second := get(), get()
	if first.Code != http.StatusOK || second.Code != http.StatusOK || first.Body.String() != second.Body.String() {
		t.Fatalf("the cached document differs: %d %d", first.Code, second.Code)
	}
	if !strings.Contains(first.Body.String(), "/v1/greet") {
		t.Errorf("document lacks the route: %s", first.Body.String())
	}

	// A route whose description cannot be built fails the document with a
	// server error, never a half-document.
	broken, err := forge.New(forge.WithConfig(testConfig(t)), forge.WithTelemetry(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := broken.Handle("GET /a", openapi.Describe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), openapi.Operation{ID: "noResponse"})); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	broken.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, forge.OpenAPIPath, nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "noResponse") || strings.Contains(recorder.Body.String(), "cannot describe") {
		t.Errorf("the build failure leaked to the client: %s", recorder.Body.String())
	}
}

// runLogging runs app until the log shows want, then stops it.
func runLogging(t *testing.T, app *forge.App, logs *safeBuffer, want string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	waitFor(t, func() bool { return strings.Contains(logs.String(), want) })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestRunSaysWhenTheApplicationServesItsOwnOpenAPIDocument(t *testing.T) {
	t.Parallel()
	logs := &safeBuffer{}
	app, err := forge.New(forge.WithConfig(testConfig(t)), forge.WithTelemetry(nil), forge.WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.HandleFunc("GET "+forge.OpenAPIPath, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) }); err != nil {
		t.Fatal(err)
	}
	runLogging(t, app, logs, "Forge's OpenAPI document is not mounted there")
}

func TestRunWarnsWhenProductionServesTheOpenAPIDocument(t *testing.T) {
	t.Parallel()
	logs := &safeBuffer{}
	cfg := testConfig(t)
	cfg.Environment = config.Production
	cfg.HTTP.Transport = config.TransportTrustedProxy
	enabled := true
	cfg.OpenAPI.Enabled = &enabled
	app, err := forge.New(forge.WithConfig(cfg), forge.WithTelemetry(nil), forge.WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	runLogging(t, app, logs, "FORGE_OPENAPI_ENABLED=true enabled it")
}

func TestNewViewRendererParsesTemplatesAndRefusesBrokenOnes(t *testing.T) {
	t.Parallel()
	renderer, err := forge.NewViewRenderer(fstest.MapFS{"hello.gohtml": {Data: []byte(`<p>{{.}}</p>`)}})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	if err := renderer.Render(recorder, "hello.gohtml", http.StatusOK, "<b>x</b>"); err != nil {
		t.Fatal(err)
	}
	if got := recorder.Body.String(); got != "<p>&lt;b&gt;x&lt;/b&gt;</p>" {
		t.Errorf("rendered %q, want the value escaped", got)
	}
	if _, err := forge.NewViewRenderer(nil); err == nil {
		t.Error("a nil filesystem was accepted")
	}
	if _, err := forge.NewViewRenderer(fstest.MapFS{"broken.gohtml": {Data: []byte(`{{.Missing`)}}); err == nil {
		t.Error("a template that does not parse was accepted")
	}
}
