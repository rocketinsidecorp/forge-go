package telemetry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
)

// Span limits. Instrumentation never needs more; the limits bound the memory
// a span can hold when application code adds attributes in a loop.
const (
	MaxSpanAttributes = 64
	MaxSpanEvents     = 32
)

// SpanKind is the OpenTelemetry span kind; values match the OTLP enum.
type SpanKind uint8

const (
	SpanKindInternal SpanKind = iota + 1
	SpanKindServer
	SpanKindClient
	SpanKindProducer
	SpanKindConsumer
)

// String returns the lowercase kind name.
func (k SpanKind) String() string {
	switch k {
	case SpanKindServer:
		return "server"
	case SpanKindClient:
		return "client"
	case SpanKindProducer:
		return "producer"
	case SpanKindConsumer:
		return "consumer"
	}
	return "internal"
}

// StatusCode is a span status; values match the OTLP enum.
type StatusCode uint8

const (
	StatusUnset StatusCode = iota
	StatusOK
	StatusError
)

// String returns the lowercase status name.
func (c StatusCode) String() string {
	switch c {
	case StatusOK:
		return "ok"
	case StatusError:
		return "error"
	}
	return "unset"
}

// Event is a timestamped annotation on a span.
type Event struct {
	Name       string
	Time       time.Time
	Attributes []Attribute
}

// SpanData is an ended span as exporters receive it.
type SpanData struct {
	// Scope is the instrumentation scope, such as
	// "github.com/rocketinsidecorp/forge-go/httpserver".
	Scope             string
	SpanContext       SpanContext
	ParentSpanID      SpanID
	ParentRemote      bool
	Name              string
	Kind              SpanKind
	Start             time.Time
	End               time.Time
	Attributes        []Attribute
	Events            []Event
	Status            StatusCode
	StatusDescription string
	DroppedAttributes int
	DroppedEvents     int
}

// Tracer starts spans for one instrumentation scope. A nil *Tracer is valid
// and starts no spans, which is how disabled telemetry costs nothing.
type Tracer struct {
	telemetry *Telemetry
	scope     string
}

// Start begins a span named name as a child of the span (or remote parent)
// in ctx, or as a new trace's root. It returns ctx carrying the new span.
// With a nil Tracer it returns ctx and a nil span, whose methods are no-ops.
//
// A span is recording (it keeps attributes and is exported) only when its
// trace is sampled and an exporter is configured. A non-recording span still
// has identifiers, so logs are correlated and context is propagated.
func (tr *Tracer) Start(ctx context.Context, name string, kind SpanKind, attrs ...Attribute) (context.Context, *Span) {
	if tr == nil || tr.telemetry == nil {
		return ctx, nil
	}
	return tr.StartAt(ctx, time.Now(), name, kind, attrs...)
}

// StartAt is Start with an explicit start time, for instrumentation that
// learns only after an operation whether it deserves a span (a database
// driver answering driver.ErrSkip, for example).
func (tr *Tracer) StartAt(ctx context.Context, start time.Time, name string, kind SpanKind, attrs ...Attribute) (context.Context, *Span) {
	if tr == nil || tr.telemetry == nil {
		return ctx, nil
	}
	span := tr.telemetry.newSpan(tr, SpanContextFromContext(ctx), start, name, kind, attrs)
	return context.WithValue(ctx, spanKey, span), span
}

// StartChild starts a span only when ctx already carries a span started by a
// Forge Tracer, using that span's Telemetry. Library code that is not given a
// Telemetry (queue.Enqueue, for example) uses it to appear inside traces
// without creating root spans of its own.
func StartChild(ctx context.Context, scope, name string, kind SpanKind, attrs ...Attribute) (context.Context, *Span) {
	parent := SpanFromContext(ctx)
	if parent == nil || parent.tracer == nil {
		return ctx, nil
	}
	return parent.tracer.telemetry.Tracer(scope).Start(ctx, name, kind, attrs...)
}

// Span is one timed operation. All methods are safe for concurrent use and
// accept a nil receiver. Methods called after End have no effect.
type Span struct {
	tracer       *Tracer
	sc           SpanContext
	parent       SpanID
	parentRemote bool
	recording    bool

	mu            sync.Mutex
	ended         bool
	name          string
	kind          SpanKind
	start         time.Time
	attrs         []Attribute
	events        []Event
	status        StatusCode
	description   string
	droppedAttrs  int
	droppedEvents int
}

