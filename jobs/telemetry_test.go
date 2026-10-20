package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/telemetry"
	"github.com/rocketinsidecorp/forge-go/telemetry/telemetrytest"
)

func TestSchedulerTelemetry(t *testing.T) {
	tel, recorder := telemetrytest.New(t)
	s := NewScheduler(SchedulerOptions{Telemetry: tel})
	defer func() { _ = s.Stop() }()
	ctx := context.Background()
	if err := s.run(ctx, "cleanup", func(ctx context.Context) error {
		if !telemetry.SpanFromContext(ctx).IsRecording() {
			t.Error("jobs must run inside their span")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.run(ctx, "cleanup", func(context.Context) error { return errors.New("disk full at /secret/path") }); err == nil {
		t.Fatal("job errors must be returned")
	}
	short, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	_ = s.run(short, "report", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })

	series := telemetrytest.Series(tel, MetricJobDuration)
	for _, key := range []string{
		"forge.scheduler.job=cleanup,forge.scheduler.outcome=succeeded",
		"forge.scheduler.job=cleanup,forge.scheduler.outcome=failed",
		"forge.scheduler.job=report,forge.scheduler.outcome=timeout",
	} {
		if series[key] != 1 {
			t.Fatalf("series %s missing: %v", key, series)
		}
	}
	spans := recorder.Spans(t, tel)
	if len(spans) != 3 || spans[0].Name != "job cleanup" || spans[1].Status != telemetry.StatusError {
		t.Fatalf("spans %+v", spans)
	}
	for _, attr := range spans[1].Attributes {
		if attr.Value.Emit() == "disk full at /secret/path" {
			t.Fatal("error message recorded")
		}
	}

	plain := NewScheduler(SchedulerOptions{})
	defer func() { _ = plain.Stop() }()
	if err := plain.run(ctx, "x", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
