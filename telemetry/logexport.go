package telemetry

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"
)

// Log record limits. A record is bounded so a full queue holds a bounded
// amount of memory: the body is cut at MaxLogBodyLength bytes, string
// attribute values at MaxAttributeValueLength, and attributes beyond
// MaxLogAttributes are dropped and counted in the record's
// droppedAttributesCount.
const (
	MaxLogBodyLength = 4096
	MaxLogAttributes = 64
	maxLogKeyLength  = 256
)

// OpenTelemetry severity numbers (opentelemetry-proto SeverityNumber). Each
// level covers four numbers; slog's four named levels map to the first of
// their range and the offsets in between keep slog's finer levels ordered.
const (
	severityTrace = 1
	severityDebug = 5
	severityInfo  = 9
	severityWarn  = 13
	severityError = 17
	severityFatal = 21
)

// LogRecord is one log entry queued for export: the data model of
// OpenTelemetry logs, reduced to what Forge fills in.
type LogRecord struct {
	// Time is when the event happened; ObservedTime when Forge saw it.
	Time         time.Time
	ObservedTime time.Time
	// Severity is the OpenTelemetry severity number, 1 to 24.
	Severity     int
	SeverityText string
	Body         string
	Attributes   []Attribute
	// DroppedAttributes counts attributes beyond MaxLogAttributes.
	DroppedAttributes int
	// TraceID, SpanID and Flags correlate the record with a trace; they are
	// zero for a record logged outside one.
	TraceID TraceID
	SpanID  SpanID
	Flags   TraceFlags
}

// LogRecordExporter sends log records to a backend. ExportLogs is called
// from one goroutine at a time; it must honor ctx and must not retain
// records after it returns.
type LogRecordExporter interface {
	ExportLogs(ctx context.Context, resource Resource, records []LogRecord) error
	Shutdown(ctx context.Context) error
}

// LogStats reports log export counters, like Stats for spans.
func (t *Telemetry) LogStats() Stats {
	if t == nil || t.logs == nil {
		return Stats{}
	}
	failed := t.logs.failed.Load()
	return Stats{Dropped: t.logs.dropped.Load() + failed, Exported: t.logs.exported.Load(), Failed: failed}
}

// LogHandler returns an slog.Handler that queues every enabled record for
// export, or nil when no log exporter is configured (a nil Telemetry
// included). The handler never blocks and never logs: when the queue is full
// the record is dropped and counted (forge_telemetry_logs_dropped_total).
// Combine it with the handler that writes to standard output through
// NewTeeHandler; forge.New does this when the log exporter comes from the
// FORGE_TELEMETRY_* configuration.
//
// The record carries exactly what the logger would write: message and
// attributes, flattened with dotted group names, with values resolved
// through slog.LogValuer (config.Secret therefore exports its redaction, not
// its value). The trace context comes from the ctx given to InfoContext and
// friends, or from trace_id and span_id attributes (the request logger's).
func (t *Telemetry) LogHandler() slog.Handler {
	if t == nil || t.logs == nil {
		return nil
	}
	return &otlpLogHandler{telemetry: t}
}

// NewTeeHandler returns a handler that passes every record to each handler
// that has the record's level enabled, with its own copy of the record. Nil
// handlers are skipped. With one handler it returns that handler unchanged;
// with none, a handler that is never enabled.
func NewTeeHandler(handlers ...slog.Handler) slog.Handler {
	var tee teeHandler
	for _, handler := range handlers {
		if handler != nil {
			tee = append(tee, handler)
		}
	}
	switch len(tee) {
	case 0:
		return slog.DiscardHandler
	case 1:
		return tee[0]
	}
	return tee
}

type teeHandler []slog.Handler

