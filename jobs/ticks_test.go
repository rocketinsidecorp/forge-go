package jobs_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/jobs"
	"github.com/rocketinsidecorp/forge-go/jobs/ticktest"
)

// memoryTicks is one in-process "database" of recorded ticks, with the
// process clock as the database clock.
type memoryTicks struct {
	mu     sync.Mutex
	latest map[string]time.Time
	claims map[string]int // attempts per job
}

func newMemoryTicks() *memoryTicks {
	return &memoryTicks{latest: map[string]time.Time{}, claims: map[string]int{}}
}

func (m *memoryTicks) ClaimTick(_ context.Context, job string, tick time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claims[job]++
	if wait := time.Until(tick); wait > 0 {
		return false, &jobs.EarlyTickError{Wait: wait}
	}
	if last, ok := m.latest[job]; ok && !last.Before(tick) {
		return false, nil
	}
	m.latest[job] = tick
	return true, nil
}

func (m *memoryTicks) attempts(job string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claims[job]
}

// The conformance suite holds for an in-process claimer, so its scheduler
// scenario is checked without a database.
func TestTickClaimerConformanceInMemory(t *testing.T) {
	t.Parallel()
	ticktest.Run(t, func(testing.TB) (jobs.TickClaimer, jobs.TickClaimer) {
		ticks := newMemoryTicks()
		return ticks, ticks
	})
}

type scriptedTicks struct {
	claim func(attempt int) (bool, error)
	calls atomic.Int32
}

func (s *scriptedTicks) ClaimTick(context.Context, string, time.Time) (bool, error) {
	return s.claim(int(s.calls.Add(1)))
}

func scheduleTick(t *testing.T, ticks jobs.TickClaimer, onError func(string, error), job jobs.Job) *jobs.Scheduler {
	t.Helper()
	s := jobs.NewScheduler(jobs.SchedulerOptions{Ticks: ticks, OnError: onError})
	if err := s.Register("tick", jobs.ScheduleSpec{Every: time.Second}, job); err != nil {
		t.Fatal(err)
	}
	s.Start()
	t.Cleanup(func() { _ = s.Stop() })
	return s
}

// A tick that cannot be recorded is skipped (fail closed) and reported.
func TestATickIsSkippedWhenItCannotBeRecorded(t *testing.T) {
	t.Parallel()
	ticks := &scriptedTicks{claim: func(int) (bool, error) { return false, errors.New("database unavailable") }}
	var runs atomic.Int32
	reported := make(chan error, 8)
	scheduleTick(t, ticks, func(_ string, err error) {
		select {
		case reported <- err:
		default:
		}
	}, func(context.Context) error { runs.Add(1); return nil })
	select {
	case err := <-reported:
		if !strings.Contains(err.Error(), "skipped") || !strings.Contains(err.Error(), "database unavailable") {
			t.Fatalf("reported %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the failure to record a tick was not reported")
	}
	if runs.Load() != 0 {
		t.Fatal("a tick ran without being recorded")
	}
}

// A tick another replica recorded is skipped silently: it is not a failure.
func TestATickClaimedElsewhereIsNotAnError(t *testing.T) {
	t.Parallel()
	ticks := &scriptedTicks{claim: func(int) (bool, error) { return false, nil }}
	var runs, errs atomic.Int32
	scheduleTick(t, ticks, func(string, error) { errs.Add(1) }, func(context.Context) error { runs.Add(1); return nil })
	deadline := time.Now().Add(30 * time.Second)
	for ticks.calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no ticks were claimed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runs.Load() != 0 || errs.Load() != 0 {
		t.Fatalf("runs=%d errors=%d for ticks claimed elsewhere; want 0 and 0", runs.Load(), errs.Load())
	}
}

// A replica whose clock is ahead of the database waits for it and then runs
// the tick once.
func TestAnEarlyTickWaitsForTheDatabaseClock(t *testing.T) {
	t.Parallel()
	ticks := &scriptedTicks{claim: func(attempt int) (bool, error) {
		if attempt == 1 {
			return false, &jobs.EarlyTickError{Wait: 20 * time.Millisecond}
		}
		return attempt == 2, nil
	}}
	ran := make(chan struct{}, 8)
	var errs atomic.Int32
	scheduleTick(t, ticks, func(string, error) { errs.Add(1) }, func(context.Context) error { ran <- struct{}{}; return nil })
	select {
	case <-ran:
	case <-time.After(30 * time.Second):
		t.Fatal("the early tick never ran")
	}
	if errs.Load() != 0 {
		t.Fatal("an early tick was reported as an error")
	}
}

// A replica waits for a concurrency slot before claiming, so it never claims
// a tick it cannot run now.
func TestTicksAreClaimedOnlyWithAFreeSlot(t *testing.T) {
	t.Parallel()
	ticks := newMemoryTicks()
	release := make(chan struct{})
	blocked := make(chan struct{})
	var once sync.Once
	s := jobs.NewScheduler(jobs.SchedulerOptions{MaxConcurrentJobs: 1, Ticks: ticks})
	for _, register := range []struct {
		name  string
		every time.Duration
		job   jobs.Job
	}{
		// Its run times out after its interval, 3s, freeing the slot.
		{"blocker", 3 * time.Second, func(ctx context.Context) error {
			once.Do(func() { close(blocked) })
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		}},
		{"other", time.Second, func(context.Context) error { return nil }},
	} {
		if err := s.Register(register.name, jobs.ScheduleSpec{Every: register.every}, register.job); err != nil {
			t.Fatal(err)
		}
	}
	s.Start()
	defer func() { _ = s.Stop() }()
	select {
	case <-blocked:
	case <-time.After(30 * time.Second):
		t.Fatal("the blocking job never started")
	}
	before := ticks.attempts("other")
	time.Sleep(1200 * time.Millisecond) // a tick of "other" passes while the slot is taken
	if after := ticks.attempts("other"); after != before {
		t.Fatalf("the replica claimed %d tick(s) of another job while its only slot was taken", after-before)
	}
	close(release)
}

func TestRegisterRefusesAJobNameTooLongToRecord(t *testing.T) {
	t.Parallel()
	s := jobs.NewScheduler(jobs.SchedulerOptions{})
	if err := s.Register(strings.Repeat("j", jobs.MaxJobNameLength+1), jobs.ScheduleSpec{Every: time.Second}, func(context.Context) error { return nil }); err == nil {
		t.Fatal("an over-long job name was accepted")
	}
	if err := s.Register(strings.Repeat("j", jobs.MaxJobNameLength), jobs.ScheduleSpec{Every: time.Second}, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
