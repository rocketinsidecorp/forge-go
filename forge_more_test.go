package forge_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
)

func TestNewRejectsBadOptionsAndConfiguration(t *testing.T) {
	t.Parallel()
	if _, err := forge.New(nil); err == nil {
		t.Error("a nil option was accepted")
	}
	if _, err := forge.New(forge.WithLogger(nil)); err == nil {
		t.Error("a nil logger was accepted")
	}
	if _, err := forge.New(forge.WithHTTPMiddleware(nil)); err == nil {
		t.Error("a nil middleware was accepted")
	}
	cfg := config.Default()
	cfg.HTTP.Address = ""
	if _, err := forge.New(forge.WithConfig(cfg)); err == nil {
		t.Error("an invalid configuration was accepted")
	}
}

func TestShutdownHooksRunOnceAndReportEveryFailure(t *testing.T) {
	t.Parallel()
	app, err := forge.New(forge.WithTelemetry(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.OnShutdown(nil); err == nil {
		t.Error("a nil hook was accepted")
	}
	var ran []string
	first, second := errors.New("first failed"), errors.New("second failed")
	for name, failure := range map[string]error{"a": first, "b": second} {
		if err := app.OnShutdown(func(context.Context) error { ran = append(ran, name); return failure }); err != nil {
			t.Fatal(err)
		}
	}
	err = app.Close(t.Context())
	if !errors.Is(err, first) || !errors.Is(err, second) || len(ran) != 2 {
		t.Fatalf("err=%v ran=%v, want both hooks run and both errors reported", err, ran)
	}
	// Close is idempotent and the application takes no hook after it.
	if err := app.Close(t.Context()); err != nil || len(ran) != 2 {
		t.Fatalf("second Close: %v ran=%v", err, ran)
	}
	if err := app.OnShutdown(func(context.Context) error { return nil }); err == nil || !strings.Contains(err.Error(), "already shut down") {
		t.Fatalf("hook after Close: %v", err)
	}
}

func TestAppExposesItsConfigurationAndRoutes(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	app, err := forge.New(forge.WithLogger(logger), forge.WithTelemetry(nil))
	if err != nil {
		t.Fatal(err)
	}
	if app.Config().HTTP.Address == "" || app.Logger() == nil || app.Telemetry() != nil {
		t.Errorf("config=%+v telemetry=%v", app.Config().HTTP, app.Telemetry())
	}
	if err := app.HandleFunc("GET /things", func(http.ResponseWriter, *http.Request) {}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, route := range app.Routes() {
		found = found || strings.Contains(route.Pattern, "/things")
	}
	if !found {
		t.Errorf("routes = %+v", app.Routes())
	}
}

func TestExecuteOpenAPIReportsItsFailures(t *testing.T) {
	t.Parallel()
	lookup := envLookup(map[string]string{"FORGE_ENV": "test"})
	ok := func(context.Context, *forge.App) error { return nil }

	var stderr bytes.Buffer
	if code := forge.Execute(context.Background(), []string{"openapi"}, lookup, &bytes.Buffer{}, &stderr, func(context.Context, *forge.App) error { return errors.New("no database") }); code != 1 || !strings.Contains(stderr.String(), "configure application: no database") {
		t.Errorf("configure failure: code=%d stderr=%s", code, stderr.String())
	}
	stderr.Reset()
	if code := forge.Execute(context.Background(), []string{"openapi", "--check", filepath.Join(t.TempDir(), "missing.json")}, lookup, &bytes.Buffer{}, &stderr, ok); code != 1 || !strings.Contains(stderr.String(), "read ") {
		t.Errorf("check of a missing file: code=%d stderr=%s", code, stderr.String())
	}
	stderr.Reset()
	if code := forge.Execute(context.Background(), []string{"openapi", "--output", filepath.Join(t.TempDir(), "no", "such", "dir", "api.json")}, lookup, &bytes.Buffer{}, &stderr, ok); code != 1 || !strings.Contains(stderr.String(), "write ") {
		t.Errorf("output to a missing directory: code=%d stderr=%s", code, stderr.String())
	}
	stderr.Reset()
	if code := forge.Execute(context.Background(), []string{"openapi"}, lookup, failingWriter{}, &stderr, ok); code != 1 || !strings.Contains(stderr.String(), "openapi:") {
		t.Errorf("stdout failure: code=%d stderr=%s", code, stderr.String())
	}
	stderr.Reset()
	hook := func(_ context.Context, app *forge.App) error {
		return app.OnShutdown(func(context.Context) error { return errors.New("pool close failed") })
	}
	if code := forge.Execute(context.Background(), []string{"openapi"}, lookup, &bytes.Buffer{}, &stderr, hook); code != 0 || !strings.Contains(stderr.String(), "release resources: ") {
		t.Errorf("release failure: code=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat("api.json"); err == nil {
		t.Error("the openapi command wrote a file nobody asked for")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestExecuteHealthcheckProbesTLSAndReportsBadStatus(t *testing.T) {
	t.Parallel()
	// A listener that answers 503 on the liveness path is unhealthy.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	var stderr bytes.Buffer
	lookup := envLookup(map[string]string{"FORGE_HTTP_ADDR": address, "FORGE_ENV": "test"})
	if code := forge.Execute(context.Background(), []string{"healthcheck"}, lookup, &bytes.Buffer{}, &stderr, func(context.Context, *forge.App) error { return nil }); code != 1 || !strings.Contains(stderr.String(), "503") {
		t.Errorf("code=%d stderr=%s", code, stderr.String())
	}
	// An address without a port cannot be probed.
	stderr.Reset()
	cfgLookup := envLookup(map[string]string{"FORGE_HTTP_ADDR": "localhost", "FORGE_ENV": "test"})
	if code := forge.Execute(context.Background(), []string{"healthcheck"}, cfgLookup, &bytes.Buffer{}, &stderr, func(context.Context, *forge.App) error { return nil }); code == 0 {
		t.Errorf("a portless address passed: %s", stderr.String())
	}
}
