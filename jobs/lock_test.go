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
	"github.com/rocketinsidecorp/forge-go/jobs/locktest"
)

// memoryLocks is one in-process "database" of tick locks shared by the
// locks it hands out.
type memoryLocks struct {
	mu      sync.Mutex
	held    map[string]bool
	unlocks atomic.Int32
	fail    atomic.Bool
}

type memoryLock struct{ locks *memoryLocks }

func (l memoryLock) TryLock(_ context.Context, job string, tick time.Time) (func(context.Context) error, bool, error) {
	if l.locks.fail.Load() {
		return nil, false, errors.New("database unavailable")
	}
	key := job + "@" + tick.UTC().Format(time.RFC3339Nano)
	l.locks.mu.Lock()
	defer l.locks.mu.Unlock()
	if l.locks.held[key] {
		return nil, false, nil
	}
	l.locks.held[key] = true
	var once sync.Once
	return func(context.Context) error {
		once.Do(func() {
			l.locks.unlocks.Add(1)
			l.locks.mu.Lock()
			delete(l.locks.held, key)
			l.locks.mu.Unlock()
		})
		return nil
	}, true, nil
}

func newMemoryLocks() *memoryLocks { return &memoryLocks{held: map[string]bool{}} }

// The conformance suite holds for an in-process lock, so its scheduler
// scenario is checked without a database.
func TestLockConformanceInMemory(t *testing.T) {
	t.Parallel()
	locktest.Run(t, func(testing.TB) (jobs.Lock, jobs.Lock) {
		locks := newMemoryLocks()
		return memoryLock{locks}, memoryLock{locks}
	})
}

func TestSchedulerOptionsValidateLockWindow(t *testing.T) {
	t.Parallel()
	lock := memoryLock{newMemoryLocks()}
	for _, valid := range []jobs.SchedulerOptions{{}, {Lock: lock}, {Lock: lock, LockWindow: time.Second}, {Lock: lock, LockWindow: time.Hour}} {
		if err := valid.Validate(); err != nil {
			t.Errorf("%+v refused: %v", valid, err)
		}
	}
	for _, invalid := range []jobs.SchedulerOptions{{Lock: lock, LockWindow: -time.Second}, {Lock: lock, LockWindow: time.Hour + 1}, {LockWindow: time.Second}} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("%+v accepted", invalid)
		}
	}
}

// A tick whose lock cannot be checked is skipped (fail closed) and reported.
func TestATickIsSkippedWhenItsLockFails(t *testing.T) {
	t.Parallel()
	locks := newMemoryLocks()
	locks.fail.Store(true)
	var runs atomic.Int32
	reported := make(chan error, 8)
	s := jobs.NewScheduler(jobs.SchedulerOptions{Lock: memoryLock{locks}, OnError: func(_ string, err error) {
		select {
		case reported <- err:
		default:
		}
	}})
	if err := s.Register("tick", jobs.ScheduleSpec{Every: time.Second}, func(context.Context) error { runs.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	s.Start()
	defer func() { _ = s.Stop() }()
	select {
	case err := <-reported:
		if !strings.Contains(err.Error(), "skipped") || !strings.Contains(err.Error(), "database unavailable") {
			t.Fatalf("reported %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the lock failure was not reported")
	}
	if runs.Load() != 0 {
		t.Fatal("a tick ran without its lock")
	}
}

// The winner holds the tick's lock for the lock window, then releases it
// exactly once; stopping releases it at once.
func TestTheTickLockIsHeldForTheWindowAndReleasedOnStop(t *testing.T) {
	t.Parallel()
	locks := newMemoryLocks()
	ran := make(chan time.Time, 8)
	s := jobs.NewScheduler(jobs.SchedulerOptions{Lock: memoryLock{locks}, LockWindow: 400 * time.Millisecond})
	if err := s.Register("tick", jobs.ScheduleSpec{Every: 2 * time.Second}, func(context.Context) error { ran <- time.Now(); return nil }); err != nil {
		t.Fatal(err)
	}
	s.Start()
	var at time.Time
	select {
	case at = <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the job never ran")
	}
	time.Sleep(time.Until(at.Add(150 * time.Millisecond)))
	locks.mu.Lock()
	heldDuringWindow := len(locks.held)
	locks.mu.Unlock()
	if heldDuringWindow != 1 {
		t.Fatalf("%d locks held inside the window, want 1", heldDuringWindow)
	}
	time.Sleep(time.Until(at.Add(700 * time.Millisecond)))
	if locks.unlocks.Load() != 1 {
		t.Fatalf("%d unlocks after the window, want 1", locks.unlocks.Load())
	}

	// Stop while the next tick's lock is held: it is released promptly.
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the job did not run again")
	}
	began := time.Now()
	if err := s.StopContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 300*time.Millisecond {
		t.Fatalf("Stop waited %s for the lock window", took)
	}
	if locks.unlocks.Load() != 2 {
		t.Fatalf("%d unlocks after Stop, want 2", locks.unlocks.Load())
	}
}
