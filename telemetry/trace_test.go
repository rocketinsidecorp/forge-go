package telemetry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
)

// recordingExporter keeps exported spans. block, when set, makes exports wait
// until it is closed or the export context ends.
type recordingExporter struct {
	mu       sync.Mutex
	spans    []SpanData
	batches  []int
	block    chan struct{}
	err      error
	shutdown int
}

func (e *recordingExporter) ExportSpans(ctx context.Context, _ Resource, spans []SpanData) error {
	if e.block != nil {
		select {
		case <-e.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	e.spans = append(e.spans, spans...)
	e.batches = append(e.batches, len(spans))
	return nil
}

func (e *recordingExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shutdown++
	return nil
}

func (e *recordingExporter) exported() []SpanData {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]SpanData(nil), e.spans...)
}

func newTestTelemetry(t *testing.T, opts Options) *Telemetry {
	t.Helper()
	if opts.ServiceName == "" {
		opts.ServiceName = "test"
	}
	tel, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tel.Shutdown(ctx)
	})
	return tel
}

func shutdown(t *testing.T, tel *Telemetry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	for name, opts := range map[string]Options{
		"no service":       {},
		"ratio above one":  {ServiceName: "s", SampleRatio: 1.5},
		"negative ratio":   {ServiceName: "s", SampleRatio: -0.1},
		"negative queue":   {ServiceName: "s", MaxQueueSize: -1},
		"tiny interval":    {ServiceName: "s", MetricsInterval: time.Millisecond},
		"queue over limit": {ServiceName: "s", MaxQueueSize: maxQueueSizeLimit + 1},
	} {
		if _, err := New(opts); err == nil {
			t.Errorf("%s: New accepted invalid options", name)
		}
	}
}

func TestNilTelemetryIsANoOp(t *testing.T) {
	var tel *Telemetry
	ctx, span := tel.Tracer("x").Start(context.Background(), "op", SpanKindInternal, String("k", "v"))
	if span != nil || ctx != context.Background() {
		t.Fatal("a nil Telemetry must not start spans")
	}
	span.SetAttributes(String("a", "b"))
	span.AddEvent("e")
	span.RecordError(errors.New("boom"))
	span.SetStatus(StatusError, "x")
	span.SetName("n")
	span.End()
	if tel.Metrics() != nil || tel.ForceFlush(context.Background()) != nil || tel.Shutdown(context.Background()) != nil {
		t.Fatal("nil Telemetry methods must be no-ops")
	}
	middleware, err := tel.ServerMiddleware()
	if middleware != nil || err != nil {
		t.Fatal("nil Telemetry installs no middleware")
	}
	counter, err := tel.Metrics().Counter("c", "", "1")
	if counter != nil || err != nil {
		t.Fatal("nil registry returns nil instruments")
	}
	counter.Add(1)
}

func TestDisabledPathsDoNotAllocate(t *testing.T) {
	var tel *Telemetry
	tracer := tel.Tracer("x")
	var histogram *Histogram
	ctx := context.Background()
	allocs := testing.AllocsPerRun(1000, func() {
		_, span := tracer.Start(ctx, "op", SpanKindServer)
		span.SetAttributes()
		span.End()
		histogram.Record(1, "a")
		_ = SpanContextFromContext(ctx)
	})
	if allocs != 0 {
		t.Fatalf("disabled telemetry allocated %v times per operation", allocs)
	}
}

func TestSpansWithoutExporterCarryIdentifiersButDoNotRecord(t *testing.T) {
	tel := newTestTelemetry(t, Options{SampleRatio: 1})
	ctx, span := tel.Tracer("x").Start(context.Background(), "op", SpanKindInternal)
	if !span.SpanContext().IsValid() || !span.SpanContext().IsSampled() {
		t.Fatalf("span context %+v", span.SpanContext())
	}
	if span.IsRecording() {
		t.Fatal("without an exporter spans must not record")
	}
	_, child := tel.Tracer("x").Start(ctx, "child", SpanKindInternal)
	if child.SpanContext().TraceID != span.SpanContext().TraceID || child.SpanContext().SpanID == span.SpanContext().SpanID {
		t.Fatal("child must share the trace and have its own span ID")
	}
	child.End()
	span.End()
}

