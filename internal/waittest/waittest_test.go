package waittest

import (
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type fakeReporter struct{ failure string }

func (*fakeReporter) Helper() {}

func (f *fakeReporter) Fatalf(format string, args ...any) { f.failure = fmt.Sprintf(format, args...) }

func TestPollReturnsAsSoonAsTheConditionHolds(t *testing.T) {
	t.Parallel()
	var ready atomic.Bool
	go func() {
		time.Sleep(20 * time.Millisecond) // real-clock: makes the condition true later, from another goroutine
		ready.Store(true)
	}()
	start := time.Now()
	if !Poll(time.Minute, ready.Load) {
		t.Fatal("the condition held but Poll reported it did not")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Poll took %s: it waited for the timeout instead of the condition", elapsed)
	}
}

func TestPollChecksBeforeSleepingAndAtTheDeadline(t *testing.T) {
	t.Parallel()
	calls := 0
	if !Poll(0, func() bool { calls++; return true }) || calls != 1 {
		t.Errorf("an already-true condition: %d evaluations, want one", calls)
	}
	calls = 0
	if Poll(0, func() bool { calls++; return false }) || calls != 2 {
		t.Errorf("a timeout of zero: %d evaluations, want the first and the last", calls)
	}
}

func TestPollTimesOutOnTheFakeClockInsideASynctestBubble(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if Poll(time.Hour, func() bool { return false }) {
			t.Error("a condition that never holds was reported as held")
		}
		if elapsed := time.Since(start); elapsed < time.Hour {
			t.Errorf("Poll gave up after %s of fake time, want the hour it was given", elapsed)
		}
	})
}

func TestUntilNamesWhatItWaitedFor(t *testing.T) {
	t.Parallel()
	passing := &fakeReporter{}
	until(passing, time.Minute, "nothing", func() bool { return true })
	if passing.failure != "" {
		t.Errorf("a holding condition failed: %q", passing.failure)
	}
	failing := &fakeReporter{}
	until(failing, time.Millisecond, "the cache to warm", func() bool { return false })
	if want := "timed out after 1ms waiting for the cache to warm"; failing.failure != want {
		t.Errorf("failure %q, want %q", failing.failure, want)
	}
	Until(t, time.Minute, "a true condition", func() bool { return true })
}
