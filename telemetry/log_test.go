package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestLogHandlerCorrelatesRecordsWithTheActiveSpan(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(NewLogHandler(slog.NewJSONHandler(&out, nil)))
	tel := newTestTelemetry(t, Options{SampleRatio: 1})
	ctx, span := tel.Tracer("x").Start(context.Background(), "op", SpanKindInternal)

	logger.InfoContext(ctx, "with span")
	logger.Info("without context")
	logger.With(LogAttrs(ctx)...).InfoContext(ctx, "already correlated")
	logger.WithGroup("g").InfoContext(ctx, "grouped")

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines: %q", lines)
	}
	decode := func(line string) map[string]any {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	first := decode(lines[0])
	if first[LogTraceIDKey] != span.SpanContext().TraceID.String() || first[LogSpanIDKey] != span.SpanContext().SpanID.String() {
		t.Fatalf("record %v", first)
	}
	if _, ok := decode(lines[1])[LogTraceIDKey]; ok {
		t.Fatal("a record without context must not get a trace ID")
	}
	if strings.Count(lines[2], LogTraceIDKey) != 1 {
		t.Fatalf("trace_id duplicated: %s", lines[2])
	}
	if NewLogHandler(logger.Handler()) != logger.Handler() {
		t.Fatal("wrapping twice must not stack handlers")
	}
	if LogAttrs(context.Background()) != nil {
		t.Fatal("LogAttrs without a span must be nil")
	}
}

func TestLogExporterWritesSpans(t *testing.T) {
	var out bytes.Buffer
	tel := newTestTelemetry(t, Options{SampleRatio: 1, SpanExporter: NewLogExporter(slog.New(slog.NewTextHandler(&out, nil)))})
	_, span := tel.Tracer("x").Start(context.Background(), "checkout", SpanKindServer, String("http.route", "/cart"))
	span.End()
	shutdown(t, tel)
	for _, want := range []string{"msg=span", "name=checkout", "kind=server", "attributes.http.route=/cart", "trace_id="} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("log %q lacks %q", out.String(), want)
		}
	}
}
