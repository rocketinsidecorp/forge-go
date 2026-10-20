package postgres

import (
	"context"
	"time"

	"github.com/rocketinsidecorp/forge-go/events"
	"github.com/rocketinsidecorp/forge-go/telemetry"
)

// MetricDispatchDuration is the histogram of outbox delivery attempts.
const MetricDispatchDuration = "forge.outbox.dispatch.duration"

type dispatchTelemetry struct {
	tracer   *telemetry.Tracer
	duration *telemetry.Histogram
}

func newDispatchTelemetry(t *telemetry.Telemetry) (*dispatchTelemetry, error) {
	if t == nil {
		return nil, nil
	}
	duration, err := t.Metrics().Histogram(MetricDispatchDuration, "Duration of outbox event delivery attempts.", "s", telemetry.DurationBuckets,
		"forge.event.type", "forge.outbox.outcome")
	if err != nil {
		return nil, err
	}
	return &dispatchTelemetry{tracer: t.Tracer(telemetry.ScopeName + "/outbox"), duration: duration}, nil
}

func (dt *dispatchTelemetry) start(event events.Event, attempt int) *telemetry.Span {
	if dt == nil {
		return nil
	}
	_, span := dt.tracer.Start(context.Background(), "process outbox", telemetry.SpanKindConsumer)
	if span.IsRecording() {
		span.SetAttributes(
			telemetry.String("messaging.system", "forge.outbox"),
			telemetry.String("messaging.operation.type", "process"),
			telemetry.String("messaging.message.id", event.ID.String()),
			telemetry.String("forge.event.type", event.Type),
			telemetry.Int("forge.event.version", event.Version),
			telemetry.Int("forge.outbox.attempt", attempt),
		)
	}
	return span
}

func (dt *dispatchTelemetry) finish(span *telemetry.Span, event events.Event, duration time.Duration, outcome string, handlerErr, err error) {
	if dt == nil {
		return
	}
	if span.IsRecording() {
		span.SetAttributes(telemetry.String("forge.outbox.outcome", outcome))
		if err != nil {
			span.RecordError(err)
		} else {
			span.RecordError(handlerErr)
		}
	}
	span.End()
	dt.duration.Record(duration.Seconds(), event.Type, outcome)
}
