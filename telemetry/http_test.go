package telemetry_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rocketinsidecorp/forge-go/router"
	"github.com/rocketinsidecorp/forge-go/telemetry"
	"github.com/rocketinsidecorp/forge-go/web"
)

type spanSink struct {
	mu    sync.Mutex
	spans []telemetry.SpanData
}

func (s *spanSink) ExportSpans(_ context.Context, _ telemetry.Resource, spans []telemetry.SpanData) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spans = append(s.spans, spans...)
	return nil
}

func (s *spanSink) Shutdown(context.Context) error { return nil }

func (s *spanSink) all() []telemetry.SpanData {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]telemetry.SpanData(nil), s.spans...)
}

func newTelemetry(t *testing.T, sink *spanSink, trust bool) *telemetry.Telemetry {
	t.Helper()
	opts := telemetry.Options{ServiceName: "test", SampleRatio: 1, TrustIncoming: trust}
	if sink != nil {
		opts.SpanExporter = sink
	}
	tel, err := telemetry.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	return tel
}

func flush(t *testing.T, tel *telemetry.Telemetry) {
	t.Helper()
	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func attr(span telemetry.SpanData, key string) (telemetry.Value, bool) {
	for _, a := range span.Attributes {
		if a.Key == key {
			return a.Value, true
		}
	}
	return telemetry.Value{}, false
}

func durationSeries(t *testing.T, tel *telemetry.Telemetry) map[string]uint64 {
	t.Helper()
	series := map[string]uint64{}
	for _, metric := range tel.Metrics().Collect() {
		if metric.Name != telemetry.MetricHTTPServerDuration {
			continue
		}
		for _, p := range metric.Points {
			labels := make([]string, len(p.Labels))
			for i, label := range p.Labels {
				labels[i] = label.Value.AsString()
			}
			series[strings.Join(labels, " ")] = p.Count
		}
	}
	return series
}

func instrumentedRouter(t *testing.T, tel *telemetry.Telemetry, logs *bytes.Buffer) http.Handler {
	t.Helper()
	routes := router.New()
	if err := routes.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		web.Logger(r.Context()).Info("loading user")
		w.WriteHeader(http.StatusNoContent)
	}); err != nil {
		t.Fatal(err)
	}
	if err := routes.HandleFunc("POST /fail", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}); err != nil {
		t.Fatal(err)
	}
	middleware, err := tel.ServerMiddleware()
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	handler := middleware(routes)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(web.WithLogger(r.Context(), logger)))
	})
}

func TestServerMiddlewareTracesAndMeasuresByRoutePattern(t *testing.T) {
	sink := &spanSink{}
	tel := newTelemetry(t, sink, true)
	var logs bytes.Buffer
	handler := instrumentedRouter(t, tel, &logs)

	for _, target := range []string{"/users/1", "/users/2?token=secret", "/users/3"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
	}
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/fail", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/no/such/route/123", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("BREW", "/users/1", nil))
	flush(t, tel)

	series := durationSeries(t, tel)
	want := map[string]uint64{
		"GET /users/{id} 2xx": 3,
		"POST /fail 5xx":      1,
		"GET  4xx":            1,
		"_OTHER  4xx":         1,
	}
	for key, count := range want {
		if series[key] != count {
			t.Errorf("series %q = %d; all series %v", key, series[key], series)
		}
	}
	if len(series) != len(want) {
		t.Errorf("unexpected series: %v", series)
	}

	spans := sink.all()
	if len(spans) != 6 {
		t.Fatalf("spans = %d", len(spans))
	}
	first := spans[0]
	if first.Name != "GET /users/{id}" || first.Kind != telemetry.SpanKindServer {
		t.Fatalf("span %+v", first)
	}
	if route, _ := attr(first, "http.route"); route.AsString() != "/users/{id}" {
		t.Fatalf("http.route = %v", route.AsString())
	}
	if status, _ := attr(first, "http.response.status_code"); status.AsInt64() != 204 {
		t.Fatalf("status = %v", status.AsInt64())
	}
	failed := spans[3]
	if failed.Status != telemetry.StatusError {
		t.Fatalf("5xx span status %v", failed.Status)
	}
	if spans[4].Name != "GET" {
		t.Fatalf("unmatched request span name %q", spans[4].Name)
	}
	for _, span := range spans {
		for _, a := range span.Attributes {
			if strings.Contains(a.Value.Emit(), "secret") || strings.Contains(a.Value.Emit(), "/users/2") {
				t.Fatalf("raw path or query recorded: %+v", a)
			}
		}
	}
	if !strings.Contains(logs.String(), `"trace_id":"`+first.SpanContext.TraceID.String()+`"`) ||
		!strings.Contains(logs.String(), `"span_id":"`+first.SpanContext.SpanID.String()+`"`) {
		t.Fatalf("request logs are not correlated: %s", logs.String())
	}
}

func TestServerMiddlewareContinuesTrustedIncomingTraces(t *testing.T) {
	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	for _, trust := range []bool{true, false} {
		sink := &spanSink{}
		tel := newTelemetry(t, sink, trust)
		handler := instrumentedRouter(t, tel, &bytes.Buffer{})
		request := httptest.NewRequest(http.MethodGet, "/users/1", nil)
		request.Header.Set("traceparent", parent)
		handler.ServeHTTP(httptest.NewRecorder(), request)
		flush(t, tel)
		span := sink.all()[0]
		continued := span.SpanContext.TraceID.String() == "4bf92f3577b34da6a3ce929d0e0e4736" && span.ParentSpanID.String() == "00f067aa0ba902b7" && span.ParentRemote
		if continued != trust {
			t.Fatalf("trust=%v: continued=%v span=%+v", trust, continued, span)
		}
	}
}

