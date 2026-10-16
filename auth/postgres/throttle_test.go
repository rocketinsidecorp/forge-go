package postgres_test

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
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
