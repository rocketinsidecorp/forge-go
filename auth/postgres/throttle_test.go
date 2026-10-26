package postgres_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/auth"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

func TestLoginThrottlerSharesStateAcrossInstances(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	config := auth.LoginThrottleConfig{
		Window:       time.Minute,
		AccountLimit: 1,
		SourceLimit:  100,
		MaxEntries:   100,
	}
	first, err := authpostgres.NewLoginThrottler(db, config)
	if err != nil {
		t.Fatal(err)
	}
	second, err := authpostgres.NewLoginThrottler(db, config)
	if err != nil {
		t.Fatal(err)
	}
	key := auth.LoginThrottleKey{Account: sha256.Sum256([]byte("account"))}
	decision, err := first.Attempt(context.Background(), key)
	if err != nil || !decision.Allowed {
		t.Fatalf("first attempt=%+v err=%v", decision, err)
	}
	decision, err = second.Attempt(context.Background(), key)
	if err != nil || decision.Allowed || decision.RetryAfter <= 0 {
		t.Fatalf("second attempt=%+v err=%v", decision, err)
	}
	if err := second.Success(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	decision, err = first.Attempt(context.Background(), key)
	if err != nil || !decision.Allowed {
		t.Fatalf("after success=%+v err=%v", decision, err)
	}
}

func TestLoginThrottlerPrunesExpiredBuckets(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	limiter, err := authpostgres.NewLoginThrottler(db, auth.DefaultLoginThrottleConfig())
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("expired"))
	if _, err := db.Exec(context.Background(), `
		INSERT INTO forge_login_throttle (scope, key_hash, attempt_count, reset_at)
		VALUES (1, $1, 1, now() - interval '1 minute')
	`, digest[:]); err != nil {
		t.Fatal(err)
	}
	pruned, err := limiter.PruneExpired(context.Background(), 10)
	if err != nil || pruned != 1 {
		t.Fatalf("pruned=%d err=%v", pruned, err)
	}
	var count int
	if err := db.QueryRow(context.Background(), "SELECT count(*) FROM forge_login_throttle").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rows=%d", count)
	}
}

func throttleKey(account, source string, withPair bool) auth.LoginThrottleKey {
	key := auth.LoginThrottleKey{Account: sha256.Sum256([]byte("account:" + account))}
	if source != "" {
		key.Source, key.HasSource = sha256.Sum256([]byte("source:"+source)), true
		if withPair {
			key.Pair, key.HasPair = sha256.Sum256([]byte("pair:"+account+"|"+source)), true
		}
	}
	return key
}

func counterOf(t *testing.T, db *postgres.DB, scope int16, digest [32]byte) (count int, found bool) {
	t.Helper()
	err := db.QueryRow(context.Background(), "SELECT attempt_count FROM forge_login_throttle WHERE scope = $1 AND key_hash = $2", scope, digest[:]).Scan(&count)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return count, true
}

