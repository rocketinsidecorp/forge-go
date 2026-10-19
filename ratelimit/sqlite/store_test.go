package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/ratelimit"
	"github.com/rocketinsidecorp/forge-go/ratelimit/limitertest"
	ratelimitsqlite "github.com/rocketinsidecorp/forge-go/ratelimit/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
)

func newLimiter(t testing.TB, opts ratelimit.StoreOptions) *ratelimitsqlite.Limiter {
	t.Helper()
	limiter, err := ratelimitsqlite.NewLimiter(sqlitetest.NewMigrated(t, ratelimitsqlite.Migrations()), opts)
	if err != nil {
		t.Fatal(err)
	}
	return limiter
}

func TestLimiterConformance(t *testing.T) {
	limitertest.Run(t, func(t testing.TB) ratelimit.Limiter {
		return newLimiter(t, ratelimit.StoreOptions{PruneInterval: -1})
	})
}

func TestAllowPrunesExpiredBucketsOpportunistically(t *testing.T) {
	l := newLimiter(t, ratelimit.StoreOptions{PruneInterval: time.Millisecond, PruneBatch: 1000})
	ctx := context.Background()
	for _, key := range []string{"a", "b", "c"} {
		if _, err := l.Allow(ctx, key, ratelimit.Limit{N: 1, Per: 50 * time.Millisecond}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := l.Allow(ctx, "d", ratelimit.Limit{N: 1, Per: time.Minute}); err != nil {
		t.Fatal(err)
	}
	// The prune ran inline after the hit above: nothing expired is left.
	if removed, err := l.Prune(ctx); err != nil || removed != 0 {
		t.Fatalf("Prune after an opportunistic prune = %d, %v; want 0", removed, err)
	}
}

func TestPruneIsBounded(t *testing.T) {
	l := newLimiter(t, ratelimit.StoreOptions{PruneInterval: -1, PruneBatch: 2})
	ctx := context.Background()
	for _, key := range []string{"a", "b", "c", "d", "e"} {
		if _, err := l.Allow(ctx, key, ratelimit.Limit{N: 1, Per: 20 * time.Millisecond}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(150 * time.Millisecond)
	for i, want := range []int64{2, 2, 1, 0} {
		if removed, err := l.Prune(ctx); err != nil || removed != want {
			t.Fatalf("Prune #%d = %d, %v; want %d", i+1, removed, err, want)
		}
	}
}

func TestNewLimiterValidation(t *testing.T) {
	if _, err := ratelimitsqlite.NewLimiter(nil, ratelimit.StoreOptions{}); err == nil {
		t.Fatal("nil database accepted")
	}
}
