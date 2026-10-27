package forge_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/httpserver"
	"github.com/rocketinsidecorp/forge-go/router"
	"github.com/rocketinsidecorp/forge-go/telemetry"
)

type discardExporter struct{}

func (discardExporter) ExportSpans(context.Context, telemetry.Resource, []telemetry.SpanData) error {
	return nil
}

func (discardExporter) Shutdown(context.Context) error { return nil }

func benchmarkApp(tb testing.TB, tel *telemetry.Telemetry) http.Handler {
	tb.Helper()
	// The test environment logs the request line at Debug, which the default
	// logger (Info) drops: what is compared is the telemetry cost. The
	// development environment writes the line (telemetry.AccessLog).
	cfg := config.Default()
	cfg.Environment = config.Test
	app, err := forge.New(forge.WithConfig(cfg), forge.WithTelemetry(tel))
	if err != nil {
		tb.Fatal(err)
	}
	if err := app.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }); err != nil {
		tb.Fatal(err)
	}
	return app.Handler()
}

func benchmarkTelemetry(tb testing.TB, exporter telemetry.SpanExporter) *telemetry.Telemetry {
	tb.Helper()
	tel, err := telemetry.New(telemetry.Options{ServiceName: "bench", SampleRatio: 1, SpanExporter: exporter, MaxQueueSize: 1 << 16})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	return tel
}

func serve(handler http.Handler, request *http.Request) {
	handler.ServeHTTP(discardWriter{header: http.Header{}}, request)
}

// discardWriter is a ResponseWriter that allocates nothing per request.
type discardWriter struct{ header http.Header }

func (w discardWriter) Header() http.Header       { return w.header }
func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriter) WriteHeader(int)             {}

// BenchmarkHTTPTelemetry measures the full Forge handler chain. "disabled"
// is FORGE_TELEMETRY_ENABLED=false; "baseline" is the same chain built by
// httpserver without any telemetry code, so the two must match.
func BenchmarkHTTPTelemetry(b *testing.B) {
	request := httptest.NewRequest(http.MethodGet, "/users/42", nil)
	cases := []struct {
		name    string
		handler http.Handler
	}{
		{"baseline", baselineHandler(b)},
		{"disabled", benchmarkApp(b, nil)},
		{"enabled_no_export", benchmarkApp(b, benchmarkTelemetry(b, nil))},
		{"enabled_exporting", benchmarkApp(b, benchmarkTelemetry(b, discardExporter{}))},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				serve(tc.handler, request)
			}
		})
	}
}

func baselineHandler(tb testing.TB) http.Handler {
	tb.Helper()
	routes := router.New()
	if err := routes.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }); err != nil {
		tb.Fatal(err)
	}
	// The same discarding JSON logger forge.New uses by default.
	server, err := httpserver.New(config.Default(), routes, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		tb.Fatal(err)
	}
	return server.Handler()
}

// TestDisabledTelemetryAddsNoAllocations proves the disabled path costs
// nothing: the application chain allocates exactly what a server built
// without any telemetry code allocates.
func TestDisabledTelemetryAddsNoAllocations(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/users/42", nil)
	baseline, disabled := baselineHandler(t), benchmarkApp(t, nil)
	serve(baseline, request)
	serve(disabled, request)
	want := testing.AllocsPerRun(200, func() { serve(baseline, request) })
	got := testing.AllocsPerRun(200, func() { serve(disabled, request) })
	if got > want {
		t.Fatalf("disabled telemetry allocates %v per request, baseline %v", got, want)
	}
}
