// Package postgres is the PostgreSQL adapter of Forge's shared rate limiter
// (ADR 0026): one atomic INSERT ... ON CONFLICT statement per hit, with all
// window arithmetic on the database clock, so replicas with skewed clocks
// agree and a hit can neither be lost nor counted twice.
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sync/atomic"
	"time"

	forgepostgres "github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/ratelimit"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations returns the rate limiter's migration rooted at ".". Compose it
// with the application's own through
// postgres.CommandsFromSets(app.Migrations(), ratelimitpostgres.Migrations()).
func Migrations() fs.FS {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

// Limiter is the PostgreSQL implementation of ratelimit.Limiter and
// ratelimit.Pruner. It is safe for concurrent use and starts no goroutines.
type Limiter struct {
	db        *forgepostgres.DB
	opts      ratelimit.StoreOptions
	lastPrune atomic.Int64 // unix nanoseconds of the last opportunistic prune
}

var (
	_ ratelimit.Limiter = (*Limiter)(nil)
	_ ratelimit.Pruner  = (*Limiter)(nil)
)

// NewLimiter returns a limiter on db. The table comes from Migrations.
func NewLimiter(db *forgepostgres.DB, opts ratelimit.StoreOptions) (*Limiter, error) {
	if db == nil {
		return nil, errors.New("rate limit PostgreSQL database is required")
	}
	opts, err := opts.Normalize()
	if err != nil {
		return nil, err
	}
	return &Limiter{db: db, opts: opts}, nil
}

// Allow implements ratelimit.Limiter.
func (l *Limiter) Allow(ctx context.Context, key string, limit ratelimit.Limit) (ratelimit.Decision, error) {
	if err := ctx.Err(); err != nil {
		return ratelimit.Decision{}, err
	}
	if err := ratelimit.CheckRequest(key, limit); err != nil {
		return ratelimit.Decision{}, err
	}
	// now() is the statement's start time (autocommit), so every comparison in
	// the statement sees the same instant. An expired bucket restarts at 1.
	var hits int64
	var retryMicros int64
	err := l.db.QueryRow(ctx, `
		INSERT INTO forge_rate_limits AS b (bucket, hits, expires_at)
		VALUES ($1, 1, now() + ($2 * interval '1 microsecond'))
		ON CONFLICT (bucket) DO UPDATE SET
			hits = CASE WHEN b.expires_at <= now() THEN 1 ELSE b.hits + 1 END,
			expires_at = CASE WHEN b.expires_at <= now() THEN EXCLUDED.expires_at ELSE b.expires_at END
		RETURNING b.hits, (extract(epoch FROM (b.expires_at - now())) * 1000000)::bigint
	`, key, limit.Per.Microseconds()).Scan(&hits, &retryMicros)
	if err != nil {
		return ratelimit.Decision{}, forgepostgres.Translate(fmt.Errorf("count rate limit hit: %w", err))
	}
	l.maybePrune(ctx)
	if hits > int64(limit.N) {
		return ratelimit.Decision{RetryAfter: max(time.Duration(retryMicros)*time.Microsecond, time.Millisecond)}, nil
	}
	return ratelimit.Decision{Allowed: true, Remaining: limit.N - int(hits)}, nil
}

// maybePrune removes a bounded batch of expired buckets at most once per
// PruneInterval, so tables stay small without a scheduler. It ignores errors:
// the hit it follows has already been decided.
func (l *Limiter) maybePrune(ctx context.Context) {
	if l.opts.PruneInterval < 0 {
		return
	}
	now := time.Now().UnixNano()
	last := l.lastPrune.Load()
	if now-last < int64(l.opts.PruneInterval) || !l.lastPrune.CompareAndSwap(last, now) {
		return
	}
	_, _ = l.Prune(ctx)
}

// Prune deletes at most PruneBatch expired buckets and returns how many.
// Replicas may call it concurrently: SKIP LOCKED keeps them from blocking each
// other.
func (l *Limiter) Prune(ctx context.Context) (int64, error) {
	tag, err := l.db.Exec(ctx, `
		DELETE FROM forge_rate_limits
		WHERE bucket IN (
			SELECT bucket FROM forge_rate_limits
			WHERE expires_at <= clock_timestamp()
			ORDER BY expires_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
	`, l.opts.PruneBatch)
	if err != nil {
		return 0, forgepostgres.Translate(fmt.Errorf("prune rate limits: %w", err))
	}
	return tag.RowsAffected(), nil
}