func TestParentBasedRatioSampling(t *testing.T) {
	exporter := &recordingExporter{}
	never := newTestTelemetry(t, Options{SampleRatio: 0, SpanExporter: exporter})
	for range 200 {
		_, span := never.Tracer("x").Start(context.Background(), "root", SpanKindInternal)
		if span.SpanContext().IsSampled() || span.IsRecording() {
			t.Fatal("ratio 0 must not sample new traces")
		}
		span.End()
	}

	// Parent decisions win over the ratio, in both directions.
	sampledParent := ContextWithRemoteSpanContext(context.Background(), SpanContext{TraceID: TraceID{1}, SpanID: SpanID{2}, Flags: FlagSampled, TraceState: "a=b"})
	_, child := never.Tracer("x").Start(sampledParent, "child", SpanKindServer)
	if !child.IsRecording() || child.SpanContext().TraceID != (TraceID{1}) || child.SpanContext().TraceState != "a=b" {
		t.Fatalf("child of a sampled parent: %+v", child.SpanContext())
	}
	child.End()
	shutdown(t, never)
	spans := exporter.exported()
	if len(spans) != 1 || spans[0].ParentSpanID != (SpanID{2}) || !spans[0].ParentRemote {
		t.Fatalf("exported %+v", spans)
	}

	always := newTestTelemetry(t, Options{SampleRatio: 1, SpanExporter: &recordingExporter{}})
	unsampledParent := ContextWithRemoteSpanContext(context.Background(), SpanContext{TraceID: TraceID{1}, SpanID: SpanID{2}})
	_, child = always.Tracer("x").Start(unsampledParent, "child", SpanKindServer)
	if child.SpanContext().IsSampled() || child.IsRecording() {
		t.Fatal("child of an unsampled parent must not be sampled")
	}

	half := newTestTelemetry(t, Options{SampleRatio: 0.5})
	sampled := 0
	for range 4000 {
		_, span := half.Tracer("x").Start(context.Background(), "root", SpanKindInternal)
		if span.SpanContext().IsSampled() {
			sampled++
		}
	}
	if sampled < 1700 || sampled > 2300 {
		t.Fatalf("ratio 0.5 sampled %d of 4000", sampled)
	}
}

func TestRatioThresholdMatchesTraceIDBits(t *testing.T) {
	tel := newTestTelemetry(t, Options{SampleRatio: 0.25})
	low := TraceID{15: 0x02}                                        // tiny lower half: sampled
	high := TraceID{8: 0xff, 9: 0xff, 10: 0xff, 11: 0xff, 15: 0xff} // near the top: not sampled
	if !tel.sampleRoot(low) || tel.sampleRoot(high) {
		t.Fatal("sampling must compare the trace ID's low 8 bytes with the threshold")
	}
}

func TestSpanRecordsAttributesEventsAndStatus(t *testing.T) {
	exporter := &recordingExporter{}
	tel := newTestTelemetry(t, Options{SampleRatio: 1, SpanExporter: exporter})
	_, span := tel.Tracer("scope").Start(context.Background(), "op", SpanKindClient, String("a", "1"), String("", "ignored"))
	span.SetAttributes(String("a", "2"), Int("n", 3), Bool("b", true), Float64("f", 1.5))
	for i := range MaxSpanAttributes + 5 {
		span.SetAttributes(Int(fmt.Sprintf("k%d", i), i))
	}
	for range MaxSpanEvents + 2 {
		span.AddEvent("event", String("x", "y"))
	}
	span.SetName("renamed")
	span.SetStatus(StatusError, strings.Repeat("d", 2000))
	span.End()
	span.End() // second End has no effect
	span.SetAttributes(String("late", "x"))
	shutdown(t, tel)

	spans := exporter.exported()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans", len(spans))
	}
	got := spans[0]
	if got.Name != "renamed" || got.Kind != SpanKindClient || got.Scope != "scope" || got.Status != StatusError {
		t.Fatalf("span %+v", got)
	}
	if len(got.Attributes) != MaxSpanAttributes || got.DroppedAttributes != 9 {
		t.Fatalf("attributes %d dropped %d", len(got.Attributes), got.DroppedAttributes)
	}
	if got.Attributes[0].Key != "a" || got.Attributes[0].Value.AsString() != "2" {
		t.Fatalf("a later value replaces an earlier one: %+v", got.Attributes[0])
	}
	if len(got.Events) != MaxSpanEvents || got.DroppedEvents != 2 {
		t.Fatalf("events %d dropped %d", len(got.Events), got.DroppedEvents)
	}
	if len(got.StatusDescription) != MaxAttributeValueLength {
		t.Fatalf("status description not truncated: %d", len(got.StatusDescription))
	}
	if !got.End.After(got.Start) && !got.End.Equal(got.Start) {
		t.Fatal("end before start")
	}
}