// SpanContext returns the span's propagated identity.
func (s *Span) SpanContext() SpanContext {
	if s == nil {
		return SpanContext{}
	}
	return s.sc
}

// IsRecording reports whether attributes and events are kept for export.
// Instrumentation checks it before computing expensive attributes.
func (s *Span) IsRecording() bool { return s != nil && s.recording }

// SetName replaces the span name, for example once the HTTP route is known.
func (s *Span) SetName(name string) {
	if !s.IsRecording() {
		return
	}
	s.mu.Lock()
	if !s.ended {
		s.name = name
	}
	s.mu.Unlock()
}

// SetAttributes adds attributes, replacing earlier values of the same keys.
// Attributes beyond MaxSpanAttributes are dropped and counted.
func (s *Span) SetAttributes(attrs ...Attribute) {
	if !s.IsRecording() || len(attrs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.addAttributes(attrs)
}

func (s *Span) addAttributes(attrs []Attribute) {
next:
	for _, attr := range attrs {
		if attr.Key == "" {
			continue
		}
		for i := range s.attrs {
			if s.attrs[i].Key == attr.Key {
				s.attrs[i] = attr
				continue next
			}
		}
		if len(s.attrs) >= MaxSpanAttributes {
			s.droppedAttrs++
			continue
		}
		s.attrs = append(s.attrs, attr)
	}
}

// AddEvent records a named, timestamped event. Events beyond MaxSpanEvents
// are dropped and counted.
func (s *Span) AddEvent(name string, attrs ...Attribute) {
	if !s.IsRecording() {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	if len(s.events) >= MaxSpanEvents {
		s.droppedEvents++
		return
	}
	if len(attrs) > MaxSpanAttributes {
		attrs = attrs[:MaxSpanAttributes]
	}
	s.events = append(s.events, Event{Name: name, Time: now, Attributes: append([]Attribute(nil), attrs...)})
}

// SetStatus sets the span status. An OK status is final; Unset is ignored.
// Keep the description free of user data: it is exported as is.
func (s *Span) SetStatus(code StatusCode, description string) {
	if !s.IsRecording() || code == StatusUnset {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended || s.status == StatusOK {
		return
	}
	s.status = code
	s.description = ""
	if code == StatusError {
		s.description = truncate(description, MaxAttributeValueLength)
	}
}

// RecordError marks the span failed with the error's type (see ErrorType).
// The error message is not recorded: messages routinely embed identifiers,
// SQL values or addresses that must not leave the process.
func (s *Span) RecordError(err error) {
	if err == nil || !s.IsRecording() {
		return
	}
	kind := ErrorType(err)
	s.SetAttributes(String("error.type", kind))
	s.SetStatus(StatusError, kind)
}

// End completes the span and hands it to the exporter if it is recording.
// Only the first call has an effect.
func (s *Span) End() {
	if s == nil {
		return
	}
	end := time.Now()
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	if !s.recording {
		s.mu.Unlock()
		return
	}
	data := SpanData{
		Scope:             s.tracer.scope,
		SpanContext:       s.sc,
		ParentSpanID:      s.parent,
		ParentRemote:      s.parentRemote,
		Name:              s.name,
		Kind:              s.kind,
		Start:             s.start,
		End:               end,
		Attributes:        s.attrs,
		Events:            s.events,
		Status:            s.status,
		StatusDescription: s.description,
		DroppedAttributes: s.droppedAttrs,
		DroppedEvents:     s.droppedEvents,
	}
	s.attrs, s.events = nil, nil
	s.mu.Unlock()
	s.tracer.telemetry.exportSpan(data)
}

// ErrorType returns a low-cardinality description of err that is safe to
// export: the code of a fault.Error, "timeout" or "canceled" for context
// errors, and otherwise the Go type of the innermost wrapped error. It never
// includes the error message.
func ErrorType(err error) string {
	if err == nil {
		return ""
	}
	if f := fault.From(err); f != nil && f.Code != "" {
		return f.Code
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return fmt.Sprintf("%T", err)
		}
		err = next
	}
}