func (t teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range t {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (t teeHandler) Handle(ctx context.Context, record slog.Record) error {
	var errs []error
	for _, handler := range t {
		if handler.Enabled(ctx, record.Level) {
			// Handlers may modify the record they receive.
			errs = append(errs, handler.Handle(ctx, record.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (t teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make(teeHandler, len(t))
	for i, handler := range t {
		next[i] = handler.WithAttrs(attrs)
	}
	return next
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	next := make(teeHandler, len(t))
	for i, handler := range t {
		next[i] = handler.WithGroup(name)
	}
	return next
}

// otlpLogHandler converts slog records into LogRecords on the telemetry's
// log queue. Derived handlers (WithAttrs, WithGroup) share the queue.
type otlpLogHandler struct {
	telemetry *Telemetry
	// attrs are the attributes added with WithAttrs, already flattened with
	// the group prefix that was current when they were added.
	attrs []Attribute
	// prefix is the current group path with a trailing dot, or "".
	prefix string
}

func (h *otlpLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.telemetry.logLevel.Level()
}

func (h *otlpLogHandler) Handle(ctx context.Context, record slog.Record) error {
	now := time.Now()
	severity, text := severityOf(record.Level)
	out := LogRecord{
		Time:         record.Time,
		ObservedTime: now,
		Severity:     severity,
		SeverityText: text,
		Body:         strings.Clone(truncate(record.Message, MaxLogBodyLength)),
	}
	if out.Time.IsZero() {
		out.Time = now
	}
	attrs := make([]Attribute, 0, len(h.attrs)+record.NumAttrs())
	attrs = append(attrs, h.attrs...)
	record.Attrs(func(attr slog.Attr) bool {
		attrs = appendLogAttr(attrs, h.prefix, attr)
		return true
	})

	// A traced context wins; otherwise a trace_id attribute (the request
	// logger's, attached with With) supplies the correlation. Either way the
	// identifiers move out of the attributes into the record's own fields.
	if sc := SpanContextFromContext(ctx); sc.IsValid() {
		out.TraceID, out.SpanID, out.Flags = sc.TraceID, sc.SpanID, sc.Flags
		attrs, _, _ = extractTraceAttrs(attrs)
	} else {
		var traceID TraceID
		var spanID SpanID
		attrs, traceID, spanID = extractTraceAttrs(attrs)
		if traceID.IsValid() && spanID.IsValid() {
			out.TraceID, out.SpanID = traceID, spanID
		}
	}
	if len(attrs) > MaxLogAttributes {
		out.DroppedAttributes = len(attrs) - MaxLogAttributes
		attrs = attrs[:MaxLogAttributes]
	}
	out.Attributes = attrs
	h.telemetry.exportLog(out)
	return nil
}

func (h *otlpLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	next := *h
	next.attrs = append(make([]Attribute, 0, len(h.attrs)+len(attrs)), h.attrs...)
	for _, attr := range attrs {
		next.attrs = appendLogAttr(next.attrs, h.prefix, attr)
	}
	return &next
}

func (h *otlpLogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.prefix = h.prefix + name + "."
	return &next
}

// extractTraceAttrs removes valid top-level trace_id and span_id string
// attributes, returning the identifiers they held. Other attributes with
// these keys (wrong length, not hex, grouped) stay.
func extractTraceAttrs(attrs []Attribute) ([]Attribute, TraceID, SpanID) {
	var traceID TraceID
	var spanID SpanID
	kept := attrs[:0]
	for _, attr := range attrs {
		if attr.Value.Kind() == KindString {
			switch attr.Key {
			case LogTraceIDKey:
				var id TraceID
				if decodeHexID(id[:], attr.Value.AsString()) {
					traceID = id
					continue
				}
			case LogSpanIDKey:
				var id SpanID
				if decodeHexID(id[:], attr.Value.AsString()) {
					spanID = id
					continue
				}
			}
		}
		kept = append(kept, attr)
	}
	clear(attrs[len(kept):])
	return kept, traceID, spanID
}

// decodeHexID fills dst from the lowercase hex form s, which must have
// exactly 2*len(dst) characters.
func decodeHexID(dst []byte, s string) bool {
	if len(s) != 2*len(dst) || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.Decode(dst, []byte(s))
	return err == nil
}

// appendLogAttr flattens attr, resolved through slog.LogValuer, into dst.
// Groups become dotted prefixes ("http.status"), as OpenTelemetry has no
// nested attribute maps in Forge's encoding.
func appendLogAttr(dst []Attribute, prefix string, attr slog.Attr) []Attribute {
	value := attr.Value.Resolve()
	if attr.Key == "" && value.Kind() == slog.KindAny && value.Any() == nil {
		return dst // slog ignores empty attributes
	}
	if value.Kind() == slog.KindGroup {
		group := value.Group()
		if len(group) == 0 {
			return dst
		}
		if attr.Key != "" {
			prefix += attr.Key + "."
		}
		for _, member := range group {
			dst = appendLogAttr(dst, prefix, member)
		}
		return dst
	}
	key := truncate(prefix+attr.Key, maxLogKeyLength)
	switch value.Kind() {
	case slog.KindString:
		return append(dst, String(key, value.String()))
	case slog.KindInt64:
		return append(dst, Int64(key, value.Int64()))
	case slog.KindUint64:
		if u := value.Uint64(); u <= math.MaxInt64 {
			return append(dst, Int64(key, int64(u)))
		}
		return append(dst, String(key, strconv.FormatUint(value.Uint64(), 10)))
	case slog.KindFloat64:
		return append(dst, Float64(key, value.Float64()))
	case slog.KindBool:
		return append(dst, Bool(key, value.Bool()))
	case slog.KindDuration:
		return append(dst, String(key, value.Duration().String()))
	case slog.KindTime:
		return append(dst, String(key, value.Time().Format(time.RFC3339Nano)))
	}
	// KindAny: what the text handler would print. Errors print their
	// message, like on standard output.
	if err, ok := value.Any().(error); ok {
		return append(dst, String(key, err.Error()))
	}
	return append(dst, String(key, fmt.Sprintf("%+v", value.Any())))
}

// severityOf maps a slog level to an OpenTelemetry severity number and
// text. slog's Debug (-4), Info (0), Warn (4) and Error (8) land on the first
// number of the matching range; levels in between keep their order inside the
// range, and levels outside the named ones fall into TRACE below Debug and
// FATAL above Error+3.
func severityOf(level slog.Level) (int, string) {
	switch {
	case level < slog.LevelDebug:
		return severityTrace + int(min(max(level+8, 0), 3)), "TRACE"
	case level < slog.LevelInfo:
		return severityDebug + int(level-slog.LevelDebug), "DEBUG"
	case level < slog.LevelWarn:
		return severityInfo + int(level-slog.LevelInfo), "INFO"
	case level < slog.LevelError:
		return severityWarn + int(level-slog.LevelWarn), "WARN"
	case level < slog.LevelError+4:
		return severityError + int(level-slog.LevelError), "ERROR"
	}
	return severityFatal + int(min(level-slog.LevelError-4, 3)), "FATAL"
}

// exportLog queues one record without blocking.
func (t *Telemetry) exportLog(record LogRecord) {
	if t.logs == nil {
		return
	}
	if !t.logs.enqueue(record) {
		t.droppedLogs.Add(1)
	}
}
