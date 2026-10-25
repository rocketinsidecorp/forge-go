package limitertest

import (
	"context"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/ratelimit"
)

// NewSabotagedLimiter returns a limiter on fresh storage and a function that
// makes that storage unavailable. The function returns the one that makes the
// storage available again.
type NewSabotagedLimiter func(t testing.TB) (l ratelimit.Limiter, sabotage func(t testing.TB) (restore func()))

// RunUnavailable checks what every database-backed limiter must do when its
// storage fails: Allow reports the failure and never an allowing decision
// (the caller chooses whether to fail open or closed), pruning reports it, and
// the hits counted before the outage still count afterwards.
func RunUnavailable(t *testing.T, newLimiter NewSabotagedLimiter) {
	t.Helper()
	l, sabotage := newLimiter(t)
	limit := ratelimit.Limit{N: 2, Per: time.Hour}
	allow(t, l, "key", limit) // one hit of two

	restore := sabotage(t)
	d, err := l.Allow(context.Background(), "key", limit)
	if err == nil || d.Allowed {
		t.Errorf("Allow during the outage = %+v, %v; want the failure and no permission", d, err)
	}
	if pruner, ok := l.(ratelimit.Pruner); ok {
		if _, err := pruner.Prune(context.Background()); err == nil {
			t.Error("Prune succeeded during the outage")
		}
	}
	restore()

	// The failed hit was not counted: one hit remains, then the limit bites.
	if d := allow(t, l, "key", limit); !d.Allowed || d.Remaining != 0 {
		t.Errorf("second hit = %+v, want allowed with none remaining", d)
	}
	if d := allow(t, l, "key", limit); d.Allowed {
		t.Errorf("third hit = %+v, want denied: the hit before the outage must still count", d)
	}
}
