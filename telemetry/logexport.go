package telemetry

import (
	"bytes"
	"context"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"reflect"
	"slices"
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

// LogFormat is how the log exporter encodes attribute values that are not
// strings, numbers, booleans, durations or times (slog's KindAny: structs,
// pointers, errors, maps...). It must match the handler that writes standard
// output, so the exporter never carries more than standard output shows.
type LogFormat uint8

const (
	// LogFormatJSON encodes like slog.JSONHandler, the production format:
	// an error by its message unless it implements json.Marshaler, any
	// other value with encoding/json, which honors MarshalJSON, MarshalText,
	// json:"-" and leaves unexported fields out. A value encoded as a JSON
	// string exports its text without the quotes.
	LogFormatJSON LogFormat = iota
	// LogFormatText encodes like slog.TextHandler, the development format:
	// AppendText or MarshalText when the value has one, a byte slice as its
	// text, anything else as fmt's %+v (which uses Error and String).
	LogFormatText
)

// LogOutput describes how the handler that writes standard output renders a
// record, so that the log exporter never carries more than that output
// shows.
type LogOutput struct {
	// Format is the standard-output handler's encoding.
	Format LogFormat
	// ReplaceAttr is the standard-output handler's
	// slog.HandlerOptions.ReplaceAttr, or nil. The exporter applies it as
	// slog does: to the message (key slog.MessageKey, no groups) and to every
	// attribute, after resolving its slog.LogValuer and again on what it
	// returns, with the open groups, never to a group itself; an attribute
	// it returns empty is dropped.
	ReplaceAttr func(groups []string, a slog.Attr) slog.Attr
}

// LogOutputHandler is implemented by a standard-output handler that reports
// how it renders records. FromConfig, and so forge.New, reads it from the
// logger it is given to configure the log exporter. NewOutputHandler's
// result and NewLogHandler's (which reports what the handler it wraps
// reports) implement it; a custom wrapper around slog.TextHandler, or around
// a handler with a ReplaceAttr, should implement it too, or the exporter
// encodes as slog.JSONHandler does and does not apply the ReplaceAttr.
type LogOutputHandler interface {
	slog.Handler
	LogOutput() LogOutput
}

// NewOutputHandler returns the standard-output handler that writes to w in
// format with opts (nil means slog's defaults), as slog.NewJSONHandler or
// slog.NewTextHandler would, wrapped by NewLogHandler for trace correlation.
// It reports format and opts.ReplaceAttr through LogOutputHandler, so the log
// exporter that forge.New tees the logger into applies the same ReplaceAttr:
//
//	logger := slog.New(telemetry.NewOutputHandler(os.Stdout, telemetry.LogFormatJSON,
//		&slog.HandlerOptions{ReplaceAttr: redact}))
//	app, err := forge.New(forge.WithLogger(logger))
func NewOutputHandler(w io.Writer, format LogFormat, opts *slog.HandlerOptions) slog.Handler {
	output := LogOutput{Format: format}
	if opts != nil {
		output.ReplaceAttr = opts.ReplaceAttr
	}
	var handler slog.Handler
	if format == LogFormatText {
		handler = slog.NewTextHandler(w, opts)
	} else {
		output.Format = LogFormatJSON
		handler = slog.NewJSONHandler(w, opts)
	}
	return NewLogHandler(outputHandler{Handler: handler, output: output})
}

// outputHandler is a standard-output handler that reports its LogOutput.
type outputHandler struct {
	slog.Handler
	output LogOutput
}

func (h outputHandler) LogOutput() LogOutput { return h.output }

func (h outputHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return outputHandler{Handler: h.Handler.WithAttrs(attrs), output: h.output}
}

func (h outputHandler) WithGroup(name string) slog.Handler {
	return outputHandler{Handler: h.Handler.WithGroup(name), output: h.output}
}

// logOutputOf returns how handler renders standard output: what it reports
// as a LogOutputHandler, text without ReplaceAttr for a bare
// slog.TextHandler, and otherwise JSON without ReplaceAttr, the production
// format.
func logOutputOf(handler slog.Handler) LogOutput {
	switch h := handler.(type) {
	case LogOutputHandler:
		return h.LogOutput()
	case *slog.TextHandler:
		return LogOutput{Format: LogFormatText}
	}
	return LogOutput{Format: LogFormatJSON}
}

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
// records after it returns. Shutdown is called once, after the last export
// (see SpanExporter).
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
// The record carries what the standard-output handler writes and nothing
// more: message and attributes, flattened with dotted group names, with
// values resolved through slog.LogValuer (config.Secret therefore exports its
// redaction, not its value), rewritten by Options.LogReplaceAttr as slog
// rewrites them with HandlerOptions.ReplaceAttr, and other values encoded as
// Options.LogFormat says, the way slog.JSONHandler (the default) or
// slog.TextHandler encodes them; a value that fails to encode exports a
// placeholder naming its type. The record's time and level are exported as
// logged, whatever LogReplaceAttr does with the time and level keys. The
// trace context comes from the ctx given to InfoContext and friends, or from
// trace_id and span_id attributes (the request logger's).
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
	// groups is the current group path (WithGroup), passed to
	// LogReplaceAttr; prefix is the same path joined with dots, with a
	// trailing dot, or "".
	groups []string
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
	}
	body := h.telemetry.logBody(record.Message)
	out.Body = truncate(body, MaxLogBodyLength)
	if len(body) <= MaxLogBodyLength {
		// truncate copies only what it cuts; a short message may still
		// share a larger string's memory.
		out.Body = strings.Clone(out.Body)
	}
	if out.Time.IsZero() {
		out.Time = now
	}
	attrs := make([]Attribute, 0, len(h.attrs)+record.NumAttrs())
	attrs = append(attrs, h.attrs...)
	record.Attrs(func(attr slog.Attr) bool {
		attrs = h.telemetry.appendLogAttr(attrs, h.groups, h.prefix, attr)
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
		next.attrs = h.telemetry.appendLogAttr(next.attrs, h.groups, h.prefix, attr)
	}
	return &next
}

