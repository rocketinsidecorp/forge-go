package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"
)

type failingTextAppender struct{}

func (failingTextAppender) AppendText([]byte) ([]byte, error) { return nil, errors.New(logValueSecret) }

type failingTextMarshaler struct{}

func (failingTextMarshaler) MarshalText() ([]byte, error) { return nil, errors.New(logValueSecret) }

// nilPointerPanics panics when called on a nil receiver, like a method that
// dereferences its receiver.
type nilPointerPanics struct{ name string }

func (p *nilPointerPanics) MarshalJSON() ([]byte, error) { return []byte(`"` + p.name + `"`), nil }
func (p *nilPointerPanics) MarshalText() ([]byte, error) { return []byte(p.name), nil }

func TestLogHandlerWithAttrsAndGroupReturnTheHandlerWhenThereIsNothingToAdd(t *testing.T) {
	t.Parallel()
	tel := newTestTelemetry(t, Options{LogExporter: &recordingLogExporter{}})
	handler := tel.LogHandler()
	if handler.WithAttrs(nil) != handler || handler.WithGroup("") != handler {
		t.Fatal("an empty WithAttrs or WithGroup must return the same handler")
	}
	if handler.WithGroup("g") == handler || handler.WithAttrs([]slog.Attr{slog.Int("a", 1)}) == handler {
		t.Fatal("a non-empty WithAttrs or WithGroup must derive a new handler")
	}
}

func TestLogRecordWithoutATimeGetsTheObservedTime(t *testing.T) {
	t.Parallel()
	exporter := &recordingLogExporter{}
	tel := newTestTelemetry(t, Options{LogExporter: exporter})
	before := time.Now()
	record := slog.NewRecord(time.Time{}, slog.LevelInfo, "untimed", 0)
	if err := tel.LogHandler().Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	shutdown(t, tel)
	records := exporter.exported()
	if len(records) != 1 || records[0].Time.Before(before) || !records[0].Time.Equal(records[0].ObservedTime) {
		t.Fatalf("records: %+v", records)
	}
}

func TestLogTeeIsDisabledWhenNoHandlerIsEnabled(t *testing.T) {
	t.Parallel()
	tee := NewTeeHandler(
		slog.NewJSONHandler(&strings.Builder{}, &slog.HandlerOptions{Level: slog.LevelError}),
		slog.NewTextHandler(&strings.Builder{}, &slog.HandlerOptions{Level: slog.LevelError}),
	)
	if tee.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("no handler wants Info")
	}
	if !tee.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("both handlers want Error")
	}
}

func TestLogBodyFollowsReplaceAttrOfTheMessage(t *testing.T) {
	t.Parallel()
	replace := func(fn func(a slog.Attr) slog.Attr) func([]string, slog.Attr) slog.Attr {
		return func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.MessageKey {
				return fn(a)
			}
			return a
		}
	}
	type payload struct{ Code int }
	tests := map[string]struct {
		fn     func(a slog.Attr) slog.Attr
		format LogFormat
		want   string
	}{
		"removed":       {func(slog.Attr) slog.Attr { return slog.Attr{} }, LogFormatJSON, ""},
		"turned group":  {func(a slog.Attr) slog.Attr { return slog.Group(a.Key, slog.Int("n", 1)) }, LogFormatJSON, ""},
		"rewritten":     {func(a slog.Attr) slog.Attr { return slog.String(a.Key, "[redacted]") }, LogFormatJSON, "[redacted]"},
		"any as JSON":   {func(a slog.Attr) slog.Attr { return slog.Any(a.Key, payload{7}) }, LogFormatJSON, `{"Code":7}`},
		"any as text":   {func(a slog.Attr) slog.Attr { return slog.Any(a.Key, payload{7}) }, LogFormatText, `{Code:7}`},
		"integer":       {func(a slog.Attr) slog.Attr { return slog.Int(a.Key, 42) }, LogFormatJSON, "42"},
		"bool":          {func(a slog.Attr) slog.Attr { return slog.Bool(a.Key, true) }, LogFormatJSON, "true"},
		"duration":      {func(a slog.Attr) slog.Attr { return slog.Duration(a.Key, 1500*time.Millisecond) }, LogFormatJSON, "1.5s"},
		"unresolved":    {func(a slog.Attr) slog.Attr { return slog.Any(a.Key, lateSecret{}) }, LogFormatJSON, replacedSecret},
		"float":         {func(a slog.Attr) slog.Attr { return slog.Float64(a.Key, 0.25) }, LogFormatJSON, "0.25"},
		"untouched key": {func(a slog.Attr) slog.Attr { return a }, LogFormatJSON, "original"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			exporter := &recordingLogExporter{}
			tel := newTestTelemetry(t, Options{LogExporter: exporter, LogFormat: tc.format, LogReplaceAttr: replace(tc.fn)})
			logsLogger(tel).Info("original")
			shutdown(t, tel)
			records := exporter.exported()
			if len(records) != 1 || records[0].Body != tc.want {
				t.Fatalf("records: %+v, want body %q", records, tc.want)
			}
		})
	}
}

