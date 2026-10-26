package jobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

func startedJob(t *testing.T, s *Scheduler, job Job) {
	t.Helper()
	running := make(chan struct{})
	once := make(chan struct{}, 1)
	if err := s.Register("probe", ScheduleSpec{Every: time.Second}, func(ctx context.Context) error {
		select {
		case once <- struct{}{}:
			close(running)
		default:
		}
		return job(ctx)
	}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	select {
	case <-running:
	case <-time.After(10 * time.Second):
		t.Fatal("the job never started")
	}
}

// A job that ignores cancellation must not hold the shutdown past its budget.
func TestStopContextReturnsTheDeadlineWhileAJobIgnoresCancellation(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	finished := make(chan struct{})
	s := NewScheduler(SchedulerOptions{})
	startedJob(t, s, func(context.Context) error {
		defer close(finished)
		<-release // ignores ctx
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	begin := time.Now()
	if err := s.StopContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StopContext = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(begin); elapsed > 5*time.Second {
		t.Fatalf("StopContext took %v despite a 50ms budget", elapsed)
	}
	// The scheduler stays stopped, and a later call waits for the same job.
	s.Start()
	close(release)
	<-finished
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	if err := s.StopContext(waitCtx); err != nil {
		t.Fatalf("StopContext after the job returned = %v, want nil", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop after StopContext = %v", err)
	}
}

func TestStopContextCancelsCooperativeJobsAndReturnsNil(t *testing.T) {
	t.Parallel()
	cancelled := make(chan struct{})
	s := NewScheduler(SchedulerOptions{})
	startedJob(t, s, func(ctx context.Context) error {
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.StopContext(ctx); err != nil {
		t.Fatalf("StopContext = %v, want nil", err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("StopContext returned before the job observed cancellation")
	}
}

// Stop keeps its contract: it waits for as long as the jobs take.
func TestStopStillWaitsForRunningJobs(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	s := NewScheduler(SchedulerOptions{})
	startedJob(t, s, func(context.Context) error {
		<-release
		return nil
	})
	stopped := make(chan error, 1)
	go func() { stopped <- s.Stop() }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned %v while a job was running", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return after the job finished")
	}
}

// With nothing running, an expired context is not a timeout, and the call may
// repeat.
func TestStopContextWithNoRunningJobsSucceedsWithAnExpiredContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := NewScheduler(SchedulerOptions{})
	for range 3 {
		if err := s.StopContext(ctx); err != nil {
			t.Fatalf("StopContext = %v, want nil", err)
		}
	}
}
