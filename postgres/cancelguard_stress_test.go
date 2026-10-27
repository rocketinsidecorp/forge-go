package postgres_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

// TestCancelledQueriesDoNotPoisonPool is a stress reproduction, skipped
// unless FORGE_TEST_PGX_CANCEL_STRESS names a duration (for example 60s):
// workers run queries whose contexts are cancelled at random moments and,
// after each, a query whose context is never cancelled, which must not fail.
// Both take an argument: pgx reports a stored error on the extended
// protocol, while the simple protocol's first read discards it.
// FORGE_TEST_PGX_CANCEL_STRESS_POOL=plain runs it on a pgxpool configured
// from the same URL without Forge's guard (the failure it reproduces);
// the default is the pool postgres.Open returns. FORGE_TEST_PGX_CANCEL_STRESS_CANCEL
// (default 0, no cancellation) is the upper bound of the random cancel delay,
// and _BURNERS the number of CPU-bound goroutines that delay the scheduler
// (run with GOMAXPROCS=1 for the most pressure); _WORKERS (default 16) and
// _PAYLOAD (argument bytes, default 0) add pressure.
func TestCancelledQueriesDoNotPoisonPool(t *testing.T) {
	duration, err := time.ParseDuration(os.Getenv("FORGE_TEST_PGX_CANCEL_STRESS"))
	if err != nil {
		t.Skip("set FORGE_TEST_PGX_CANCEL_STRESS to a duration to run the stress reproduction")
	}
	maxCancel, _ := time.ParseDuration(os.Getenv("FORGE_TEST_PGX_CANCEL_STRESS_CANCEL"))
	burners, _ := strconv.Atoi(os.Getenv("FORGE_TEST_PGX_CANCEL_STRESS_BURNERS"))
	workers, _ := strconv.Atoi(os.Getenv("FORGE_TEST_PGX_CANCEL_STRESS_WORKERS"))
	// A larger argument makes the write slower, so pgx's slow-write timer
	// fires more often.
	payloadSize, _ := strconv.Atoi(os.Getenv("FORGE_TEST_PGX_CANCEL_STRESS_PAYLOAD"))
	payload := strings.Repeat("x", payloadSize)
	if workers == 0 {
		workers = 16
	}

	_, cfg := postgrestest.NewWithConfig(t)
	cfg.MaxConns, cfg.MinConns = 4, 0
	ctx := context.Background()
	var pool *pgxpool.Pool
	if os.Getenv("FORGE_TEST_PGX_CANCEL_STRESS_POOL") == "plain" {
		plain, err := pgxpool.ParseConfig(cfg.URL.Reveal())
		if err != nil {
			t.Fatal(err)
		}
		plain.MaxConns, plain.MinConns = cfg.MaxConns, 0
		if pool, err = pgxpool.NewWithConfig(ctx, plain); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
	} else {
		db, err := postgres.Open(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(db.Close)
		pool = db.Pool()
	}

	stop := make(chan struct{})
	var burning sync.WaitGroup
	for range burners {
		burning.Go(func() {
			for n := 0; ; n++ {
				if n%(1<<20) == 0 {
					select {
					case <-stop:
						return
					default:
					}
				}
			}
		})
	}

	var queries, fired, poisoned, poisonedUncancelled, otherErrors atomic.Int64
	var firstPoison atomic.Value
	deadline := time.Now().Add(duration)
	var running sync.WaitGroup
	for range workers {
		running.Go(func() {
			for time.Now().Before(deadline) {
				queryCtx, cancel := context.WithCancel(ctx)
				var timer *time.Timer
				if maxCancel > 0 {
					timer = time.AfterFunc(rand.N(maxCancel), cancel)
				}
				var size int
				err := pool.QueryRow(queryCtx, "SELECT length($1::text)", payload).Scan(&size)
				cancelled := timer != nil && !timer.Stop()
				cancel()
				queries.Add(1)
				if cancelled {
					fired.Add(1)
				}
				if err != nil && !cancelled {
					// The context was never cancelled: a failure is a
					// previous query's poison.
					if isTimeout(err) {
						poisonedUncancelled.Add(1)
						firstPoison.CompareAndSwap(nil, err.Error())
					} else {
						otherErrors.Add(1)
					}
				}
				if err := pool.QueryRow(ctx, "SELECT $1::int", 1).Scan(&size); err != nil {
					if isTimeout(err) {
						poisoned.Add(1)
						firstPoison.CompareAndSwap(nil, err.Error())
					} else {
						otherErrors.Add(1)
					}
				}
			}
		})
	}
	running.Wait()
	close(stop)
	burning.Wait()

	stat := pool.Stat()
	t.Logf("%s: %d cancellable queries (%d cancellations fired), %d poisoned background queries, %d poisoned uncancelled queries, %d other errors, %d connections created",
		duration, queries.Load(), fired.Load(), poisoned.Load(), poisonedUncancelled.Load(), otherErrors.Load(), stat.NewConnsCount())
	if poisoned.Load()+poisonedUncancelled.Load() > 0 {
		t.Errorf("%d queries failed on a connection a cancelled query poisoned, first: %v", poisoned.Load()+poisonedUncancelled.Load(), firstPoison.Load())
	}
	if otherErrors.Load() > 0 {
		t.Errorf("%d queries that were not cancelled failed otherwise", otherErrors.Load())
	}
}

// isTimeout reports whether err is a network timeout, the error a poisoned
// connection returns.
func isTimeout(err error) bool {
	return pgconn.Timeout(err) || errors.Is(err, os.ErrDeadlineExceeded)
}
