package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/metrics"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rocketinsidecorp/forge-go/config"
)

func TestValueAccessorsReturnZeroForOtherKinds(t *testing.T) {
	t.Parallel()
	text := String("k", "v").Value
	number := Int64("k", -7).Value
	ratio := Float64("k", 2.5).Value
	flag := Bool("k", true).Value
	if text.AsInt64() != 0 || text.AsFloat64() != 0 || number.AsFloat64() != 0 || ratio.AsInt64() != 0 || ratio.AsBool() || text.AsBool() {
		t.Fatal("an accessor of the wrong kind must return the zero value")
	}
	for name, tc := range map[string]struct {
		value Value
		want  string
	}{
		"string": {text, "v"},
		"int":    {number, "-7"},
		"float":  {ratio, "2.5"},
		"bool":   {flag, "true"},
		"zero":   {Value{}, ""},
	} {
		if got := tc.value.Emit(); got != tc.want {
			t.Errorf("%s: Emit = %q, want %q", name, got, tc.want)
		}
	}
}

func TestTruncateNeverSplitsAMultiByteCharacter(t *testing.T) {
	t.Parallel()
	// The euro sign is three bytes and straddles the 1024-byte limit.
	value := strings.Repeat("a", MaxAttributeValueLength-1) + "€" + "tail"
	got := String("k", value).Value.AsString()
	if got != strings.Repeat("a", MaxAttributeValueLength-1) || !utf8.ValidString(got) {
		t.Fatalf("got %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
	// Invalid UTF-8 is replaced, whether the value was cut or not.
	if got := String("k", "a\xffb").Value.AsString(); got != "a\uFFFDb" {
		t.Errorf("invalid UTF-8: %q", got)
	}
	if got := String("k", strings.Repeat("\xff", 2*MaxAttributeValueLength)).Value.AsString(); !utf8.ValidString(got) {
		t.Errorf("a cut invalid value must be valid UTF-8")
	}
}

func TestKindAndStatusNames(t *testing.T) {
	t.Parallel()
	for kind, want := range map[SpanKind]string{
		SpanKindInternal: "internal", SpanKindServer: "server", SpanKindClient: "client",
		SpanKindProducer: "producer", SpanKindConsumer: "consumer", SpanKind(0): "internal",
	} {
		if got := kind.String(); got != want {
			t.Errorf("SpanKind(%d) = %q, want %q", kind, got, want)
		}
	}
	for code, want := range map[StatusCode]string{StatusUnset: "unset", StatusOK: "ok", StatusError: "error"} {
		if got := code.String(); got != want {
			t.Errorf("StatusCode(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestNilTracerAndSpanAreInert(t *testing.T) {
	t.Parallel()
	var tracer *Tracer
	ctx, span := tracer.StartAt(context.Background(), time.Now(), "op", SpanKindInternal)
	if span != nil || ctx != context.Background() {
		t.Fatal("a nil Tracer must start no span")
	}
	if (*Span)(nil).SpanContext() != (SpanContext{}) {
		t.Fatal("a nil span has no identity")
	}
}

func TestSpansIgnoreChangesAfterEndAndCapEventAttributes(t *testing.T) {
	t.Parallel()
	exporter := &recordingExporter{}
	tel := newTestTelemetry(t, Options{SampleRatio: 1, SpanExporter: exporter})
	_, span := tel.Tracer("x").Start(context.Background(), "op", SpanKindInternal)

	many := make([]Attribute, MaxSpanAttributes+5)
	for i := range many {
		many[i] = Int("n", i)
	}
	span.AddEvent("wide", many...)
	span.SetStatus(StatusOK, "")
	span.SetStatus(StatusError, "ignored: an OK status is final")
	span.End()
	span.AddEvent("late")
	span.SetStatus(StatusError, "late")
	shutdown(t, tel)

	spans := exporter.exported()
	if len(spans) != 1 {
		t.Fatalf("spans: %d", len(spans))
	}
	if len(spans[0].Events) != 1 || spans[0].Events[0].Name != "wide" || len(spans[0].Events[0].Attributes) != MaxSpanAttributes {
		t.Errorf("events = %+v", spans[0].Events)
	}
	if spans[0].Status != StatusOK {
		t.Errorf("status = %v, want ok", spans[0].Status)
	}
}

func TestSpanContextHelpersTolerateNilsAndInvalidInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if ContextWithSpan(ctx, nil) != ctx {
		t.Error("a nil span leaves ctx unchanged")
	}
	if ContextWithRemoteSpanContext(ctx, SpanContext{}) != ctx {
		t.Error("an invalid span context leaves ctx unchanged")
	}
	var nilCtx context.Context
	if SpanFromContext(nilCtx) != nil || SpanContextFromContext(nilCtx) != (SpanContext{}) {
		t.Error("a nil context carries no span")
	}

	tel := newTestTelemetry(t, Options{SampleRatio: 1})
	_, span := tel.Tracer("x").Start(ctx, "op", SpanKindInternal)
	withSpan := ContextWithSpan(ctx, span)
	if SpanFromContext(withSpan) != span || SpanContextFromContext(withSpan) != span.SpanContext() {
		t.Error("ContextWithSpan does not carry the span")
	}
	remote := span.SpanContext()
	got := SpanContextFromContext(ContextWithRemoteSpanContext(ctx, remote))
	if !got.Remote || got.TraceID != remote.TraceID || got.SpanID != remote.SpanID {
		t.Errorf("remote parent = %+v", got)
	}
}

func TestPropagationRejectsWhatTheSpecForbids(t *testing.T) {
	t.Parallel()
	if got := FormatTraceparent(SpanContext{}); got != "" {
		t.Errorf("an invalid span context formats as %q", got)
	}
	var members []string
	for i := range 20 {
		members = append(members, "k"+string(rune('a'+i))+"="+strings.Repeat("v", 30))
	}
	long := strings.Join(members, ",") // 20 members of 34 characters: valid parts, over 512 in all
	for name, value := range map[string]string{
		"over the raw limit":        strings.Repeat("a", 2*maxTracestateLength+1),
		"over the normalized limit": long,
		"bad key character":         "a$b=1",
		"bad multi tenant key":      "TENANT@system=1",
		"bad system id":             "tenant@=1",
		"no equals":                 "novalue",
		"duplicate key":             "a=1,a=2",
	} {
		if got, ok := NormalizeTracestate(value); ok || got != "" {
			t.Errorf("%s: accepted %q", name, got)
		}
	}
	if got, ok := NormalizeTracestate(" a=1 , , b@sys=2 "); !ok || got != "a=1,b@sys=2" {
		t.Errorf("normalization: %q %v", got, ok)
	}
}

func TestFromConfig(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		cfg := config.Default()
		cfg.Telemetry.Enabled = false
		tel, err := FromConfig(cfg, nil)
		if tel != nil || err != nil {
			t.Fatalf("tel %v err %v", tel, err)
		}
	})
	t.Run("invalid configuration", func(t *testing.T) {
		cfg := config.Default()
		cfg.Telemetry.SampleRatio = 2
		if tel, err := FromConfig(cfg, nil); tel != nil || err == nil || !strings.Contains(err.Error(), "SAMPLE_RATIO") {
			t.Fatalf("tel %v err %v", tel, err)
		}
	})
	t.Run("service name defaults to the executable", func(t *testing.T) {
		cfg := config.Default()
		cfg.Telemetry.ServiceName = ""
		tel, err := FromConfig(cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer shutdown(t, tel)
		want := "unknown_service:" + filepath.Base(os.Args[0])
		var got string
		for _, attr := range tel.Resource().Attributes {
			if attr.Key == "service.name" {
				got = attr.Value.AsString()
			}
		}
		if got != want {
			t.Errorf("service.name = %q, want %q", got, want)
		}
	})
	t.Run("log exporter writes spans to the logger", func(t *testing.T) {
		cfg := config.Default()
		cfg.Telemetry.ServiceName = "svc"
		cfg.Telemetry.Exporter = config.TelemetryExporterLog
		var out strings.Builder
		tel, err := FromConfig(cfg, newBufferLogger(&out))
		if err != nil {
			t.Fatal(err)
		}
		_, span := tel.Tracer("x").Start(context.Background(), "logged-op", SpanKindInternal)
		span.End()
		shutdown(t, tel)
		if !strings.Contains(out.String(), "logged-op") {
			t.Fatalf("the span was not logged: %q", out.String())
		}
	})
	t.Run("otlp exporter sends spans and metrics and sets the options", func(t *testing.T) {
		c, server := newCollector(t)
		cfg := config.Default()
		cfg.Telemetry.ServiceName = "svc"
		cfg.Telemetry.Exporter = config.TelemetryExporterOTLP
		cfg.Telemetry.OTLPEndpoint = server.URL
		cfg.Telemetry.OTLPCompression = "gzip"
		cfg.Telemetry.OTLPHeaders = config.NewSecret("X-Api-Key=k%20ey")
		cfg.Telemetry.TrustTraceparent = true
		tel, err := FromConfig(cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !tel.TrustIncoming() {
			t.Error("TRUST_TRACEPARENT is not applied")
		}
		_, span := tel.Tracer("x").Start(context.Background(), "op", SpanKindInternal)
		span.End()
		shutdown(t, tel)
		paths := map[string]bool{}
		for _, r := range c.received() {
			paths[r.path] = true
			if r.header.Get("X-Api-Key") != "k ey" || r.header.Get("Content-Encoding") != "gzip" {
				t.Errorf("%s: headers %v", r.path, r.header)
			}
		}
		if !paths["/v1/traces"] || !paths["/v1/metrics"] {
			t.Errorf("requests: %v", paths)
		}
	})
}

func TestNilTelemetryAccessors(t *testing.T) {
	t.Parallel()
	var tel *Telemetry
	if tel.Resource().Attributes != nil || tel.TrustIncoming() {
		t.Fatal("a nil Telemetry has no resource and trusts nothing")
	}
	if !newTestTelemetry(t, Options{TrustIncoming: true}).TrustIncoming() {
		t.Fatal("TrustIncoming is not reported")
	}
}

// --- batching ---

// gatedExporter signals every span export and holds it until released or
// cancelled.
type gatedExporter struct {
	entered chan struct{}
	release chan struct{}
}

func newGatedExporter() *gatedExporter {
	return &gatedExporter{entered: make(chan struct{}, 16), release: make(chan struct{})}
}

func (e *gatedExporter) ExportSpans(ctx context.Context, _ Resource, _ []SpanData) error {
	e.entered <- struct{}{}
	select {
	case <-e.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *gatedExporter) Shutdown(context.Context) error { return nil }

func TestBatcherExportsOnItsInterval(t *testing.T) {
	t.Parallel()
	exporter := newGatedExporter()
	close(exporter.release)
	tel := newTestTelemetry(t, Options{SampleRatio: 1, SpanExporter: exporter, BatchTimeout: 5 * time.Millisecond})
	_, span := tel.Tracer("x").Start(context.Background(), "op", SpanKindInternal)
	span.End()
	select {
	case <-exporter.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the batch was not exported on the interval")
	}
}

func TestForceFlushHonorsItsContext(t *testing.T) {
	t.Parallel()
	endOne := func(tel *Telemetry) {
		_, span := tel.Tracer("x").Start(context.Background(), "op", SpanKindInternal)
		span.End()
	}
	t.Run("a busy exporter blocks the request", func(t *testing.T) {
		t.Parallel()
		exporter := newGatedExporter()
		tel := newTestTelemetry(t, Options{SampleRatio: 1, SpanExporter: exporter, MaxExportBatch: 1})
		t.Cleanup(func() { close(exporter.release) })
		endOne(tel) // a full batch: the exporter is entered and held
		<-exporter.entered
		endOne(tel)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := tel.ForceFlush(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("the reply does not arrive in time", func(t *testing.T) {
		t.Parallel()
		exporter := newGatedExporter()
		tel := newTestTelemetry(t, Options{SampleRatio: 1, SpanExporter: exporter})
		t.Cleanup(func() { close(exporter.release) })
		endOne(tel)
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- tel.ForceFlush(ctx) }()
		<-exporter.entered // the flush was accepted and its export is held
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("after shutdown there is nothing to flush", func(t *testing.T) {
		t.Parallel()
		tel := newTestTelemetry(t, Options{SampleRatio: 1, SpanExporter: &recordingExporter{}})
		shutdown(t, tel)
		if err := tel.ForceFlush(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

// --- metrics ---

func TestInstrumentsIgnoreNilReceiversAndBadValues(t *testing.T) {
	t.Parallel()
	var counter *Counter
	var floats *FloatCounter
	var gauge *Gauge
	var histogram *Histogram
	counter.Observe(1)
	counter.Add(1)
	floats.Observe(1)
	floats.Add(1)
	gauge.Set(1)
	gauge.Add(1)
	histogram.Record(1)
	histogram.set([]uint64{1}, 1)
	if (*Registry)(nil).Collect() != nil {
		t.Fatal("a nil registry collects nothing")
	}

	registry := NewRegistry(0)
	c, _ := registry.Counter("c", "", "1")
	f, _ := registry.FloatCounter("f", "", "s")
	g, _ := registry.Gauge("g", "", "1")
	h, _ := registry.Histogram("h", "", "s", []float64{1})
	c.Observe(-5)
	f.Observe(math.NaN())
	f.Observe(math.Inf(1))
	f.Add(-1)
	f.Add(math.Inf(1))
	g.Set(math.NaN())
	g.Add(math.NaN())
	h.Record(math.NaN())
	h.set([]uint64{1, 2, 3}, 9) // wrong bucket count: two bounds' worth of counts for one bound
	for _, metric := range registry.Collect() {
		if metric.Name == overflowMetricName {
			continue
		}
		if len(metric.Points) != 0 {
			t.Errorf("%s recorded %+v", metric.Name, metric.Points)
		}
	}

	// Legitimate values still work, and the series never goes down.
	c.Observe(5)
	c.Observe(3)
	f.Observe(2.5)
	f.Observe(1)
	f.Add(0.5)
	g.Set(3)
	g.Add(-1)
	want := map[string]float64{"c": 5, "f": 3, "g": 2}
	for _, metric := range registry.Collect() {
		if expected, ok := want[metric.Name]; ok && (len(metric.Points) != 1 || metric.Points[0].Value != expected) {
			t.Errorf("%s = %+v, want %v", metric.Name, metric.Points, expected)
		}
	}
}

func TestRegistryRejectsBadDefinitionsOfEveryInstrumentKind(t *testing.T) {
	t.Parallel()
	registry := NewRegistry(0)
	if _, err := registry.FloatCounter("bad name", "", "s"); err == nil {
		t.Error("FloatCounter accepted an invalid name")
	}
	if _, err := registry.Gauge("bad name", "", "s"); err == nil {
		t.Error("Gauge accepted an invalid name")
	}
	var nilRegistry *Registry
	if f, err := nilRegistry.FloatCounter("f", "", "s"); f != nil || err != nil {
		t.Errorf("nil registry FloatCounter = %v, %v", f, err)
	}
	if g, err := nilRegistry.Gauge("g", "", "s"); g != nil || err != nil {
		t.Errorf("nil registry Gauge = %v, %v", g, err)
	}
}

func TestPrometheusOutputEncodesSpecialValuesAndLabels(t *testing.T) {
	t.Parallel()
	registry := NewRegistry(0)
	gauge, err := registry.Gauge("temperature", "Ambient.", "ms", "room")
	if err != nil {
		t.Fatal(err)
	}
	gauge.Set(math.Inf(1), "hot")
	gauge.Set(math.Inf(-1), "cold")
	gauge.Set(1, "bad\xffutf8")
	// +Inf plus -Inf is NaN: the sum of opposite infinities (Set and Add
	// refuse NaN itself but not this).
	gauge.Set(math.Inf(1), "nan")
	gauge.Add(math.Inf(-1), "nan")

	var out strings.Builder
	if err := registry.WritePrometheus(&out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"# TYPE temperature_milliseconds gauge",
		`temperature_milliseconds{room="hot"} +Inf`,
		`temperature_milliseconds{room="cold"} -Inf`,
		`temperature_milliseconds{room="nan"} NaN`,
		"temperature_milliseconds{room=\"bad\uFFFDutf8\"} 1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
}

func TestPrometheusHandlerRefusesOtherMethods(t *testing.T) {
	t.Parallel()
	handler := NewRegistry(0).PrometheusHandler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("%d %v", recorder.Code, recorder.Header())
	}
}

// --- runtime metrics ---

func TestRegisterRuntimeMetricsReportsEveryConflict(t *testing.T) {
	t.Parallel()
	names := []string{
		MetricGoroutines, MetricGoHeapAlloc, MetricGoHeapInuse, MetricGoMemoryTotal, MetricGoGCGoal, MetricGoProcessors,
		MetricGoAllocated, MetricGoGCCycles, MetricGoGCPause,
	}
	for _, name := range names {
		registry := NewRegistry(0)
		// An instrument of another kind under the same name is a conflicting
		// definition.
		if _, err := registry.FloatCounter(name, "squatter", "x"); err != nil {
			t.Fatal(err)
		}
		err := registerRuntimeMetrics(registry)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v, want a conflict naming the metric", name, err)
		}
	}
	if err := registerRuntimeMetrics(NewRegistry(0)); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeSampleHelpers(t *testing.T) {
	t.Parallel()
	if got := uintSample(metrics.Sample{}); got != 0 {
		t.Errorf("an unsupported sample reads %d", got)
	}
	for in, want := range map[uint64]int64{0: 0, 42: 42, math.MaxInt64: math.MaxInt64, math.MaxInt64 + 1: math.MaxInt64, math.MaxUint64: math.MaxInt64} {
		if got := clampInt64(in); got != want {
			t.Errorf("clampInt64(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestFoldHistogramUsesTheFiniteEdgeOfOpenBuckets(t *testing.T) {
	t.Parallel()
	bounds := []float64{1, 10}
	h := &metrics.Float64Histogram{
		Counts:  []uint64{2, 0, 3, 4},
		Buckets: []float64{math.Inf(-1), 0.5, 1, 6, math.Inf(1)},
	}
	counts := make([]uint64, len(bounds)+1)
	sum := foldHistogram(h, bounds, counts)
	// (-Inf,0.5] holds 2 at 0.5; (1,6] holds 3 at 3.5; (6,+Inf) holds 4 at 6.
	if want := 2*0.5 + 3*3.5 + 4*6.0; sum != want {
		t.Errorf("sum = %v, want %v", sum, want)
	}
	// Upper edges 0.5 and 6 fall in the first and second bucket; +Inf in the last.
	if counts[0] != 2 || counts[1] != 3 || counts[2] != 4 {
		t.Errorf("counts = %v", counts)
	}
}

// --- OTLP ---

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	future := time.Now().Add(20 * time.Second).UTC().Format(http.TimeFormat)
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	for name, tc := range map[string]struct {
		in       string
		min, max time.Duration
	}{
		"empty":          {"", 0, 0},
		"seconds":        {"7", 7 * time.Second, 7 * time.Second},
		"capped seconds": {"86400", maxRetryAfter, maxRetryAfter},
		"zero":           {"0", 0, 0},
		"negative":       {"-3", 0, 0},
		"garbage":        {"soon", 0, 0},
		"future date":    {future, 10 * time.Second, 20 * time.Second},
		"past date":      {past, 0, 0},
	} {
		if got := parseRetryAfter(tc.in); got < tc.min || got > tc.max {
			t.Errorf("%s: parseRetryAfter(%q) = %v, want %v..%v", name, tc.in, got, tc.min, tc.max)
		}
	}
}

func TestJitterStaysWithinHalfAndWhole(t *testing.T) {
	t.Parallel()
	for _, d := range []time.Duration{0, 1} {
		if got := jitter(d); got != d {
			t.Errorf("jitter(%v) = %v", d, got)
		}
	}
	for range 50 {
		if got := jitter(time.Second); got < time.Second/2 || got > time.Second {
			t.Fatalf("jitter(1s) = %v", got)
		}
	}
}

func TestOTLPExporterValidatesItsSettings(t *testing.T) {
	t.Parallel()
	for name, opts := range map[string]OTLPOptions{
		"negative attempts": {Endpoint: "http://collector:4318", MaxAttempts: -1},
		"negative backoff":  {Endpoint: "http://collector:4318", InitialBackoff: -1},
		"negative maximum":  {Endpoint: "http://collector:4318", MaxBackoff: -1},
		"empty header name": {Endpoint: "http://collector:4318", Headers: map[string]string{"": "v"}},
		"bad header name":   {Endpoint: "http://collector:4318", Headers: map[string]string{"X Bad": "v"}},
		"reserved header":   {Endpoint: "http://collector:4318", Headers: map[string]string{"content-type": "v"}},
		"control in value":  {Endpoint: "http://collector:4318", Headers: map[string]string{"X-Key": "a\nb"}},
	} {
		if exporter, err := NewOTLPExporter(opts); err == nil || exporter != nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestOTLPExporterSendsNothingForEmptyBatches(t *testing.T) {
	t.Parallel()
	c, server := newCollector(t)
	exporter := newExporter(t, server.URL, OTLPOptions{})
	ctx := context.Background()
	if err := exporter.ExportSpans(ctx, Resource{}, nil); err != nil {
		t.Error(err)
	}
	if err := exporter.ExportMetrics(ctx, Resource{}, nil); err != nil {
		t.Error(err)
	}
	if err := exporter.ExportLogs(ctx, Resource{}, nil); err != nil {
		t.Error(err)
	}
	if got := c.received(); len(got) != 0 {
		t.Fatalf("requests: %+v", got)
	}
}

func TestOTLPErrorNamesTheEndpointPathAndKeepsTheCause(t *testing.T) {
	t.Parallel()
	cause := errors.New("connection refused")
	err := error(&otlpError{target: "/v1/traces", cause: cause})
	if !errors.Is(err, cause) || err.Error() != "telemetry: OTLP export to /v1/traces failed: connection refused" {
		t.Errorf("err = %v", err)
	}
	if got := (&otlpError{target: "/v1/logs", status: 500}).Error(); got != "telemetry: OTLP export to /v1/logs failed with HTTP 500" {
		t.Errorf("err = %q", got)
	}
}

// A Retry-After longer than the context allows ends the retries at once
// instead of waiting for a deadline that is certain to pass.
func TestOTLPExporterDoesNotWaitPastTheDeadline(t *testing.T) {
	t.Parallel()
	hits := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits <- struct{}{}
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	exporter := newExporter(t, server.URL, OTLPOptions{MaxAttempts: 5})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := exporter.ExportSpans(ctx, Resource{}, []SpanData{{Name: "s"}})
	var failure *otlpError
	if !errors.As(err, &failure) || failure.status != http.StatusServiceUnavailable || len(hits) != 1 {
		t.Fatalf("err = %v after %d requests", err, len(hits))
	}
}

// cancelOnDeadline cancels itself the first time its deadline is asked for,
// which the exporter does after it chose a wait and before it sleeps.
type cancelOnDeadline struct {
	context.Context
	cancel context.CancelFunc
}

func (c cancelOnDeadline) Deadline() (time.Time, bool) {
	c.cancel()
	return time.Time{}, false
}

func TestOTLPExporterStopsRetryingWhenCancelledWhileWaiting(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)
	// A long backoff: only the cancellation can end the wait.
	exporter := newExporter(t, server.URL, OTLPOptions{MaxAttempts: 3, InitialBackoff: time.Hour, MaxBackoff: time.Hour})
	inner, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := exporter.ExportSpans(cancelOnDeadline{Context: inner, cancel: cancel}, Resource{}, []SpanData{{Name: "s"}})
	var failure *otlpError
	if !errors.As(err, &failure) || failure.status != http.StatusBadGateway {
		t.Fatalf("err = %v", err)
	}
}

func TestEncodeAttributesHandlesNonFiniteFloatsAndSkipsUnknownKinds(t *testing.T) {
	t.Parallel()
	encoded := encodeAttributes([]Attribute{
		Float64("nan", math.NaN()), Float64("inf", math.Inf(1)), Float64("ninf", math.Inf(-1)), Float64("pi", 3.5),
		{Key: "zero"}, // no kind: nothing to send
	})
	if len(encoded) != 4 {
		t.Fatalf("encoded %d attributes, want the 4 with a kind", len(encoded))
	}
	for i, want := range []string{`"NaN"`, `"Infinity"`, `"-Infinity"`, `3.5`} {
		got, err := encoded[i].Value.DoubleValue.MarshalJSON()
		if err != nil || string(got) != want {
			t.Errorf("%s = %s (%v), want %s", encoded[i].Key, got, err, want)
		}
	}
}

// newBufferLogger returns a logger that writes JSON lines to out.
func newBufferLogger(out *strings.Builder) *slog.Logger {
	return slog.New(slog.NewJSONHandler(out, nil))
}
