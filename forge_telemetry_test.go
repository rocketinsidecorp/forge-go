package forge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/telemetry"
	"github.com/rocketinsidecorp/forge-go/web"
)

// otlpCollector records the OTLP request paths and bodies it receives.
type otlpCollector struct {
	mu     sync.Mutex
	bodies map[string][]string
}

func (c *otlpCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	if c.bodies == nil {
		c.bodies = map[string][]string{}
	}
	c.bodies[r.URL.Path] = append(c.bodies[r.URL.Path], string(body))
	c.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (c *otlpCollector) get(path string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.bodies[path], "\n")
}

func TestExecuteServesMetricsExportsTracesAndCorrelatesLogs(t *testing.T) {
	collector := &otlpCollector{}
	otlp := httptest.NewServer(collector)
	defer otlp.Close()
	appAddress, metricsAddress := freeAddress(t), freeAddress(t)
	lookup := envLookup(map[string]string{
		"FORGE_ENV":                     "test",
		"FORGE_HTTP_ADDR":               appAddress,
		"FORGE_TELEMETRY_SERVICE_NAME":  "orders",
		"FORGE_TELEMETRY_EXPORTER":      "otlp",
		"FORGE_TELEMETRY_OTLP_ENDPOINT": otlp.URL,
		"FORGE_TELEMETRY_METRICS_ADDR":  metricsAddress,
	})
	configure := func(_ context.Context, app *forge.App) error {
		if app.Telemetry() == nil {
			t.Error("telemetry must be enabled by default")
		}
		return app.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
			web.Logger(r.Context()).Info("order loaded")
			_, _ = w.Write([]byte("ok"))
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	var output safeBuffer
	done := make(chan int, 1)
	go func() { done <- forge.Execute(ctx, nil, lookup, &output, io.Discard, configure) }()

	waitFor(t, func() bool {
		response, err := http.Get("http://" + appAddress + "/orders/42?secret=1")
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	})
	response, err := http.Get("http://" + metricsAddress + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.Header.Get("Content-Type") != telemetry.PrometheusContentType ||
		!strings.Contains(string(metrics), `http_server_request_duration_seconds_count{http_request_method="GET",http_route="/orders/{id}",http_response_status_class="2xx"} 1`) {
		t.Fatalf("metrics endpoint:\n%s", metrics)
	}
	if strings.Contains(string(metrics), "/orders/42") || strings.Contains(string(metrics), "secret") {
		t.Fatal("raw paths must never become metric labels")
	}
	// The application listener does not serve metrics.
	if response, err := http.Get("http://" + appAddress + "/metrics"); err == nil {
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("application listener answered /metrics with %d", response.StatusCode)
		}
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d: %s", code, output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}

	traces := collector.get("/v1/traces")
	if !strings.Contains(traces, `"name":"GET /orders/{id}"`) || !strings.Contains(traces, `"stringValue":"orders"`) {
		t.Fatalf("traces exported at shutdown: %s", traces)
	}
	if !strings.Contains(collector.get("/v1/metrics"), "http.server.request.duration") {
		t.Fatalf("metrics not exported at shutdown: %s", collector.get("/v1/metrics"))
	}
	var exported struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID string `json:"traceId"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(traces, "\n", 2)[0]), &exported); err != nil {
		t.Fatal(err)
	}
	traceID := exported.ResourceSpans[0].ScopeSpans[0].Spans[0].TraceID
	if !strings.Contains(output.String(), "trace_id="+traceID) {
		t.Fatalf("request log not correlated with trace %s:\n%s", traceID, output.String())
	}
}

func TestWithTelemetryNilDisablesInstrumentation(t *testing.T) {
	t.Parallel()
	app, err := forge.New(forge.WithTelemetry(nil))
	if err != nil {
		t.Fatal(err)
	}
	if app.Telemetry() != nil {
		t.Fatal("WithTelemetry(nil) must disable telemetry")
	}
	var logs bytes.Buffer
	cfg := config.Default()
	cfg.Telemetry.Enabled = false
	disabled, err := forge.New(forge.WithConfig(cfg), forge.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Telemetry() != nil {
		t.Fatal("FORGE_TELEMETRY_ENABLED=false must disable telemetry")
	}
	if err := disabled.HandleFunc("GET /x", func(w http.ResponseWriter, r *http.Request) { web.Logger(r.Context()).Info("x") }); err != nil {
		t.Fatal(err)
	}
	disabled.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if strings.Contains(logs.String(), "trace_id") {
		t.Fatal("disabled telemetry must not add trace IDs")
	}
}

func TestRecoveredPanicsAreMeasuredAsServerErrors(t *testing.T) {
	t.Parallel()
	app, err := forge.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("boom") }); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", recorder.Code)
	}
	var out strings.Builder
	if err := app.Telemetry().Metrics().WritePrometheus(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `http_route="/boom",http_response_status_class="5xx"} 1`) {
		t.Fatalf("panic not measured:\n%s", out.String())
	}
	if err := app.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMetricsListenerFailureStopsRun(t *testing.T) {
	t.Parallel()
	busy := freeAddress(t)
	listener, err := net.Listen("tcp", busy)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	cfg := config.Default()
	cfg.HTTP.Address = freeAddress(t)
	cfg.Telemetry.MetricsAddress = busy
	app, err := forge.New(forge.WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	if err := app.OnShutdown(func(context.Context) error { closed = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := app.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "listen for metrics") {
		t.Fatalf("Run = %v", err)
	}
	if !closed {
		t.Fatal("shutdown hooks must run when the metrics listener cannot start")
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// safeBuffer is a bytes.Buffer safe for the concurrent writes of a logger
// and reads of a test.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestIncomingTraceContextIsIgnoredUnlessTrusted(t *testing.T) {
	t.Parallel()
	const incoming = "4bf92f3577b34da6a3ce929d0e0e4736"
	for _, trust := range []bool{false, true} {
		cfg := config.Default()
		cfg.Telemetry.TrustTraceparent = trust
		app, err := forge.New(forge.WithConfig(cfg))
		if err != nil {
			t.Fatal(err)
		}
		var seen telemetry.SpanContext
		if err := app.HandleFunc("GET /x", func(_ http.ResponseWriter, r *http.Request) {
			seen = telemetry.SpanContextFromContext(r.Context())
		}); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, "/x", nil)
		// A sampled parent would force recording past the sample ratio.
		request.Header.Set("traceparent", "00-"+incoming+"-00f067aa0ba902b7-01")
		request.Header.Set("tracestate", "evil=1")
		app.Handler().ServeHTTP(httptest.NewRecorder(), request)
		continued := seen.TraceID.String() == incoming
		if continued != trust || (!trust && seen.TraceState != "") || !seen.IsValid() {
			t.Fatalf("trust=%v: span context %+v", trust, seen)
		}
	}
}

func TestPublicMetricsAddressWithoutOptInLogsAWarning(t *testing.T) {
	t.Parallel()
	var logs safeBuffer
	cfg := config.Default()
	cfg.HTTP.Address = freeAddress(t)
	_, port, _ := net.SplitHostPort(freeAddress(t))
	cfg.Telemetry.MetricsAddress = "0.0.0.0:" + port
	app, err := forge.New(forge.WithConfig(cfg), forge.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	waitFor(t, func() bool { return strings.Contains(logs.String(), "serving metrics") })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "FORGE_TELEMETRY_METRICS_PUBLIC") {
		t.Fatalf("no warning for a non-loopback metrics listener:\n%s", logs.String())
	}
}