func (h *otlpLogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.groups = append(slices.Clip(h.groups), name)
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

// logBody returns the record body for message: the message itself, or with
// LogReplaceAttr what standard output shows under the message key, which is
// empty when LogReplaceAttr removes the message or turns it into a group.
func (t *Telemetry) logBody(message string) string {
	if t.logReplace == nil {
		return message
	}
	attr := t.logReplace(nil, slog.String(slog.MessageKey, message))
	attr.Value = attr.Value.Resolve()
	switch {
	case isEmptyLogAttr(attr) || attr.Value.Kind() == slog.KindGroup:
		return ""
	case attr.Value.Kind() == slog.KindString:
		return attr.Value.String() // bounded by MaxLogBodyLength, not by an attribute's limit
	case attr.Value.Kind() == slog.KindAny:
		return encodeLogValue(t.logFormat, attr.Value.Any())
	}
	return logAttribute(t.logFormat, "", attr.Value).Value.Emit()
}

// appendLogAttr flattens attr into dst as slog's built-in handlers write
// it: the value resolved through slog.LogValuer, then rewritten by
// LogReplaceAttr with the open groups (never a group itself) and resolved
// again; empty attributes and empty groups are left out, and a group with an
// empty key is inlined. Groups become dotted prefixes ("http.status"), as
// OpenTelemetry has no nested attribute maps in Forge's encoding.
func (t *Telemetry) appendLogAttr(dst []Attribute, groups []string, prefix string, attr slog.Attr) []Attribute {
	attr.Value = attr.Value.Resolve()
	if t.logReplace != nil && attr.Value.Kind() != slog.KindGroup {
		// Clipped, so a ReplaceAttr that appends cannot write into the
		// handler's group path.
		attr = t.logReplace(slices.Clip(groups), attr)
		// ReplaceAttr may return an unresolved value.
		attr.Value = attr.Value.Resolve()
	}
	if isEmptyLogAttr(attr) {
		return dst
	}
	if attr.Value.Kind() == slog.KindGroup {
		members := attr.Value.Group()
		if len(members) == 0 {
			return dst
		}
		if attr.Key != "" {
			groups = append(slices.Clip(groups), attr.Key)
			prefix += attr.Key + "."
		}
		for _, member := range members {
			dst = t.appendLogAttr(dst, groups, prefix, member)
		}
		return dst
	}
	return append(dst, logAttribute(t.logFormat, truncate(prefix+attr.Key, maxLogKeyLength), attr.Value))
}

// isEmptyLogAttr reports whether slog's handlers ignore attr: an empty key
// with a nil value, which is what a zero slog.Attr holds.
func isEmptyLogAttr(attr slog.Attr) bool {
	return attr.Key == "" && attr.Value.Kind() == slog.KindAny && attr.Value.Any() == nil
}

// logAttribute converts a resolved value that is not a group.
func logAttribute(format LogFormat, key string, value slog.Value) Attribute {
	switch value.Kind() {
	case slog.KindString:
		return String(key, value.String())
	case slog.KindInt64:
		return Int64(key, value.Int64())
	case slog.KindUint64:
		if u := value.Uint64(); u <= math.MaxInt64 {
			return Int64(key, int64(u))
		}
		return String(key, strconv.FormatUint(value.Uint64(), 10))
	case slog.KindFloat64:
		return Float64(key, value.Float64())
	case slog.KindBool:
		return Bool(key, value.Bool())
	case slog.KindDuration:
		return String(key, value.Duration().String())
	case slog.KindTime:
		return String(key, value.Time().Format(time.RFC3339Nano))
	}
	return String(key, encodeLogValue(format, value.Any()))
}

// encodeLogValue encodes a KindAny value as the standard-output handler of
// format does. Like slog's handlers, it survives a panicking method: a nil
// pointer reads "<nil>", any other panic a placeholder without the panic's
// message, which could quote the value.
func encodeLogValue(format LogFormat, v any) (encoded string) {
	defer func() {
		if recover() != nil {
			if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer && rv.IsNil() {
				encoded = "<nil>"
				return
			}
			encoded = "!PANIC: encoding " + fmt.Sprintf("%T", v)
		}
	}()
	if format == LogFormatText {
		return encodeTextLogValue(v)
	}
	return encodeJSONLogValue(v)
}

// encodeJSONLogValue mirrors slog.JSONHandler.
func encodeJSONLogValue(v any) string {
	if err, ok := v.(error); ok {
		if _, marshaler := v.(json.Marshaler); !marshaler {
			return err.Error()
		}
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		// The error can quote what a MarshalJSON method returned.
		return unencodableLogValue(v)
	}
	data := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	var text string
	if len(data) > 0 && data[0] == '"' && json.Unmarshal(data, &text) == nil {
		return text
	}
	return string(data)
}

// encodeTextLogValue mirrors slog.TextHandler.
func encodeTextLogValue(v any) string {
	switch value := v.(type) {
	case encoding.TextAppender:
		data, err := value.AppendText(nil)
		if err != nil {
			return unencodableLogValue(v)
		}
		return string(data)
	case encoding.TextMarshaler:
		data, err := value.MarshalText()
		if err != nil {
			return unencodableLogValue(v)
		}
		return string(data)
	}
	if t := reflect.TypeOf(v); t != nil && t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
		return string(reflect.ValueOf(v).Bytes())
	}
	return fmt.Sprintf("%+v", v)
}

// unencodableLogValue is the placeholder for a value whose encoding failed.
// It names the type only.
func unencodableLogValue(v any) string {
	return fmt.Sprintf("!ERROR: cannot encode %T", v)
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
