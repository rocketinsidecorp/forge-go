package auth

import (
	"context"
	"testing"
	"time"
)

// The clock can step backwards (an NTP correction, a restored VM). Counters
// are kept in creation order, which then is no longer expiry order: a counter
// behind a live one can already be over, and must be dropped when it is
// looked at instead of blocking someone for a window that has ended.
func TestACounterWhoseWindowEndedBehindALiveOneIsDropped(t *testing.T) {
	t.Parallel()
	config := testThrottleConfig()
	limiter, clock := newTestThrottler(t, config)
	first := deriveLoginThrottleKey("first@example.com", "192.0.2.1")
	second := deriveLoginThrottleKey("second@example.com", "192.0.2.2")

	allowed(t, limiter, first) // window until t0+60s
	clock.Advance(-30 * time.Second)
	for i := 0; i < config.PairLimit; i++ {
		allowed(t, limiter, second) // window until t0+30s: created after first, expires before it
	}
	if allowed(t, limiter, second) {
		t.Fatal("the pair limit did not block")
	}
	blockedBefore := limiter.blocking
	clock.Advance(65 * time.Second) // t0+35s: second's window is over, first's is not
	if !allowed(t, limiter, second) {
		t.Fatal("a counter whose window ended behind a live one still blocks")
	}
	if limiter.blocking >= blockedBefore {
		t.Errorf("blocking = %d, want fewer than the %d blocking counters before", limiter.blocking, blockedBefore)
	}
	if err := limiter.Success(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	// Removing a counter that is not there changes nothing.
	limiter.remove(nil)
}

// A full table whose counters all block, or belong to the attempt itself,
// has nothing to evict: the attempt is refused, and the blocking counters of
// the accounts under attack survive.
func TestAFullTableWithNothingToEvictRefusesTheAttempt(t *testing.T) {
	t.Parallel()
	config := LoginThrottleConfig{Window: time.Minute, AccountLimit: 1, SourceLimit: 4, MaxEntries: 3}
	limiter, _ := newTestThrottler(t, config)
	a := deriveLoginThrottleKey("a@example.com", "192.0.2.1")
	b := deriveLoginThrottleKey("b@example.com", "192.0.2.2")
	c := deriveLoginThrottleKey("c@example.com", "192.0.2.3")
	if !allowed(t, limiter, a) || !allowed(t, limiter, b) {
		t.Fatal("the first two attempts were refused")
	}
	// a's and b's accounts now block (limit 1); only b's source is idle.
	decision, err := limiter.Attempt(context.Background(), c)
	if err != nil || decision.Allowed || decision.RetryAfter != config.Window {
		t.Fatalf("a third account in a full table = %+v, %v; want a refusal for one window", decision, err)
	}
	if got := len(limiter.buckets); got != config.MaxEntries {
		t.Errorf("the table holds %d counters, want its bound of %d untouched", got, config.MaxEntries)
	}
	for _, key := range []LoginThrottleKey{a, b} {
		if _, blocked := limiter.buckets[throttleBucketKey{scope: AccountThrottleScope, digest: key.Account}]; !blocked {
			t.Error("a blocking account counter was evicted to make room")
		}
	}
}