func TestRecordErrorNeverExportsTheMessage(t *testing.T) {
	exporter := &recordingExporter{}
	tel := newTestTelemetry(t, Options{SampleRatio: 1, SpanExporter: exporter})
	_, span := tel.Tracer("x").Start(context.Background(), "op", SpanKindInternal)
	span.RecordError(fmt.Errorf("user alice@example.com: %w", errors.New("secret-token-123")))
	span.End()
	shutdown(t, tel)
	data := fmt.Sprintf("%+v", exporter.exported())
	if strings.Contains(data, "alice") || strings.Contains(data, "secret-token") {
		t.Fatalf("error message leaked into span: %s", data)
	}
	if !strings.Contains(data, "*errors.errorString") {
		t.Fatalf("error type missing: %s", data)
	}
}

func TestErrorType(t *testing.T) {
	for want, err := range map[string]error{
		"not_found":           fmt.Errorf("wrapped: %w", fault.New("not_found", "x", fault.CategoryNotFound, 404)),
		"timeout":             fmt.Errorf("op: %w", context.DeadlineExceeded),
		"canceled":            context.Canceled,
		"*errors.errorString": fmt.Errorf("a: %w", errors.New("b")),
	} {
		if got := ErrorType(err); got != want {
			t.Errorf("ErrorType(%v) = %q, want %q", err, got, want)
		}
	}
	if ErrorType(nil) != "" {
		t.Error("ErrorType(nil) must be empty")
	}
}

func TestStartChildOnlyInsideATrace(t *testing.T) {
	exporter := &recordingExporter{}
	tel := newTestTelemetry(t, Options{SampleRatio: 1, SpanExporter: exporter})
	if _, span := StartChild(context.Background(), "lib", "op", SpanKindProducer); span != nil {
		t.Fatal("StartChild must not create root spans")
	}
	ctx, parent := tel.Tracer("app").Start(context.Background(), "parent", SpanKindServer)
	_, child := StartChild(ctx, "lib", "op", SpanKindProducer)
	if child == nil || child.SpanContext().TraceID != parent.SpanContext().TraceID {
		t.Fatal("StartChild must continue the active trace")
	}
	child.End()
	parent.End()
}

func TestStringAttributesAreTruncatedAndValidUTF8(t *testing.T) {
	long := String("k", strings.Repeat("é", MaxAttributeValueLength))
	if len(long.Value.AsString()) > MaxAttributeValueLength || !strings.HasPrefix(long.Value.AsString(), "éé") {
		t.Fatalf("truncated to %d bytes", len(long.Value.AsString()))
	}
	if invalid := String("k", "a\xffb"); invalid.Value.AsString() != "a�b" {
		t.Fatalf("invalid UTF-8 not replaced: %q", invalid.Value.AsString())
	}
	if Int64("n", -5).Value.AsInt64() != -5 || Float64("f", 2.5).Value.AsFloat64() != 2.5 || !Bool("b", true).Value.AsBool() {
		t.Fatal("value round trip failed")
	}
}

// A truncated value is a copy: a queued span or record must not keep the
// caller's large string alive. Copying is the one allocation truncation
// makes; a value within the limit is kept as is.
func TestTruncatedStringsDoNotShareTheCallersMemory(t *testing.T) {
	large := strings.Repeat("v", 64*MaxAttributeValueLength)
	if allocs := testing.AllocsPerRun(10, func() { _ = truncate(large, MaxAttributeValueLength) }); allocs != 1 {
		t.Fatalf("truncating made %v allocations, want the one copy", allocs)
	}
	small := large[:MaxAttributeValueLength]
	if allocs := testing.AllocsPerRun(10, func() { _ = truncate(small, MaxAttributeValueLength) }); allocs != 0 {
		t.Fatalf("a value within the limit made %v allocations", allocs)
	}
}
