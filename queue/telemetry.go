package queue

import (
	"context"
	"time"

	"github.com/rocketinsidecorp/forge-go/telemetry"
)

// Telemetry names. The job ID is a span attribute on both the enqueue and
// the processing span, which is how a trace backend links the two: job rows
// do not store the enqueuing trace context (see docs/OBSERVABILITY.md).
const (
	MetricJobDuration = "forge.queue.job.duration"
	messagingSystem   = "forge.queue"
	queueScope        = telemetry.ScopeName + "/queue"
)

// outcomeStoreFailed labels attempts whose outcome could not be stored; the
// job runs again after its lease expires.
const outcomeStoreFailed Outcome = "store_failed"

type workerTelemetry struct {
	tracer   *telemetry.Tracer
	duration *telemetry.Histogram
}

func newWorkerTelemetry(t *telemetry.Telemetry) (*workerTelemetry, error) {
	if t == nil {
		return nil, nil
	}
	duration, err := t.Metrics().Histogram(MetricJobDuration, "Duration of queue job attempts.", "s", telemetry.DurationBuckets,
		"messaging.destination.name", "forge.job.kind", "forge.job.outcome")
	if err != nil {
		return nil, err
	}
	return &workerTelemetry{tracer: t.Tracer(queueScope), duration: duration}, nil
}

// start begins the processing span, the root of a new trace.
func (wt *workerTelemetry) start(job Job) *telemetry.Span {
	if wt == nil {
		return nil
	}
	_, span := wt.tracer.Start(context.Background(), "process "+job.Queue, telemetry.SpanKindConsumer)
	if span.IsRecording() {
		span.SetAttributes(
			telemetry.String("messaging.system", messagingSystem),
			telemetry.String("messaging.operation.type", "process"),
			telemetry.String("messaging.destination.name", job.Queue),
			telemetry.String("messaging.message.id", job.ID.String()),
			telemetry.String("forge.job.kind", job.Kind),
			telemetry.Int("forge.job.attempt", job.Attempt),
		)
	}
	return span
}

func (wt *workerTelemetry) finish(span *telemetry.Span, job Job, duration time.Duration, outcome Outcome, err error) {
	if wt == nil {
		return
	}
	if span.IsRecording() {
		span.SetAttributes(telemetry.String("forge.job.outcome", string(outcome)))
		switch {
		case err != nil && outcome != OutcomeSucceeded && outcome != OutcomeReleased:
			span.RecordError(err)
		case outcome == outcomeStoreFailed || outcome == OutcomeLeaseLost:
			span.SetStatus(telemetry.StatusError, string(outcome))
		}
	}
	span.End()
	wt.duration.Record(duration.Seconds(), job.Queue, job.Kind, string(outcome))
}

// startEnqueue starts a producer span when ctx is already traced.
func startEnqueue(ctx context.Context, job NewJob) (context.Context, *telemetry.Span) {
	ctx, span := telemetry.StartChild(ctx, queueScope, "publish "+job.Queue, telemetry.SpanKindProducer)
	if span.IsRecording() {
		span.SetAttributes(
			telemetry.String("messaging.system", messagingSystem),
			telemetry.String("messaging.operation.type", "send"),
			telemetry.String("messaging.destination.name", job.Queue),
			telemetry.String("forge.job.kind", job.Kind),
		)
	}
	return ctx, span
}
