package jobs

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/telemetry"
	"github.com/rocketinsidecorp/forge-go/telemetry/telemetrytest"
)

// A metric another component already defined under the scheduler's name with
// other labels is reported, once, and does not stop the scheduler: jobs still
// run and are still traced.
func TestSchedulerReportsAConflictingMetricAndKeepsRunningJobs(t *testing.T) {
	t.Parallel()
	tel, recorder := telemetrytest.New(t)
	if _, err := tel.Metrics().Histogram(MetricJobDuration, "defined first", "s", telemetry.DurationBuckets, "another.label"); err != nil {
		t.Fatal(err)
	}
	type report struct {
		job string
		err error
	}
	var mu sync.Mutex
	var reports []report
	s := NewScheduler(SchedulerOptions{Telemetry: tel, OnError: func(job string, err error) {
		mu.Lock()
		reports = append(reports, report{job, err})
		mu.Unlock()
	}})
	defer func() { _ = s.Stop() }()
	mu.Lock()
	got := append([]report(nil), reports...)
	mu.Unlock()
	if len(got) != 1 || got[0].job != "telemetry" || got[0].err == nil {
		t.Fatalf("reports = %+v, want one telemetry conflict", got)
	}
	if err := s.run(context.Background(), "cleanup", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("a job failed because the duration metric is missing: %v", err)
	}
	if spans := recorder.Spans(t, tel); len(spans) != 1 || spans[0].Name != "job cleanup" {
		t.Errorf("spans = %+v, want the job traced", spans)
	}
}

// A cron expression that never matches a real date is refused when the job is
// registered, so no job can be accepted that has no next run.
func TestRegisterRefusesACronThatNeverRuns(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerOptions{})
	defer func() { _ = s.Stop() }()
	err := s.Register("feb30", ScheduleSpec{Cron: "0 0 30 2 *"}, func(context.Context) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "no run in the next 8 years") {
		t.Errorf("Register = %v", err)
	}
}

// A job that is due while every slot is taken waits for one, and Stop does
// not wait for a slot that will never free up: it cancels the running job and
// the waiting one gives up.
func TestSchedulerStopReleasesAJobWaitingForASlot(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerOptions{MaxConcurrentJobs: 1})
	started := make(chan struct{}, 2)
	var runs atomic.Int32
	holdUntilCanceled := func(ctx context.Context) error {
		runs.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	for _, name := range []string{"first", "second"} {
		if err := s.Register(name, ScheduleSpec{Every: time.Second}, holdUntilCanceled); err != nil {
			t.Fatal(err)
		}
	}
	s.Start()
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("no job started")
	}
	// Both jobs share one boundary, so the other one is due now and queues.
	stopped := make(chan struct{})
	go func() { _ = s.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("Stop hung on a job waiting for a slot")
	}
	if got := runs.Load(); got != 1 {
		t.Errorf("%d jobs ran with one slot, want 1", got)
	}
}