func TestLoginThrottlerKeepsOneSourceFromLockingALoginOut(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	limiter, err := authpostgres.NewLoginThrottler(db, auth.LoginThrottleConfig{
		Window: time.Minute, PairLimit: 2, AccountLimit: 4, SourceLimit: 100, MaxEntries: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	attacker := throttleKey("owner@example.com", "192.0.2.66", true)
	for i := 0; i < 2; i++ {
		if decision, err := limiter.Attempt(ctx, attacker); err != nil || !decision.Allowed {
			t.Fatalf("attempt %d=%+v err=%v", i+1, decision, err)
		}
	}
	for i := 0; i < 50; i++ {
		decision, err := limiter.Attempt(ctx, attacker)
		if err != nil || decision.Allowed || decision.RetryAfter <= 0 {
			t.Fatalf("blocked attempt %d=%+v err=%v", i+1, decision, err)
		}
	}
	// Refused attempts are not counted: the login's shared counter holds the
	// two allowed attempts, not fifty-two, so the owner is not locked out.
	if count, _ := counterOf(t, db, auth.AccountThrottleScope, attacker.Account); count != 2 {
		t.Fatalf("account counter=%d, want 2: refused attempts burned the shared budget", count)
	}
	if count, _ := counterOf(t, db, auth.SourceThrottleScope, attacker.Source); count != 2 {
		t.Fatalf("source counter=%d, want 2", count)
	}
	owner := throttleKey("owner@example.com", "192.0.2.1", true)
	if decision, err := limiter.Attempt(ctx, owner); err != nil || !decision.Allowed {
		t.Fatalf("the owner from another source: %+v err=%v", decision, err)
	}

	// Success clears the pair and the account counters, not the source's.
	if err := limiter.Success(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if _, found := counterOf(t, db, auth.AccountThrottleScope, owner.Account); found {
		t.Fatal("success kept the account counter")
	}
	if _, found := counterOf(t, db, auth.PairThrottleScope, owner.Pair); found {
		t.Fatal("success kept the pair counter")
	}
	if _, found := counterOf(t, db, auth.SourceThrottleScope, owner.Source); !found {
		t.Fatal("success cleared the source counter")
	}
}

func TestLoginThrottlerCapsOneLoginAcrossSources(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	limiter, err := authpostgres.NewLoginThrottler(db, auth.LoginThrottleConfig{
		Window: time.Minute, PairLimit: 2, AccountLimit: 3, SourceLimit: 100, MaxEntries: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		key := throttleKey("ada@example.com", fmt.Sprintf("192.0.2.%d", i+1), true)
		if decision, err := limiter.Attempt(context.Background(), key); err != nil || !decision.Allowed {
			t.Fatalf("source %d=%+v err=%v", i+1, decision, err)
		}
	}
	if decision, err := limiter.Attempt(context.Background(), throttleKey("ada@example.com", "192.0.2.9", true)); err != nil || decision.Allowed {
		t.Fatalf("a fourth source guessed past the account cap: %+v err=%v", decision, err)
	}
}

func TestLoginThrottlerNeverExceedsALimitUnderConcurrency(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	limiter, err := authpostgres.NewLoginThrottler(db, auth.LoginThrottleConfig{
		Window: time.Minute, PairLimit: 5, AccountLimit: 50, SourceLimit: 300, MaxEntries: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := throttleKey("race@example.com", "192.0.2.7", true)
	var wg sync.WaitGroup
	var granted atomic.Int64
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision, err := limiter.Attempt(context.Background(), key)
			if err != nil {
				t.Error(err)
				return
			}
			if decision.Allowed {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	if granted.Load() != 5 {
		t.Fatalf("%d of 24 concurrent attempts were allowed, want exactly the pair limit (5)", granted.Load())
	}
}

func TestLoginThrottlerPrunesExpiredCountersWhileCounting(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	ctx := context.Background()
	// 400 counters from windows that ended: an attacker who cycled through
	// many logins and sources, or an application that never ran PruneExpired.
	if _, err := db.Exec(ctx, `
		INSERT INTO forge_login_throttle (scope, key_hash, attempt_count, reset_at)
		SELECT 1, sha256(('old' || n)::bytea), 1, now() - interval '1 minute'
		FROM generate_series(1, 400) AS n
	`); err != nil {
		t.Fatal(err)
	}
	limiter, err := authpostgres.NewLoginThrottler(db, auth.DefaultLoginThrottleConfig())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		if decision, err := limiter.Attempt(ctx, throttleKey(fmt.Sprintf("user%d@example.com", i), fmt.Sprintf("198.51.100.%d", i), true)); err != nil || !decision.Allowed {
			t.Fatalf("attempt %d=%+v err=%v", i, decision, err)
		}
	}
	var expired int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM forge_login_throttle WHERE reset_at <= now()").Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if expired != 0 {
		t.Fatalf("%d expired counters remain after 60 attempts: pruning does not keep up with counting", expired)
	}
}

func TestLoginThrottlerTableStaysBoundedUnderChurn(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	limiter, err := authpostgres.NewLoginThrottler(db, auth.LoginThrottleConfig{
		Window: 5 * time.Millisecond, PairLimit: 5, AccountLimit: 50, SourceLimit: 300, MaxEntries: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 300; i++ {
		if _, err := limiter.Attempt(ctx, throttleKey(fmt.Sprintf("churn%d@example.com", i), fmt.Sprintf("10.0.%d.%d", i/250, i%250+1), true)); err != nil {
			t.Fatal(err)
		}
		if i%50 == 0 {
			time.Sleep(10 * time.Millisecond) // real-clock: paces the hits so the limiter's inline prune sees rows that expire on the database clock
		}
	}
	var rows int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM forge_login_throttle").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	// 300 attempts create 900 counters; with inline pruning only the last
	// window's remain, far fewer than the number ever created.
	if rows > 150 {
		t.Fatalf("%d counters remain for 300 attempts with a 5 ms window", rows)
	}
}
