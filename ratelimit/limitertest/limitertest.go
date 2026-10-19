// Package limitertest is the conformance suite every ratelimit.Limiter must
// pass. Run it against a real database for the shared limiters:
//
//	func TestLimiter(t *testing.T) {
//	    limitertest.Run(t, func(t testing.TB) ratelimit.Limiter { ... a limiter on a fresh, migrated database ... })
//	}
package limitertest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/ratelimit"
)

// NewLimiter returns a limiter on fresh storage that the test cleans up.
type NewLimiter func(t testing.TB) ratelimit.Limiter

// window is short so expiry can be observed with real clocks, including the
// database clocks of the shared limiters.
const window = 400 * time.Millisecond

// Run executes the suite.
func Run(t *testing.T, newLimiter NewLimiter) {
	t.Helper()
	tests := []struct {
		name string
		run  func(*testing.T, ratelimit.Limiter)
	}{
		{"AllowsUpToTheLimitThenDenies", allowsUpToTheLimit},
		{"KeysAreIndependent", keysAreIndependent},
		{"WindowResets", windowResets},
		{"DeniedHitsReportRetryAfter", deniedReportsRetryAfter},
		{"ConcurrentHitsNeverExceedTheLimit", concurrentHits},
		{"RejectsInvalidInput", rejectsInvalidInput},
		{"HonorsCancelledContext", honorsCancelledContext},
		{"PrunesExpiredBucketsOnly", prunesExpired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.run(t, newLimiter(t))
		})
	}
}

func allow(t *testing.T, l ratelimit.Limiter, key string, limit ratelimit.Limit) ratelimit.Decision {
	t.Helper()
	d, err := l.Allow(context.Background(), key, limit)
	if err != nil {
		t.Fatalf("Allow(%q): %v", key, err)
	}
	return d
}

func allowsUpToTheLimit(t *testing.T, l ratelimit.Limiter) {
	limit := ratelimit.Limit{N: 3, Per: time.Minute}
	for i := range limit.N {
		d := allow(t, l, "k", limit)
		if !d.Allowed || d.Remaining != limit.N-i-1 || d.RetryAfter != 0 {
			t.Fatalf("hit %d = %+v, want allowed with %d remaining and no retry-after", i+1, d, limit.N-i-1)
		}
	}
	for range 3 {
		if d := allow(t, l, "k", limit); d.Allowed || d.Remaining != 0 {
			t.Fatalf("hit over the limit = %+v, want denied", d)
		}
	}
}

func keysAreIndependent(t *testing.T, l ratelimit.Limiter) {
	limit := ratelimit.Limit{N: 1, Per: time.Minute}
	if !allow(t, l, "a", limit).Allowed || allow(t, l, "a", limit).Allowed {
		t.Fatal("key a: want first hit allowed and second denied")
	}
	if !allow(t, l, "b", limit).Allowed {
		t.Fatal("key b must not be affected by key a")
	}
}

func windowResets(t *testing.T, l ratelimit.Limiter) {
	limit := ratelimit.Limit{N: 2, Per: window}
	allow(t, l, "k", limit)
	allow(t, l, "k", limit)
	if allow(t, l, "k", limit).Allowed {
		t.Fatal("third hit in the window must be denied")
	}
	time.Sleep(window + 150*time.Millisecond)
	d := allow(t, l, "k", limit)
	if !d.Allowed || d.Remaining != 1 {
		t.Fatalf("first hit of a new window = %+v, want allowed with 1 remaining", d)
	}
}

func deniedReportsRetryAfter(t *testing.T, l ratelimit.Limiter) {
	limit := ratelimit.Limit{N: 1, Per: 30 * time.Second}
	allow(t, l, "k", limit)
	d := allow(t, l, "k", limit)
	if d.Allowed {
		t.Fatal("second hit must be denied")
	}
	if d.RetryAfter <= 0 || d.RetryAfter > limit.Per {
		t.Fatalf("RetryAfter = %s, want within (0, %s]", d.RetryAfter, limit.Per)
	}
}

func concurrentHits(t *testing.T, l ratelimit.Limiter) {
	const goroutines, hitsEach, limitN = 16, 10, 25
	limit := ratelimit.Limit{N: limitN, Per: time.Minute}
	var allowed atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range hitsEach {
				d, err := l.Allow(context.Background(), "shared", limit)
				if err != nil {
					errs <- err
					return
				}
				if d.Allowed {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := allowed.Load(); got != limitN {
		t.Fatalf("%d concurrent hits allowed %d, want exactly %d", goroutines*hitsEach, got, limitN)
	}
}

func rejectsInvalidInput(t *testing.T, l ratelimit.Limiter) {
	ok := ratelimit.Limit{N: 1, Per: time.Minute}
	bad := map[string]struct {
		key   string
		limit ratelimit.Limit
	}{
		"empty key":      {"", ok},
		"space in key":   {"a b", ok},
		"non-ASCII key":  {"café", ok},
		"long key":       {fmt.Sprintf("%0129d", 1), ok},
		"zero N":         {"k", ratelimit.Limit{N: 0, Per: time.Minute}},
		"zero window":    {"k", ratelimit.Limit{N: 1}},
		"negative":       {"k", ratelimit.Limit{N: 1, Per: -time.Second}},
		"huge window":    {"k", ratelimit.Limit{N: 1, Per: ratelimit.MaxPer + time.Hour}},
		"control in key": {"a\x00b", ok},
	}
	for name, c := range bad {
		if _, err := l.Allow(context.Background(), c.key, c.limit); err == nil {
			t.Errorf("%s: Allow succeeded, want an error", name)
		}
	}
	// 128 characters is the longest valid key.
	if _, err := l.Allow(context.Background(), fmt.Sprintf("%0128d", 1), ok); err != nil {
		t.Errorf("128-character key: %v", err)
	}
}

func honorsCancelledContext(t *testing.T, l ratelimit.Limiter) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Allow(ctx, "k", ratelimit.Limit{N: 1, Per: time.Minute}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Allow with a cancelled context = %v, want context.Canceled", err)
	}
}

func prunesExpired(t *testing.T, l ratelimit.Limiter) {
	pruner, ok := l.(ratelimit.Pruner)
	if !ok {
		t.Skip("limiter keeps no persistent buckets")
	}
	short := ratelimit.Limit{N: 5, Per: window}
	long := ratelimit.Limit{N: 5, Per: time.Hour}
	for i := range 3 {
		allow(t, l, fmt.Sprintf("short-%d", i), short)
	}
	allow(t, l, "long", long)
	time.Sleep(window + 150*time.Millisecond)
	removed, err := pruner.Prune(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Fatalf("Prune removed %d buckets, want the 3 expired ones", removed)
	}
	if d := allow(t, l, "long", long); d.Remaining != 3 {
		t.Fatalf("live bucket after prune: %+v, want its count kept (3 remaining)", d)
	}
	if removed, err := pruner.Prune(context.Background()); err != nil || removed != 0 {
		t.Fatalf("second Prune = %d, %v; want nothing left to remove", removed, err)
	}
}
