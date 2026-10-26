package jobs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerRunsJobsOnInterval(t *testing.T) {
	var counter atomic.Int32
	s := NewScheduler(SchedulerOptions{MaxConcurrentJobs: 2})
	err := s.Register("counter", ScheduleSpec{Every: 1500 * time.Millisecond}, func(ctx context.Context) error {
		counter.Add(1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	time.Sleep(4500 * time.Millisecond)
	err = s.Stop()
	if err != nil && err != context.Canceled {
		t.Fatalf("Stop error: %v", err)
	}
	count := counter.Load()
	if count < 2 {
		t.Fatalf("expected at least 2 runs, got %d", count)
	}
}

func TestScheduleSpecNextDailyAt(t *testing.T) {
	at := DailyTime{Hour: 12, Minute: 30}
	spec := ScheduleSpec{DailyAt: &at}
	tests := []struct {
		name  string
		after time.Time
		want  time.Time
	}{
		{
			name:  "later today",
			after: time.Date(2026, time.October, 4, 12, 0, 0, 0, time.UTC),
			want:  time.Date(2026, time.October, 4, 12, 30, 0, 0, time.UTC),
		},
		{
			name:  "tomorrow after today's run",
			after: time.Date(2026, time.October, 4, 12, 30, 0, 0, time.UTC),
			want:  time.Date(2026, time.October, 5, 12, 30, 0, 0, time.UTC),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := spec.nextRun(test.after)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(test.want) {
				t.Fatalf("next run = %s, want %s", got, test.want)
			}
		})
	}
}

func TestSchedulerValidatesSchedule(t *testing.T) {
	s := NewScheduler(SchedulerOptions{})
	// No schedule
	if err := s.Register("bad", ScheduleSpec{}, func(context.Context) error { return nil }); err == nil {
		t.Fatal("expected error for empty schedule")
	}
	// Multiple schedules
	if err := s.Register("bad", ScheduleSpec{Every: time.Second, DailyAt: &DailyTime{Hour: 1}}, func(context.Context) error { return nil }); err == nil {
		t.Fatal("expected error for multiple schedules")
	}
	// Invalid Every
	if err := s.Register("bad", ScheduleSpec{Every: 100 * time.Millisecond}, func(context.Context) error { return nil }); err == nil {
		t.Fatal("expected error for Every < 1 second")
	}
	// Valid Every
	if err := s.Register("good", ScheduleSpec{Every: time.Second}, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("unexpected error for valid schedule: %v", err)
	}
	if err := s.Register("cron", ScheduleSpec{Cron: "*/15 9-17 * * 1-5"}, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("unexpected error for valid cron schedule: %v", err)
	}
	for _, expression := range []string{"", "* * *", "60 * * * *", "*/0 * * * *", "1-4-6 * * * *", "0 0 31 2 *"} {
		if err := s.Register("invalid-cron-"+expression, ScheduleSpec{Cron: expression}, func(context.Context) error { return nil }); err == nil {
			t.Errorf("accepted invalid cron expression %q", expression)
		}
	}
}

func TestSchedulerConcurrencyBound(t *testing.T) {
	var running atomic.Int32
	var maxRunning atomic.Int32
	s := NewScheduler(SchedulerOptions{MaxConcurrentJobs: 2})
	err := s.Register("slow", ScheduleSpec{Every: 1500 * time.Millisecond}, func(ctx context.Context) error {
		current := running.Add(1)
		for {
			if current > maxRunning.Load() {
				maxRunning.Store(current)
			}
			select {
			case <-ctx.Done():
				running.Add(-1)
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
				running.Add(-1)
				return nil
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	time.Sleep(4500 * time.Millisecond)
	err = s.Stop()
	if err != nil && err != context.Canceled {
		t.Fatalf("Stop error: %v", err)
	}
	if maxRunning.Load() > 2 {
		t.Fatalf("concurrency exceeded bound: max running = %d", maxRunning.Load())
	}
}

func TestSchedulerEnforcesGlobalConcurrencyAndCanStartOnlyOnce(t *testing.T) {
	var running atomic.Int32
	var peak atomic.Int32
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	s := NewScheduler(SchedulerOptions{MaxConcurrentJobs: 2})
	for _, name := range []string{"one", "two", "three"} {
		if err := s.Register(name, ScheduleSpec{Every: 3 * time.Second}, func(ctx context.Context) error {
			current := running.Add(1)
			for previous := peak.Load(); current > previous; previous = peak.Load() {
				if peak.CompareAndSwap(previous, current) {
					break
				}
			}
			started <- struct{}{}
			select {
			case <-ctx.Done():
			case <-release:
			}
			running.Add(-1)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	s.Start()
	s.Start()
	if err := s.Register("late", ScheduleSpec{Every: time.Second}, func(context.Context) error { return nil }); err == nil {
		t.Fatal("registered a job after Start")
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(4 * time.Second):
			t.Fatal("scheduler did not start two jobs")
		}
	}
	select {
	case <-started:
		t.Fatal("scheduler exceeded MaxConcurrentJobs")
	case <-time.After(1100 * time.Millisecond):
	}
	close(release)
	select {
	case <-started:
	case <-time.After(4 * time.Second):
		t.Fatal("queued job did not start after a worker became available")
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got > 2 {
		t.Fatalf("peak concurrency = %d, want at most 2", got)
	}
}

func TestSchedulerReportsJobErrors(t *testing.T) {
	want := errors.New("job failed")
	errorsSeen := make(chan error, 8)
	s := NewScheduler(SchedulerOptions{
		MaxConcurrentJobs: 1,
		OnError:           func(_ string, err error) { errorsSeen <- err },
	})
	if err := s.Register("broken", ScheduleSpec{Every: time.Second}, func(context.Context) error { return want }); err != nil {
		t.Fatal(err)
	}
	s.Start()
	select {
	case got := <-errorsSeen:
		if !errors.Is(got, want) {
			t.Fatalf("reported error = %v, want %v", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("job failure was not reported")
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestNextCronRun(t *testing.T) {
	after := time.Date(2026, time.October, 5, 8, 29, 0, 0, time.UTC) // Monday.
	got, err := nextCronRun("30 8 * * 1-5", after)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, time.October, 5, 8, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next cron run = %s, want %s", got, want)
	}

	got, err = nextCronRun("0 1 * * 7", time.Date(2026, time.October, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want = time.Date(2026, time.October, 4, 1, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Sunday alias next run = %s, want %s", got, want)
	}

	if _, err := nextCronRun("0 0 31 2 *", after); err == nil {
		t.Fatal("impossible calendar schedule should return an error")
	}
}

func TestSchedulerOptionsValidate(t *testing.T) {
	t.Parallel()
	for _, valid := range []int{0, 1, 4, 256} {
		if err := (SchedulerOptions{MaxConcurrentJobs: valid}).Validate(); err != nil {
			t.Errorf("MaxConcurrentJobs %d refused: %v", valid, err)
		}
	}
	for _, invalid := range []int{-1, 257, 1 << 20} {
		if err := (SchedulerOptions{MaxConcurrentJobs: invalid}).Validate(); err == nil {
			t.Errorf("MaxConcurrentJobs %d accepted", invalid)
		}
	}
	// NewScheduler keeps its clamping, so existing callers are unaffected.
	if got := cap(NewScheduler(SchedulerOptions{MaxConcurrentJobs: -3}).concurrent); got != 4 {
		t.Errorf("negative: %d slots, want the default 4", got)
	}
	if got := cap(NewScheduler(SchedulerOptions{MaxConcurrentJobs: 1000}).concurrent); got != 256 {
		t.Errorf("too large: %d slots, want 256", got)
	}
}
