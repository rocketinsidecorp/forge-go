package telemetry

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type collector struct {
	mu       sync.Mutex
	requests []collected
	status   []int // statuses to answer in order; then 200
}

type collected struct {
	path    string
	header  http.Header
	payload map[string]any
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "bad gzip", http.StatusBadRequest)
			return
		}
		body = reader
	}
	var payload map[string]any
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.requests = append(c.requests, collected{path: r.URL.Path, header: r.Header.Clone(), payload: payload})
	status := http.StatusOK
	if len(c.status) > 0 {
		status, c.status = c.status[0], c.status[1:]
	}
	c.mu.Unlock()
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "0")
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{}`))
}

func (c *collector) received() []collected {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]collected(nil), c.requests...)
}

func newCollector(t *testing.T, statuses ...int) (*collector, *httptest.Server) {
	t.Helper()
	c := &collector{status: statuses}
	server := httptest.NewServer(c)
	t.Cleanup(server.Close)
	return c, server
}

func newExporter(t *testing.T, endpoint string, opts OTLPOptions) *OTLPExporter {
	t.Helper()
	opts.Endpoint = endpoint
	if opts.InitialBackoff == 0 {
		opts.InitialBackoff = time.Millisecond
	}
	exporter, err := NewOTLPExporter(opts)
	if err != nil {
		t.Fatal(err)
	}
	return exporter
}

// path walks decoded JSON: numbers index arrays, strings index objects.
func path(t *testing.T, value any, steps ...any) any {
	t.Helper()
	for _, step := range steps {
		switch key := step.(type) {
		case string:
			object, ok := value.(map[string]any)
			if !ok {
				t.Fatalf("not an object at %v: %v", key, value)
			}
			value = object[key]
		case int:
			array, ok := value.([]any)
			if !ok || key >= len(array) {
				t.Fatalf("not an array or too short at %d: %v", key, value)
			}
			value = array[key]
		}
	}
	return value
}

func TestOTLPTraceExportJSONShape(t *testing.T) {
	c, server := newCollector(t)
	exporter := newExporter(t, server.URL+"/", OTLPOptions{Headers: map[string]string{"Authorization": "Bearer s3cr3t"}})
	tel := newTestTelemetry(t, Options{ServiceName: "billing", ServiceVersion: "1.2.3", Environment: "staging", SampleRatio: 1, SpanExporter: exporter})

	parent := ContextWithRemoteSpanContext(context.Background(), SpanContext{TraceID: TraceID{0xab, 15: 1}, SpanID: SpanID{0xcd, 7: 2}, Flags: FlagSampled, TraceState: "v=1"})
	_, span := tel.Tracer("github.com/example/scope").Start(parent, "GET /users/{id}", SpanKindServer,
		String("http.route", "/users/{id}"), Int64("big", 1<<60), Float64("ratio", 0.5), Bool("ok", true))
	span.AddEvent("retry", Int("attempt", 2))
	span.SetStatus(StatusError, "500")
	span.End()
	shutdown(t, tel)

	requests := c.received()
	if len(requests) != 1 {
		t.Fatalf("requests = %d", len(requests))
	}
	request := requests[0]
	if request.path != "/v1/traces" || request.header.Get("Content-Type") != "application/json" || request.header.Get("Authorization") != "Bearer s3cr3t" {
		t.Fatalf("request %s %v", request.path, request.header)
	}
	resource := path(t, request.payload, "resourceSpans", 0, "resource", "attributes")
	encodedResource, _ := json.Marshal(resource)
	for _, want := range []string{`"service.name"`, `"billing"`, `"service.version"`, `"deployment.environment.name"`, `"staging"`} {
		if !strings.Contains(string(encodedResource), want) {
			t.Fatalf("resource %s lacks %s", encodedResource, want)
		}
	}
	if scope := path(t, request.payload, "resourceSpans", 0, "scopeSpans", 0, "scope", "name"); scope != "github.com/example/scope" {
		t.Fatalf("scope %v", scope)
	}
	got := path(t, request.payload, "resourceSpans", 0, "scopeSpans", 0, "spans", 0).(map[string]any)
	if got["traceId"] != "ab000000000000000000000000000001" || len(got["spanId"].(string)) != 16 || got["parentSpanId"] != "cd00000000000002" {
		t.Fatalf("ids %v %v %v", got["traceId"], got["spanId"], got["parentSpanId"])
	}
	if got["traceState"] != "v=1" || got["kind"] != float64(SpanKindServer) || got["name"] != "GET /users/{id}" {
		t.Fatalf("span %v", got)
	}
	// Sampled flag, plus "parent is remote" known and set.
	if got["flags"] != float64(0x301) {
		t.Fatalf("flags %v", got["flags"])
	}
	for _, key := range []string{"startTimeUnixNano", "endTimeUnixNano"} {
		if _, ok := got[key].(string); !ok {
			t.Fatalf("%s must be a decimal string: %v", key, got[key])
		}
	}
	attrs, _ := json.Marshal(got["attributes"])
	for _, want := range []string{`{"key":"big","value":{"intValue":"1152921504606846976"}}`, `{"key":"ratio","value":{"doubleValue":0.5}}`, `{"key":"ok","value":{"boolValue":true}}`, `{"key":"http.route","value":{"stringValue":"/users/{id}"}}`} {
		if !strings.Contains(string(attrs), want) {
			t.Fatalf("attributes %s lack %s", attrs, want)
		}
	}
	if status := got["status"].(map[string]any); status["code"] != float64(StatusError) || status["message"] != "500" {
		t.Fatalf("status %v", status)
	}
	if event := path(t, got, "events", 0).(map[string]any); event["name"] != "retry" {
		t.Fatalf("event %v", event)
	}
}

func TestOTLPMetricExportJSONShape(t *testing.T) {
	c, server := newCollector(t)
	exporter := newExporter(t, server.URL, OTLPOptions{Gzip: true})
	tel := newTestTelemetry(t, Options{SampleRatio: 1, MetricExporter: exporter, MetricsInterval: time.Hour})
	counter, _ := tel.Metrics().Counter("jobs.run", "Jobs.", "{job}", "outcome")
	gauge, _ := tel.Metrics().Gauge("queue.depth", "", "{job}")
	histogram, _ := tel.Metrics().Histogram("job.duration", "", "s", []float64{1, 2}, "kind")
	counter.Add(3, "ok")
	gauge.Set(7)
	histogram.Record(1.5, "email")
	shutdown(t, tel) // final export at shutdown

	requests := c.received()
	if len(requests) != 1 || requests[0].path != "/v1/metrics" || requests[0].header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("requests %+v", requests)
	}
	metrics := path(t, requests[0].payload, "resourceMetrics", 0, "scopeMetrics", 0, "metrics").([]any)
	byName := map[string]map[string]any{}
	for _, metric := range metrics {
		m := metric.(map[string]any)
		byName[m["name"].(string)] = m
	}
	sum := byName["jobs.run"]["sum"].(map[string]any)
	if sum["isMonotonic"] != true || sum["aggregationTemporality"] != float64(2) || path(t, sum, "dataPoints", 0, "asInt") != "3" {
		t.Fatalf("sum %v", sum)
	}
	if path(t, byName["queue.depth"], "gauge", "dataPoints", 0, "asDouble") != float64(7) {
		t.Fatalf("gauge %v", byName["queue.depth"])
	}
	point := path(t, byName["job.duration"], "histogram", "dataPoints", 0).(map[string]any)
	counts, _ := json.Marshal(point["bucketCounts"])
	bounds, _ := json.Marshal(point["explicitBounds"])
	if point["count"] != "1" || point["sum"] != 1.5 || string(counts) != `["0","1","0"]` || string(bounds) != `[1,2]` {
		t.Fatalf("histogram point %v", point)
	}
}

func TestOTLPRetriesTransientFailures(t *testing.T) {
	c, server := newCollector(t, http.StatusServiceUnavailable, http.StatusTooManyRequests)
	exporter := newExporter(t, server.URL, OTLPOptions{})
	err := exporter.ExportSpans(context.Background(), Resource{}, []SpanData{{Name: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(c.received()); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestOTLPDoesNotRetryPermanentFailures(t *testing.T) {
	c, server := newCollector(t, http.StatusBadRequest)
	exporter := newExporter(t, server.URL, OTLPOptions{Headers: map[string]string{"X-Api-Key": "hunter2"}})
	err := exporter.ExportSpans(context.Background(), Resource{}, []SpanData{{Name: "x"}})
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("error leaks configuration: %v", err)
	}
	if got := len(c.received()); got != 1 {
		t.Fatalf("attempts = %d", got)
	}
}

func TestOTLPGivesUpAfterMaxAttemptsAndHonorsContext(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	exporter := newExporter(t, server.URL, OTLPOptions{MaxAttempts: 3})
	if err := exporter.ExportSpans(context.Background(), Resource{}, []SpanData{{}}); err == nil {
		t.Fatal("expected failure")
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d", attempts.Load())
	}

	slow := newExporter(t, server.URL, OTLPOptions{MaxAttempts: 100, InitialBackoff: time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := slow.ExportSpans(ctx, Resource{}, []SpanData{{}}); err == nil {
		t.Fatal("expected failure")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("retries ignored the context deadline")
	}
}

func TestOTLPDoesNotFollowRedirects(t *testing.T) {
	var leaked atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Store(true) }))
	defer elsewhere.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/v1/traces", http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	exporter := newExporter(t, redirecting.URL, OTLPOptions{Headers: map[string]string{"Authorization": "secret"}})
	if err := exporter.ExportSpans(context.Background(), Resource{}, []SpanData{{}}); err == nil {
		t.Fatal("a redirect must fail the export")
	}
	if leaked.Load() {
		t.Fatal("the exporter followed a redirect to another host")
	}
}

func TestNewOTLPExporterValidation(t *testing.T) {
	for _, endpoint := range []string{"", "ftp://x", "http://", "http://user:pw@host", "http://host?x=1", "http://host#f", "http://host:99999", "collector:4318"} {
		if _, err := NewOTLPExporter(OTLPOptions{Endpoint: endpoint}); err == nil {
			t.Errorf("endpoint %q accepted", endpoint)
		} else if len(endpoint) > len("http://") && strings.Contains(err.Error(), endpoint) {
			t.Errorf("error quotes the endpoint: %v", err)
		}
	}
	for name, value := range map[string]string{"Content-Type": "x", "Bad Name": "x", "X-Ok": "line\nbreak"} {
		if _, err := NewOTLPExporter(OTLPOptions{Endpoint: "http://collector:4318", Headers: map[string]string{name: value}}); err == nil {
			t.Errorf("header %q accepted", name)
		}
	}
	if _, err := NewOTLPExporter(OTLPOptions{Endpoint: "https://collector.example.com:4318/otlp"}); err != nil {
		t.Fatal(err)
	}
}

func TestJSONDoubleEncodesNonFiniteValues(t *testing.T) {
	encoded, err := json.Marshal(encodeAttributes([]Attribute{Float64("nan", nan())}))
	if err != nil || !strings.Contains(string(encoded), `"doubleValue":"NaN"`) {
		t.Fatalf("%s %v", encoded, err)
	}
}

func nan() float64 {
	zero := 0.0
	return zero / zero
}

func TestOTLPNetworkErrorsAreRetriedAndReported(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	endpoint := server.URL
	server.Close()
	exporter := newExporter(t, endpoint, OTLPOptions{MaxAttempts: 2})
	err := exporter.ExportMetrics(context.Background(), Resource{}, []MetricData{{Name: "m", Kind: MetricGauge, Points: []PointData{{Value: 1}}}})
	var failure *otlpError
	if !errors.As(err, &failure) || failure.cause == nil {
		t.Fatalf("err = %v", err)
	}
}
