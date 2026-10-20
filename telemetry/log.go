package telemetry

import (
	"context"
	"log/slog"
	"time"
)

// Log attribute keys that correlate log records with traces.
const (
	LogTraceIDKey = "trace_id"
	LogSpanIDKey  = "span_id"
)

// NewLogHandler wraps next so that records logged with a context that
// carries a span (logger.InfoContext(ctx, ...)) get trace_id and span_id
// attributes. Records without a span, and loggers that already carry a
// trace_id (such as the request logger, see web.Logger), pass through
// unchanged. Attributes added inside a group (WithGroup) are nested in that
// group, as slog nests every attribute.
func NewLogHandler(next slog.Handler) slog.Handler {
	if handler, ok := next.(*logHandler); ok {
		return handler
	}
	return &logHandler{next: next}
}

type logHandler struct {
	next slog.Handler
	// correlated is set once a trace_id was attached with WithAttrs.
	correlated bool
}

func (h *logHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *logHandler) Handle(ctx context.Context, record slog.Record) error {
	if !h.correlated {
		if sc := SpanContextFromContext(ctx); sc.IsValid() {
			record.AddAttrs(slog.String(LogTraceIDKey, sc.TraceID.String()), slog.String(LogSpanIDKey, sc.SpanID.String()))
		}
	}
	return h.next.Handle(ctx, record)
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	correlated := h.correlated
	for _, attr := range attrs {
		if attr.Key == LogTraceIDKey {
			correlated = true
		}
	}
	return &logHandler{next: h.next.WithAttrs(attrs), correlated: correlated}
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	return &logHandler{next: h.next.WithGroup(name), correlated: h.correlated}
}

// LogOutput reports how the wrapped handler renders standard output (see
// LogOutputHandler).
func (h *logHandler) LogOutput() LogOutput { return logOutputOf(h.next) }

// LogAttrs returns the trace_id and span_id of ctx's span as logger
// arguments, or nil: logger.With(telemetry.LogAttrs(ctx)...).
func LogAttrs(ctx context.Context) []any {
	sc := SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []any{slog.String(LogTraceIDKey, sc.TraceID.String()), slog.String(LogSpanIDKey, sc.SpanID.String())}
}

// LogExporter writes each sampled span as one structured log record, for
// development (FORGE_TELEMETRY_EXPORTER=log). It is not meant for
// production volumes.
type LogExporter struct {
	logger *slog.Logger
}

// NewLogExporter returns an exporter writing to logger at Info level.
func NewLogExporter(logger *slog.Logger) *LogExporter {
	return &LogExporter{logger: logger}
}

// ExportSpans logs every span.
func (e *LogExporter) ExportSpans(ctx context.Context, _ Resource, spans []SpanData) error {
	for _, span := range spans {
		attrs := make([]any, 0, len(span.Attributes))
		for _, attr := range span.Attributes {
			attrs = append(attrs, slog.String(attr.Key, attr.Value.Emit()))
		}
		args := []any{
			slog.String("name", span.Name),
			slog.String("kind", span.Kind.String()),
			slog.String(LogTraceIDKey, span.SpanContext.TraceID.String()),
			slog.String(LogSpanIDKey, span.SpanContext.SpanID.String()),
			slog.Duration("duration", span.End.Sub(span.Start).Round(time.Microsecond)),
			slog.String("status", span.Status.String()),
		}
		if span.ParentSpanID.IsValid() {
			args = append(args, slog.String("parent_span_id", span.ParentSpanID.String()))
		}
		if len(attrs) > 0 {
			args = append(args, slog.Group("attributes", attrs...))
		}
		e.logger.Log(ctx, slog.LevelInfo, "span", args...)
	}
	return nil
}

// Shutdown does nothing; the logger belongs to the caller.
func (e *LogExporter) Shutdown(context.Context) error { return nil }
