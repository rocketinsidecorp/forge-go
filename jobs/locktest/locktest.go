// Package locktest is the conformance suite every jobs.Lock must pass. It
// takes two locks on one database, as two replicas of an application would
// hold, and checks that they exclude each other per job and tick, that
// unlocking always releases, and that two schedulers sharing the database
// run each tick once.
//
//	func TestSchedulerLock(t *testing.T) {
//	    locktest.Run(t, func(t testing.TB) (jobs.Lock, jobs.Lock) { ... two locks on one fresh database ... })
//	}
package locktest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/jobs"
)

// NewLocks returns two locks on the same database, each with its own
// connections, which the test cleans up.
type NewLocks func(t testing.TB) (jobs.Lock, jobs.Lock)

// Run executes the conformance suite.
func Run(t *testing.T, newLocks NewLocks) {
	t.Helper()
	t.Run("ExclusivePerJobAndTick", func(t *testing.T) { exclusive(t, newLocks) })
	t.Run("UnlockReleasesEvenWithAnEndedContext", func(t *testing.T) { endedContext(t, newLocks) })
	t.Run("TwoSchedulersRunEachTickOnce", func(t *testing.T) { twoSchedulers(t, newLocks) })
}

func try(t *testing.T, lock jobs.Lock, job string, tick time.Time) (func(context.Context) error, bool) {
	t.Helper()
	unlock, acquired, err := lock.TryLock(context.Background(), job, tick)
	if err != nil {
		t.Fatalf("TryLock(%s, %s): %v", job, tick, err)
	}
	if acquired == (unlock == nil) {
		t.Fatalf("TryLock(%s, %s) acquired=%v with unlock=%v", job, tick, acquired, unlock != nil)
	}
	return unlock, acquired
}

func release(t *testing.T, unlock func(context.Context) error) {
	t.Helper()
	if err := unlock(context.Background()); err != nil {
		t.Fatalf("unlock: %v", err)
	}
}

func exclusive(t *testing.T, newLocks NewLocks) {
	a, b := newLocks(t)
	tick := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	held, ok := try(t, a, "report", tick)
	if !ok {
		t.Fatal("first TryLock was refused")
	}
	if _, ok := try(t, b, "report", tick); ok {
		t.Fatal("another replica took a held tick lock")
	}
	if _, ok := try(t, a, "report", tick.In(time.FixedZone("x", 3600))); ok {
		t.Fatal("the same instant in another time zone is not the same lock")
	}
	nextTick, ok := try(t, b, "report", tick.Add(time.Minute))
	if !ok {
		t.Fatal("the next tick of the job is locked by the previous one")
	}
	otherJob, ok := try(t, b, "cleanup", tick)
	if !ok {
		t.Fatal("another job's tick is locked by this one")
	}
	release(t, held)
	again, ok := try(t, b, "report", tick)
	if !ok {
		t.Fatal("a released tick lock cannot be taken again")
	}
	for _, unlock := range []func(context.Context) error{nextTick, otherJob, again} {
		release(t, unlock)
	}
}

func endedContext(t *testing.T, newLocks NewLocks) {
	a, b := newLocks(t)
	tick := time.Date(2026, 10, 26, 12, 1, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	unlock, acquired, err := a.TryLock(ctx, "report", tick)
	if err != nil || !acquired {
		t.Fatalf("TryLock = %v, %v", acquired, err)
	}
	cancel() // the scheduler shuts down while it holds the lock
	ended, stop := context.WithCancel(context.Background())
	stop()
	_ = unlock(ended) // may fail; the lock must still be gone
	deadline := time.Now().Add(10 * time.Second)
	for {
		again, ok := try(t, b, "report", tick)
		if ok {
			release(t, again)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the lock outlived an unlock with an ended context")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func twoSchedulers(t *testing.T, newLocks NewLocks) {
	a, b := newLocks(t)
	var mu sync.Mutex
	runs := map[time.Time]int{}
	var errs []error
	newScheduler := func(lock jobs.Lock) *jobs.Scheduler {
		s := jobs.NewScheduler(jobs.SchedulerOptions{Lock: lock, OnError: func(_ string, err error) {
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}})
		if err := s.Register("tick", jobs.ScheduleSpec{Every: time.Second}, func(context.Context) error {
			mu.Lock()
			runs[time.Now().UTC().Truncate(time.Second)]++
			mu.Unlock()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	first, second := newScheduler(a), newScheduler(b)
	first.Start()
	second.Start()
	time.Sleep(3500 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := first.StopContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := second.StopContext(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(errs) > 0 {
		t.Fatalf("scheduler errors: %v", errs)
	}
	if len(runs) < 2 {
		t.Fatalf("only %d tick(s) ran in 3.5s", len(runs))
	}
	var last time.Time
	for tick, n := range runs {
		if n != 1 {
			t.Errorf("tick %s ran %d times on two replicas", tick.Format(time.RFC3339), n)
		}
		if tick.After(last) {
			last = tick
		}
	}
	// Stopping released every lock.
	unlock, ok := try(t, a, "tick", last)
	if !ok {
		t.Fatal("a tick lock outlived the schedulers")
	}
	release(t, unlock)
}