func TestLogAttributeKindsAreExportedWithTheirOwnTypes(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	exporter := &recordingLogExporter{}
	tel := newTestTelemetry(t, Options{LogExporter: exporter})
	// Logger.With hands groups to the handler as they are, empty ones too.
	logsLogger(tel).With(slog.Group("with_empty")).LogAttrs(context.Background(), slog.LevelInfo, "kinds",
		slog.Uint64("small", 12),
		slog.Uint64("max_int64", math.MaxInt64),
		slog.Uint64("huge", math.MaxUint64),
		slog.Time("when", when),
		slog.Group("empty"), // a group without members is left out
		slog.Group("", slog.Int("inlined", 1)),
		slog.Group("g", slog.Group("inner", slog.String("k", "v"))),
	)
	shutdown(t, tel)
	records := exporter.exported()
	if len(records) != 1 {
		t.Fatalf("records: %+v", records)
	}
	byKey := map[string]Attribute{}
	for _, attr := range records[0].Attributes {
		byKey[attr.Key] = attr
	}
	if a := byKey["small"]; a.Value.Kind() != KindInt64 || a.Value.AsInt64() != 12 {
		t.Errorf("small = %+v", a)
	}
	if a := byKey["max_int64"]; a.Value.Kind() != KindInt64 || a.Value.AsInt64() != math.MaxInt64 {
		t.Errorf("max_int64 = %+v", a)
	}
	if a := byKey["huge"]; a.Value.Kind() != KindString || a.Value.AsString() != "18446744073709551615" {
		t.Errorf("a uint64 beyond int64 is sent as text: %+v", a)
	}
	if a := byKey["when"]; a.Value.AsString() != "2026-01-02T03:04:05.000000006Z" {
		t.Errorf("when = %+v", a)
	}
	for _, key := range []string{"empty", "with_empty"} {
		if _, ok := byKey[key]; ok {
			t.Errorf("the empty group %q has no attribute", key)
		}
	}
	if a := byKey["inlined"]; a.Value.AsInt64() != 1 {
		t.Errorf("a group without a key is inlined: %v", byKey)
	}
	if a := byKey["g.inner.k"]; a.Value.AsString() != "v" {
		t.Errorf("nested groups are dotted: %v", byKey)
	}
}

func TestLogValueEncodingSurvivesFailingAndPanickingMethods(t *testing.T) {
	t.Parallel()
	var nilPointer *nilPointerPanics
	text := exportedLogValues(t, LogFormatText, []slog.Attr{
		slog.Any("appender", failingTextAppender{}),
		slog.Any("marshaler", failingTextMarshaler{}),
		slog.Any("nil_pointer", nilPointer),
	})
	for _, key := range []string{"appender", "marshaler"} {
		if !strings.HasPrefix(text[key], "!ERROR: cannot encode ") || strings.Contains(text[key], logValueSecret) {
			t.Errorf("%s = %q", key, text[key])
		}
	}
	if text["nil_pointer"] != "<nil>" {
		t.Errorf("a nil pointer whose method dereferences it reads %q, want <nil>", text["nil_pointer"])
	}
	jsonValues := exportedLogValues(t, LogFormatJSON, []slog.Attr{slog.Any("nil_pointer", nilPointer)})
	// encoding/json writes a nil pointer as null without calling the method.
	if jsonValues["nil_pointer"] != "null" {
		t.Errorf("JSON: %q", jsonValues["nil_pointer"])
	}
}

func TestLogExporterRecordsTheParentSpan(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	exporter := NewLogExporter(slog.New(slog.NewJSONHandler(&out, nil)))
	tel := newTestTelemetry(t, Options{SampleRatio: 1, SpanExporter: exporter})
	ctx, parent := tel.Tracer("x").Start(context.Background(), "parent", SpanKindServer)
	_, child := tel.Tracer("x").Start(ctx, "child", SpanKindInternal, String("k", "v"))
	child.End()
	parent.End()
	shutdown(t, tel)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines: %q", out.String())
	}
	if !strings.Contains(lines[0], `"parent_span_id":"`+parent.SpanContext().SpanID.String()+`"`) || !strings.Contains(lines[0], `"attributes":{"k":"v"}`) {
		t.Errorf("child line: %s", lines[0])
	}
	if strings.Contains(lines[1], "parent_span_id") || strings.Contains(lines[1], "attributes") {
		t.Errorf("root line: %s", lines[1])
	}
}

func TestObservePoolReportsEveryConflictingInstrument(t *testing.T) {
	t.Parallel()
	for _, name := range []string{MetricDBConnectionCount, MetricDBConnectionMax, MetricDBPoolWaits, MetricDBPoolWaitTime} {
		tel := newTestTelemetry(t, Options{})
		if _, err := tel.Metrics().FloatCounter(name, "squatter", "x"); err != nil {
			t.Fatal(err)
		}
		stop, err := tel.ObservePool("postgresql", "main", func() PoolStats { return PoolStats{} })
		if err == nil || stop != nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: stop %v err %v", name, stop != nil, err)
		}
	}
}

func TestServerAndClientMiddlewareReportConflictingMetrics(t *testing.T) {
	t.Parallel()
	for _, name := range []string{MetricHTTPServerDuration, MetricHTTPServerActive, MetricHTTPClientDuration} {
		tel := newTestTelemetry(t, Options{})
		if _, err := tel.Metrics().FloatCounter(name, "squatter", "x"); err != nil {
			t.Fatal(err)
		}
		var err error
		if name == MetricHTTPClientDuration {
			_, err = tel.Transport(nil)
		} else {
			_, err = tel.ServerMiddleware()
		}
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
