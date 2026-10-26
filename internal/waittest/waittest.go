// Package waittest waits for a condition that another goroutine, process or
// server makes true: the one place the tests poll the real clock.
//
// A fixed pause after which a test assumes "it has happened by now" is a
// race that CI loses on a slow runner. Waiting for the condition itself is
// not: the test continues the moment the condition holds and fails, naming
// what it waited for, only after a generous timeout. Inside a
// testing/synctest bubble the same calls run on the fake clock.
package waittest

import (
	"testing"
	"time"
)

// PollInterval is how often Poll evaluates its condition.
const PollInterval = 5 * time.Millisecond

// Poll evaluates condition immediately and then every PollInterval until it
// returns true or timeout has passed, and reports whether it held. The
// condition runs once more after the timeout, so a state reached at the last
// moment is not missed.
func Poll(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return true
		}
		if !time.Now().Before(deadline) {
			return condition()
		}
		time.Sleep(PollInterval) // real-clock: the poll interval; the wait ends when the condition holds, not when this elapses
	}
}

// reporter is the part of testing.TB that Until needs, so its failure path
// can be tested.
type reporter interface {
	Helper()
	Fatalf(format string, args ...any)
}

// Until fails the test, naming what it waited for, unless condition holds
// within timeout.
func Until(t testing.TB, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	until(t, timeout, what, condition)
}

func until(t reporter, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	if !Poll(timeout, condition) {
		t.Fatalf("timed out after %s waiting for %s", timeout, what)
	}
}
