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
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/internal/waittest"
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
	lookup := envLookup(map[string]string{
		"FORGE_ENV":                     "test",
		"FORGE_HTTP_ADDR":               anyAddress,
		"FORGE_TELEMETRY_SERVICE_NAME":  "orders",
		"FORGE_TELEMETRY_EXPORTER":      "otlp",
		"FORGE_TELEMETRY_OTLP_ENDPOINT": otlp.URL,
		// Port 0 on both: the kernel picks two distinct ports.
		"FORGE_TELEMETRY_METRICS_ADDR": anyAddress,
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

	appAddress := boundAddress(t, &output, "serving HTTP")
	metricsAddress := boundAddress(t, &output, "serving metrics")
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
	listener, err := net.Listen("tcp", anyAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	cfg := config.Default()
	cfg.HTTP.Address = anyAddress
	cfg.Telemetry.MetricsAddress = listener.Addr().String()
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
	waittest.Until(t, 5*time.Second, "the condition", condition)
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
	cfg.HTTP.Address = anyAddress
	cfg.Telemetry.MetricsAddress = "0.0.0.0:0"
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

// runWithLogs serves one request with the given extra environment, stops the
// application and returns what the collector received and what was written
// to standard output.
func runWithLogs(t *testing.T, collector http.Handler, extra map[string]string) (stdout string) {
	t.Helper()
	otlp := httptest.NewServer(collector)
	defer otlp.Close()
	env := map[string]string{
		"FORGE_ENV":                     "test",
		"FORGE_HTTP_ADDR":               anyAddress,
		"FORGE_TELEMETRY_SERVICE_NAME":  "orders",
		"FORGE_TELEMETRY_OTLP_ENDPOINT": otlp.URL,
	}
	for key, value := range extra {
		env[key] = value
	}
	configure := func(_ context.Context, app *forge.App) error {
		return app.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
			web.Logger(r.Context()).Info("order loaded", "customer", "c-1")
			_, _ = w.Write([]byte("ok"))
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	var output safeBuffer
	done := make(chan int, 1)
	go func() { done <- forge.Execute(ctx, nil, envLookup(env), &output, io.Discard, configure) }()
	appAddress := boundAddress(t, &output, "serving HTTP")
	waitFor(t, func() bool {
		response, err := http.Get("http://" + appAddress + "/orders/42")
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	})
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d: %s", code, output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	return output.String()
}

func TestExecuteExportsLogsWithTraceCorrelationWhenEnabled(t *testing.T) {
	collector := &otlpCollector{}
	output := runWithLogs(t, collector, map[string]string{
		"FORGE_TELEMETRY_EXPORTER":      "otlp",
		"FORGE_TELEMETRY_LOGS_EXPORTER": "otlp",
	})
	var traces struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID string `json:"traceId"`
					SpanID  string `json:"spanId"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(collector.get("/v1/traces"), "\n", 2)[0]), &traces); err != nil {
		t.Fatal(err)
	}
	span := traces.ResourceSpans[0].ScopeSpans[0].Spans[0]

	var logs struct {
		ResourceLogs []struct {
			Resource struct {
				Attributes []struct {
					Key   string `json:"key"`
					Value struct {
						StringValue string `json:"stringValue"`
					} `json:"value"`
				} `json:"attributes"`
			} `json:"resource"`
			ScopeLogs []struct {
				LogRecords []struct {
					Body struct {
						StringValue string `json:"stringValue"`
					} `json:"body"`
					SeverityText string `json:"severityText"`
					TraceID      string `json:"traceId"`
					SpanID       string `json:"spanId"`
					Attributes   []struct {
						Key string `json:"key"`
					} `json:"attributes"`
				} `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	found, started := false, false
	for _, body := range strings.Split(collector.get("/v1/logs"), "\n") {
		if err := json.Unmarshal([]byte(body), &logs); err != nil {
			t.Fatalf("logs payload %q: %v", body, err)
		}
		service := ""
		for _, attr := range logs.ResourceLogs[0].Resource.Attributes {
			if attr.Key == "service.name" {
				service = attr.Value.StringValue
			}
		}
		if service != "orders" {
			t.Fatalf("service.name = %q", service)
		}
		for _, record := range logs.ResourceLogs[0].ScopeLogs[0].LogRecords {
			switch record.Body.StringValue {
			case "order loaded":
				found = record.TraceID == span.TraceID && record.SpanID == span.SpanID && record.SeverityText == "INFO"
				for _, attr := range record.Attributes {
					if attr.Key == "trace_id" || attr.Key == "span_id" {
						t.Fatalf("trace identifiers must not repeat as attributes: %+v", record)
					}
				}
			case "starting HTTP server":
				started = true
			}
		}
	}
	if !found || !started {
		t.Fatalf("logs exported: %s", collector.get("/v1/logs"))
	}
	// Standard output keeps the same lines.
	if !strings.Contains(output, "order loaded") || !strings.Contains(output, "trace_id="+span.TraceID) {
		t.Fatalf("stdout lost the log line:\n%s", output)
	}
}

func TestExecuteExportsNoLogsByDefault(t *testing.T) {
	collector := &otlpCollector{}
	output := runWithLogs(t, collector, map[string]string{"FORGE_TELEMETRY_EXPORTER": "otlp"})
	if got := collector.get("/v1/logs"); got != "" {
		t.Fatalf("logs exported without opt-in: %s", got)
	}
	if !strings.Contains(output, "order loaded") {
		t.Fatalf("stdout:\n%s", output)
	}
}

// A collector that rejects every log batch must not make the application
// log about it into the queue it is draining, nor fail the process.
func TestExecuteLogExportFailureStaysOutOfTheLogQueue(t *testing.T) {
	var mu sync.Mutex
	logRequests, records := 0, 0
	collector := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/v1/logs" {
			mu.Lock()
			logRequests++
			records += strings.Count(string(body), `"severityNumber"`)
			mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	output := runWithLogs(t, collector, map[string]string{"FORGE_TELEMETRY_LOGS_EXPORTER": "otlp"})
	if !strings.Contains(output, "log export failed") {
		t.Fatalf("the failure must be reported on standard output:\n%s", output)
	}
	mu.Lock()
	defer mu.Unlock()
	// "starting HTTP server", "serving HTTP" and "order loaded" are the only
	// records queued; the failure warning must not be among them.
	if records > 3 || logRequests == 0 {
		t.Fatalf("log export requests %d, records %d", logRequests, records)
	}
}

// The production logger writes JSON; the exported record must not carry what
// that JSON leaves out, such as a URL's password.
func TestExportedLogsNeverCarryMoreThanJSONStandardOutput(t *testing.T) {
	const password = "zz-hunter2-qq"
	collector := &otlpCollector{}
	otlp := httptest.NewServer(collector)
	defer otlp.Close()
	cfg, err := config.LoadWithLookup(envLookup(map[string]string{
		"FORGE_ENV":                     "test",
		"FORGE_TELEMETRY_SERVICE_NAME":  "orders",
		"FORGE_TELEMETRY_OTLP_ENDPOINT": otlp.URL,
		"FORGE_TELEMETRY_LOGS_EXPORTER": "otlp",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	app, err := forge.New(forge.WithConfig(cfg), forge.WithLogger(slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(&stdout, nil)))))
	if err != nil {
		t.Fatal(err)
	}
	dsn, err := url.Parse("postgres://u:" + password + "@db/x")
	if err != nil {
		t.Fatal(err)
	}
	app.Logger().Info("db", "url", dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Close(ctx); err != nil {
		t.Fatal(err)
	}
	exported := collector.get("/v1/logs")
	if !strings.Contains(exported, `"key":"url"`) || !strings.Contains(stdout.String(), `"User":{}`) {
		t.Fatalf("exported %s\nstdout %s", exported, stdout.String())
	}
	if strings.Contains(exported, password) || strings.Contains(stdout.String(), password) {
		t.Fatalf("password exported: %s", exported)
	}
}

// An application that redacts with ReplaceAttr on standard output gets the
// same redaction in the export when its logger comes from
// telemetry.NewOutputHandler: forge.New hands the ReplaceAttr to the
// exporter.
func TestExportedLogsApplyTheStandardOutputReplaceAttr(t *testing.T) {
	const password = "zz-hunter2-qq"
	collector := &otlpCollector{}
	otlp := httptest.NewServer(collector)
	defer otlp.Close()
	cfg, err := config.LoadWithLookup(envLookup(map[string]string{
		"FORGE_ENV":                     "test",
		"FORGE_TELEMETRY_SERVICE_NAME":  "orders",
		"FORGE_TELEMETRY_OTLP_ENDPOINT": otlp.URL,
		"FORGE_TELEMETRY_LOGS_EXPORTER": "otlp",
	}))
	if err != nil {
		t.Fatal(err)
	}
	redact := func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == "password" {
			return slog.String(a.Key, "[REDACTED]")
		}
		return a
	}
	var stdout bytes.Buffer
	logger := slog.New(telemetry.NewOutputHandler(&stdout, telemetry.LogFormatJSON, &slog.HandlerOptions{ReplaceAttr: redact}))
	app, err := forge.New(forge.WithConfig(cfg), forge.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	app.Logger().WithGroup("login").Info("attempt", "user", "ana", "password", password)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Close(ctx); err != nil {
		t.Fatal(err)
	}
	exported := collector.get("/v1/logs")
	if !strings.Contains(exported, `"key":"login.password"`) || !strings.Contains(exported, "[REDACTED]") || !strings.Contains(stdout.String(), `"password":"[REDACTED]"`) {
		t.Fatalf("exported %s\nstdout %s", exported, stdout.String())
	}
	if strings.Contains(exported, password) || strings.Contains(stdout.String(), password) {
		t.Fatalf("password exported: %s", exported)
	}
}
