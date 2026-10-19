package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

// TraceID identifies a trace: 16 bytes, never all zero when valid.
type TraceID [16]byte

// IsValid reports whether t is not all zero.
func (t TraceID) IsValid() bool { return t != TraceID{} }

// String returns the 32-character lowercase hex form used by W3C Trace
// Context and OTLP.
func (t TraceID) String() string { return hex.EncodeToString(t[:]) }

// SpanID identifies a span within a trace: 8 bytes, never all zero when valid.
type SpanID [8]byte

// IsValid reports whether s is not all zero.
func (s SpanID) IsValid() bool { return s != SpanID{} }

// String returns the 16-character lowercase hex form.
func (s SpanID) String() string { return hex.EncodeToString(s[:]) }

// TraceFlags are the W3C trace flags. Only the sampled bit is defined.
type TraceFlags byte

// FlagSampled marks a trace whose spans are recorded and exported.
const FlagSampled TraceFlags = 0x01

// SpanContext is the propagated part of a span: what crosses process
// boundaries in the traceparent and tracestate headers.
type SpanContext struct {
	TraceID TraceID
	SpanID  SpanID
	Flags   TraceFlags
	// TraceState is a validated, normalized W3C tracestate list, or "".
	// Forge forwards it unchanged and never interprets vendor entries.
	TraceState string
	// Remote is true for a span context extracted from an incoming request.
	Remote bool
}

// IsValid reports whether both identifiers are set.
func (sc SpanContext) IsValid() bool { return sc.TraceID.IsValid() && sc.SpanID.IsValid() }

// IsSampled reports whether the sampled flag is set.
func (sc SpanContext) IsSampled() bool { return sc.Flags&FlagSampled != 0 }

type contextKey int

const (
	spanKey contextKey = iota
	remoteKey
)

// ContextWithSpan returns ctx carrying span as the active span. A nil span
// returns ctx unchanged, so callers need no telemetry-enabled branch.
func ContextWithSpan(ctx context.Context, span *Span) context.Context {
	if span == nil {
		return ctx
	}
	return context.WithValue(ctx, spanKey, span)
}

// ContextWithRemoteSpanContext returns ctx carrying a span context received
// from another process, which becomes the parent of the next span started
// from ctx. An invalid span context returns ctx unchanged.
func ContextWithRemoteSpanContext(ctx context.Context, sc SpanContext) context.Context {
	if !sc.IsValid() {
		return ctx
	}
	sc.Remote = true
	return context.WithValue(ctx, remoteKey, sc)
}

// SpanFromContext returns the active span, or nil. All Span methods accept a
// nil receiver.
func SpanFromContext(ctx context.Context) *Span {
	if ctx == nil {
		return nil
	}
	span, _ := ctx.Value(spanKey).(*Span)
	return span
}

// SpanContextFromContext returns the span context of the active span, or the
// remote parent extracted from an incoming request, or the zero value.
func SpanContextFromContext(ctx context.Context) SpanContext {
	if ctx == nil {
		return SpanContext{}
	}
	if span, ok := ctx.Value(spanKey).(*Span); ok && span != nil {
		return span.sc
	}
	sc, _ := ctx.Value(remoteKey).(SpanContext)
	return sc
}

// newIDs returns a random trace ID and span ID from the operating system's
// CSPRNG. Trace IDs must be unpredictable: they correlate logs, and the
// ratio sampler reads their bits.
func newIDs() (TraceID, SpanID) {
	var buf [24]byte
	for {
		_, _ = rand.Read(buf[:]) // never fails since Go 1.24; it aborts the process instead.
		var trace TraceID
		var span SpanID
		copy(trace[:], buf[:16])
		copy(span[:], buf[16:])
		if trace.IsValid() && span.IsValid() {
			return trace, span
		}
	}
}

func newSpanID() SpanID {
	var span SpanID
	for !span.IsValid() {
		_, _ = rand.Read(span[:])
	}
	return span
}
