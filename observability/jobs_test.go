package observability_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fabriciobonjorno/forge-go/observability"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestTraceJobPropagatesContextAndRecordsSanitizedFailure(t *testing.T) {
	t.Parallel()
	errorToReturn := errors.New("database password=secret")
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})

	var jobSpan oteltrace.SpanContext
	job := observability.TraceJob(provider, "nightly-cleanup", func(ctx context.Context) error {
		jobSpan = oteltrace.SpanFromContext(ctx).SpanContext()
		return errorToReturn
	})
	if err := job(context.Background()); !errors.Is(err, errorToReturn) {
		t.Fatalf("job error=%v, want original error", err)
	}
	if !jobSpan.IsValid() {
		t.Fatal("job context does not carry the created span")
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("span count=%d want=1", len(spans))
	}
	span := spans[0]
	if span.Name != "job nightly-cleanup" {
		t.Errorf("span name=%q", span.Name)
	}
	if span.Status.Code != codes.Error {
		t.Errorf("span status=%v want error", span.Status.Code)
	}
	if got := jobSpanAttribute(span.Attributes, "forge.job.name"); got != "nightly-cleanup" {
		t.Errorf("job name attribute=%q", got)
	}
	if len(span.Events) != 1 || jobSpanAttribute(span.Events[0].Attributes, "exception.message") != "scheduled job failed" {
		t.Errorf("span did not record a sanitized failure event: %+v", span.Events)
	}
}

func TestTraceJobAcceptsNoopJob(t *testing.T) {
	t.Parallel()
	if job := observability.TraceJob(nil, "unused", nil); job != nil {
		t.Fatal("nil job should stay nil")
	}
}

func jobSpanAttribute(attributes []attribute.KeyValue, key string) string {
	for _, item := range attributes {
		if string(item.Key) == key {
			return item.Value.String()
		}
	}
	return ""
}
