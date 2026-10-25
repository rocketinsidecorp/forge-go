package mysql_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/mysql/mysqltest"
	"github.com/rocketinsidecorp/forge-go/ratelimit"
	"github.com/rocketinsidecorp/forge-go/ratelimit/limitertest"
	ratelimitmysql "github.com/rocketinsidecorp/forge-go/ratelimit/mysql"
)

func newLimiter(t testing.TB, opts ratelimit.StoreOptions) *ratelimitmysql.Limiter {
	t.Helper()
	limiter, err := ratelimitmysql.NewLimiter(mysqltest.NewMigrated(t, ratelimitmysql.Migrations()), opts)
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
	for i := range 60 {
		if _, err := l.Allow(ctx, fmt.Sprintf("k%02d", i), ratelimit.Limit{N: 1, Per: 20 * time.Millisecond}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(150 * time.Millisecond)
	// One call is bounded work: MaxPruneRounds batches of 2.
	for i, want := range []int64{ratelimit.MaxPruneRounds * 2, 20, 0} {
		if removed, err := l.Prune(ctx); err != nil || removed != want {
			t.Fatalf("Prune #%d = %d, %v; want %d", i+1, removed, err, want)
		}
	}
}

func TestSustainedNewKeysDoNotPileUpExpiredBuckets(t *testing.T) {
	// The default interval is a minute: only the hit counter can keep up.
	db := mysqltest.NewMigrated(t, ratelimitmysql.Migrations())
	l, err := ratelimitmysql.NewLimiter(db, ratelimit.StoreOptions{PruneBatch: 50})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := range 1500 {
		if _, err := l.Allow(ctx, fmt.Sprintf("k%d", i), ratelimit.Limit{N: 1, Per: time.Millisecond}); err != nil {
			t.Fatal(err)
		}
		if i%100 == 99 {
			time.Sleep(3 * time.Millisecond)
		}
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM forge_rate_limits`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows > 400 {
		t.Fatalf("%d buckets after 1500 hits of new, instantly expiring keys, want a small bounded number", rows)
	}
}

func TestNewLimiterValidation(t *testing.T) {
	if _, err := ratelimitmysql.NewLimiter(nil, ratelimit.StoreOptions{}); err == nil {
		t.Fatal("nil database accepted")
	}
}

func TestLimiterFailsWhenItsTableIsUnavailable(t *testing.T) {
	limitertest.RunUnavailable(t, func(tb testing.TB) (ratelimit.Limiter, func(testing.TB) func()) {
		db := mysqltest.NewMigrated(tb, ratelimitmysql.Migrations())
		limiter, err := ratelimitmysql.NewLimiter(db, ratelimit.StoreOptions{PruneInterval: -1})
		if err != nil {
			tb.Fatal(err)
		}
		rename := func(tb testing.TB, from, to string) {
			tb.Helper()
			if _, err := db.ExecContext(context.Background(), "RENAME TABLE "+from+" TO "+to); err != nil {
				tb.Fatal(err)
			}
		}
		return limiter, func(tb testing.TB) func() {
			rename(tb, "forge_rate_limits", "forge_rate_limits_gone")
			return func() { rename(tb, "forge_rate_limits_gone", "forge_rate_limits") }
		}
	})
}