func TestServerMiddlewareRecordsPanicsAndPreservesWriterCapabilities(t *testing.T) {
	sink := &spanSink{}
	tel := newTelemetry(t, sink, true)
	middleware, err := tel.ServerMiddleware()
	if err != nil {
		t.Fatal(err)
	}
	panicking := middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Fatalf("panic value %v must be re-raised unchanged", recovered)
			}
		}()
		panicking.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()

	flushed := false
	streaming := middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("Flusher lost")
		}
		flusher.Flush()
		flushed = true
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Second)); err == nil {
			t.Fatal("httptest.ResponseRecorder does not support deadlines; Unwrap must reach it")
		}
	}))
	streaming.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !flushed {
		t.Fatal("handler did not run")
	}
	flush(t, tel)
	spans := sink.all()
	if len(spans) != 2 || spans[0].Status != telemetry.StatusError {
		t.Fatalf("panicking request must end its span as an error: %+v", spans)
	}
	var active float64 = -1
	for _, metric := range tel.Metrics().Collect() {
		if metric.Name == telemetry.MetricHTTPServerActive {
			active = metric.Points[0].Value
		}
	}
	if active != 0 {
		t.Fatalf("active requests = %v after all requests finished", active)
	}
}

func TestTransportPropagatesAndRecordsClientSpans(t *testing.T) {
	sink := &spanSink{}
	tel := newTelemetry(t, sink, true)
	var received http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Clone()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	transport, err := tel.Transport(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transport}

	ctx, parent := tel.Tracer("test").Start(context.Background(), "parent", telemetry.SpanKindInternal)
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL+"/path?q=secret", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	parent.End()
	if request.Header.Get("traceparent") != "" {
		t.Fatal("the caller's request was modified")
	}
	flush(t, tel)
	spans := sink.all()
	if len(spans) != 2 {
		t.Fatalf("spans %d", len(spans))
	}
	clientSpan := spans[0]
	if clientSpan.Kind != telemetry.SpanKindClient || clientSpan.ParentSpanID != parent.SpanContext().SpanID {
		t.Fatalf("client span %+v", clientSpan)
	}
	sc, err := telemetry.ParseTraceparent(received.Get("traceparent"))
	if err != nil || sc.SpanID != clientSpan.SpanContext.SpanID || sc.TraceID != parent.SpanContext().TraceID {
		t.Fatalf("propagated %q (%v)", received.Get("traceparent"), err)
	}

	// Outside a trace: no span, no header, but the duration is measured.
	request, _ = http.NewRequest(http.MethodGet, upstream.URL, nil)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if received.Get("traceparent") != "" {
		t.Fatal("no traceparent outside a trace")
	}
	var count uint64
	for _, metric := range tel.Metrics().Collect() {
		if metric.Name == telemetry.MetricHTTPClientDuration {
			count = metric.Points[0].Count
		}
	}
	if count != 2 {
		t.Fatalf("client duration count %d", count)
	}

	disabled, err := (*telemetry.Telemetry)(nil).Transport(http.DefaultTransport)
	if err != nil || disabled != http.DefaultTransport {
		t.Fatal("a nil Telemetry must return the base transport unchanged")
	}
}

func TestStatusClassAndRoutePath(t *testing.T) {
	for status, want := range map[int]string{101: "1xx", 200: "2xx", 304: "3xx", 404: "4xx", 503: "5xx", 0: "other", 999: "other"} {
		if got := telemetry.StatusClass(status); got != want {
			t.Errorf("StatusClass(%d) = %q", status, got)
		}
	}
	for pattern, want := range map[string]string{"GET /a/{id}": "/a/{id}", "/static/": "/static/", "": ""} {
		if got := telemetry.RoutePath(pattern); got != want {
			t.Errorf("RoutePath(%q) = %q", pattern, got)
		}
	}
}

// TestRouteRecordingIsRaceFreeWithTimeoutHandler runs the router on
// http.TimeoutHandler's goroutine, which records the route after the
// middleware has already read it for a timed-out request. Run with -race.
func TestRouteRecordingIsRaceFreeWithTimeoutHandler(t *testing.T) {
	tel := newTelemetry(t, &spanSink{}, false)
	// The bubble's fake clock makes the order certain: the 5 ms timeout
	// fires and the request is answered and measured before the handler,
	// asleep for 50 ms, wakes and records its route. On a loaded runner real
	// time could let the handler finish first, and then the request would
	// not time out at all.
	synctest.Test(t, func(t *testing.T) {
		routes := router.New()
		if err := routes.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }); err != nil {
			t.Fatal(err)
		}
		routed := make(chan struct{})
		delayed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(routed)
			time.Sleep(50 * time.Millisecond) // fake-clock: the handler outlasts the 5ms timeout inside the bubble
			routes.ServeHTTP(w, r)
		})
		middleware, err := tel.ServerMiddleware()
		if err != nil {
			t.Fatal(err)
		}
		handler := middleware(http.TimeoutHandler(delayed, 5*time.Millisecond, "timeout"))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/slow", nil))
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want the timeout response", recorder.Code)
		}
		<-routed
		series := durationSeries(t, tel)
		if series["GET  5xx"] != 1 {
			t.Fatalf("a timed-out request is measured without a route: %v", series)
		}
	})
}
