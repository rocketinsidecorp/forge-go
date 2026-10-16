package observability

import (
	"context"
	"errors"

	"github.com/fabriciobonjorno/forge-go/jobs"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// TraceJob wraps a scheduled job in an internal span and propagates its
// context. jobName must be a stable registration name, not a per-run value.
// A generic failure event is recorded to avoid copying error details into the
// span; the scheduler remains responsible for reporting the original error.
func TraceJob(provider trace.TracerProvider, jobName string, job jobs.Job) jobs.Job {
	if job == nil {
		return nil
	}
	if provider == nil {
		provider = otel.GetTracerProvider()
	}
	tracer := provider.Tracer("github.com/fabriciobonjorno/forge-go/observability")
	return func(ctx context.Context) error {
		ctx, span := tracer.Start(
			ctx,
			"job "+jobName,
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(attribute.String("forge.job.name", jobName)),
		)
		defer span.End()

		err := job(ctx)
		if err != nil {
			span.RecordError(errors.New("scheduled job failed"))
			span.SetStatus(codes.Error, "scheduled job failed")
		}
		return err
	}
}
