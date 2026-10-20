package telemetry

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestInstrumentsRecordAndExposePrometheusText(t *testing.T) {
	registry := NewRegistry(0)
	requests, err := registry.Counter("app.requests", "Requests served.", "{request}", "method")
	if err != nil {
		t.Fatal(err)
	}
	inflight, err := registry.Gauge("app.inflight", "In flight.", "{request}")
	if err != nil {
		t.Fatal(err)
	}
	duration, err := registry.Histogram("app.duration", "Durations with \"quotes\"\nand newline.", "s", []float64{0.1, 1}, "route")
	if err != nil {
		t.Fatal(err)
	}
	requests.Add(2, "GET")
	requests.Add(1, "POST")
	requests.Add(-5, "GET") // ignored
	inflight.Add(3)
	inflight.Add(-1)
	duration.Record(0.05, `/a"b\c`)
	duration.Record(0.1, `/a"b\c`)
	duration.Record(5, `/a"b\c`)

	var out strings.Builder
	if err := registry.WritePrometheus(&out); err != nil {
		t.Fatal(err)
	}
	want := `# HELP app_duration_seconds Durations with "quotes"\nand newline.
# TYPE app_duration_seconds histogram
app_duration_seconds_bucket{route="/a\"b\\c",le="0.1"} 2
app_duration_seconds_bucket{route="/a\"b\\c",le="1"} 2
app_duration_seconds_bucket{route="/a\"b\\c",le="+Inf"} 3
app_duration_seconds_sum{route="/a\"b\\c"} 5.15
app_duration_seconds_count{route="/a\"b\\c"} 3
# HELP app_inflight In flight.
# TYPE app_inflight gauge
app_inflight 2
# HELP app_requests_total Requests served.
# TYPE app_requests_total counter
app_requests_total{method="GET"} 2
app_requests_total{method="POST"} 1
`
	if out.String() != want {
		t.Fatalf("exposition:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestSeriesLimitFoldsNewCombinationsIntoOverflow(t *testing.T) {
	registry := NewRegistry(3)
	counter, err := registry.Counter("hits", "", "1", "path")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		counter.Add(1, fmt.Sprintf("/attacker/%d", i))
	}
	counter.Add(1, "/attacker/0") // existing series still counts normally
	counter.Add(1, "a", "b")      // wrong label count goes to overflow
	metrics := registry.Collect()
	var hits, overflows MetricData
	for _, metric := range metrics {
		switch metric.Name {
		case "hits":
			hits = metric
		case overflowMetricName:
			overflows = metric
		}
	}
	if len(hits.Points) != 4 {
		t.Fatalf("series = %d, want 3 plus overflow", len(hits.Points))
	}
	last := hits.Points[len(hits.Points)-1]
	if last.Labels[0].Value.AsString() != OverflowLabelValue && hits.Points[0].Labels[0].Value.AsString() != OverflowLabelValue {
		t.Fatalf("no overflow series: %+v", hits.Points)
	}
	var total float64
	for _, p := range hits.Points {
		total += p.Value
	}
	if total != 102 {
		t.Fatalf("total = %v, no measurement may be lost", total)
	}
	if overflows.Points[0].Value != 98 {
		t.Fatalf("overflow count = %v", overflows.Points[0].Value)
	}
}

func TestLongLabelValuesAreTruncated(t *testing.T) {
	registry := NewRegistry(0)
	counter, _ := registry.Counter("c", "", "1", "l")
	counter.Add(1, strings.Repeat("x", 1000))
	value := registry.Collect()[0].Points[0].Labels[0].Value.AsString()
	if len(value) != MaxLabelValueLength {
		t.Fatalf("label value length %d", len(value))
	}
}

func TestRegistrationRules(t *testing.T) {
	registry := NewRegistry(0)
	first, err := registry.Histogram("db.duration", "", "s", nil, "system")
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Histogram("db.duration", "other text", "s", nil, "system")
	if err != nil || second.instrument != first.instrument {
		t.Fatal("an identical definition must return the shared instrument")
	}
	for name, register := range map[string]func() error{
		"conflicting labels": func() error { _, err := registry.Histogram("db.duration", "", "s", nil, "other"); return err },
		"conflicting kind":   func() error { _, err := registry.Counter("db.duration", "", "s", "system"); return err },
		"bad name":           func() error { _, err := registry.Counter("1bad name", "", "1"); return err },
		"bad label":          func() error { _, err := registry.Counter("ok", "", "1", "bad-label"); return err },
		"duplicate label":    func() error { _, err := registry.Counter("ok", "", "1", "a", "a"); return err },
		"reserved le":        func() error { _, err := registry.Histogram("ok", "", "s", nil, "le"); return err },
		"too many labels":    func() error { _, err := registry.Counter("ok", "", "1", "a", "b", "c", "d", "e", "f", "g"); return err },
		"unsorted bounds":    func() error { _, err := registry.Histogram("ok", "", "s", []float64{1, 0.5}); return err },
	} {
		if register() == nil {
			t.Errorf("%s: registration succeeded", name)
		}
	}
}

func TestRecordingAnExistingSeriesDoesNotAllocate(t *testing.T) {
	registry := NewRegistry(0)
	histogram, _ := registry.Histogram("h", "", "s", nil, "method", "route", "status")
	gauge, _ := registry.Gauge("g", "", "1", "method")
	histogram.Record(0.1, "GET", "/users/{id}", "2xx")
	gauge.Add(1, "GET")
	allocs := testing.AllocsPerRun(1000, func() {
		histogram.Record(0.2, "GET", "/users/{id}", "2xx")
		gauge.Add(1, "GET")
		gauge.Add(-1, "GET")
	})
	if allocs != 0 {
		t.Fatalf("recording allocated %v times", allocs)
	}
}

func TestConcurrentRecording(t *testing.T) {
	registry := NewRegistry(0)
	histogram, _ := registry.Histogram("h", "", "s", nil, "k")
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Go(func() {
			for i := range 1000 {
				histogram.Record(float64(i%10)/10, fmt.Sprint(worker%2))
			}
		})
	}
	wg.Wait()
	var count uint64
	for _, p := range registry.Collect()[0].Points {
		count += p.Count
	}
	if count != 8000 {
		t.Fatalf("count = %d", count)
	}
}

func TestPrometheusHandler(t *testing.T) {
	registry := NewRegistry(0)
	counter, _ := registry.Counter("x", "", "1")
	counter.Add(1)
	handler := registry.PrometheusHandler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != PrometheusContentType || !strings.Contains(response.Body.String(), "x_total 1") {
		t.Fatalf("GET: %d %q %q", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/metrics", nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %d", head.Code, head.Body.Len())
	}
	post := httptest.NewRecorder()
	handler.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", post.Code)
	}
}

func TestPrometheusName(t *testing.T) {
	for _, tc := range []struct {
		name, unit string
		kind       MetricKind
		want       string
	}{
		{"http.server.request.duration", "s", MetricHistogram, "http_server_request_duration_seconds"},
		{"http.server.active_requests", "{request}", MetricGauge, "http_server_active_requests"},
		{"forge.telemetry.spans.dropped", "{span}", MetricCounter, "forge_telemetry_spans_dropped_total"},
		{"jobs_total", "1", MetricCounter, "jobs_total"},
		{"size", "By", MetricGauge, "size_bytes"},
	} {
		if got := PrometheusName(tc.name, tc.unit, tc.kind); got != tc.want {
			t.Errorf("PrometheusName(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
