// Package sqlite is the SQLite adapter of Forge's shared rate limiter
// (ADR 0031). SQLite serves one application instance on one host, so the
// shared state is what several processes of that host (for example a web
// server and a worker) need, and the application's clock is the only clock.
// Each hit is one atomic upsert under SQLite's write lock.
package sqlite

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/rocketinsidecorp/forge-go/ratelimit"
	"github.com/rocketinsidecorp/forge-go/sqldb"
	forgesqlite "github.com/rocketinsidecorp/forge-go/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations returns the rate limiter's migration rooted at ".". Compose it
// with the application's own through
// sqlite.CommandsFromSets(app.Migrations(), ratelimitsqlite.Migrations()).
func Migrations() fs.FS {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

// Limiter is the SQLite implementation of ratelimit.Limiter and
// ratelimit.Pruner. It is safe for concurrent use and starts no goroutines.
type Limiter struct {
	db   *sqldb.DB
	opts ratelimit.StoreOptions
	now  func() time.Time
	gate ratelimit.PruneGate
}

var (
	_ ratelimit.Limiter = (*Limiter)(nil)
	_ ratelimit.Pruner  = (*Limiter)(nil)
)

// NewLimiter returns a limiter on db, which sqlite.Open returns. The table
// comes from Migrations.
func NewLimiter(db *sqldb.DB, opts ratelimit.StoreOptions) (*Limiter, error) {
	if db == nil {
		return nil, errors.New("rate limit SQLite database is required")
	}
	opts, err := opts.Normalize()
	if err != nil {
		return nil, err
	}
	return &Limiter{db: db, opts: opts, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Allow implements ratelimit.Limiter.
func (l *Limiter) Allow(ctx context.Context, key string, limit ratelimit.Limit) (ratelimit.Decision, error) {
	if err := ctx.Err(); err != nil {
		return ratelimit.Decision{}, err
	}
	if err := ratelimit.CheckRequest(key, limit); err != nil {
		return ratelimit.Decision{}, err
	}
	now := l.now().UnixMicro()
	var hits, expiresAt int64
	// In an upsert every right-hand side reads the row as it was before the
	// statement, so both columns see the same expiry.
	err := l.db.QueryRowContext(ctx, `
		INSERT INTO forge_rate_limits (bucket, hits, expires_at) VALUES (?1, 1, ?2)
		ON CONFLICT (bucket) DO UPDATE SET
			hits = CASE WHEN expires_at <= ?3 THEN 1 ELSE hits + 1 END,
			expires_at = CASE WHEN expires_at <= ?3 THEN ?2 ELSE expires_at END
		RETURNING hits, expires_at
	`, key, now+limit.Per.Microseconds(), now).Scan(&hits, &expiresAt)
	if err != nil {
		return ratelimit.Decision{}, forgesqlite.Translate(fmt.Errorf("count rate limit hit: %w", err))
	}
	l.maybePrune(ctx, now)
	if hits > int64(limit.N) {
		return ratelimit.Decision{RetryAfter: max(time.Duration(expiresAt-now)*time.Microsecond, time.Millisecond)}, nil
	}
	return ratelimit.Decision{Allowed: true, Remaining: limit.N - int(hits)}, nil
}

// maybePrune prunes when the gate says so. It ignores errors: the hit it
// follows has been decided.
func (l *Limiter) maybePrune(ctx context.Context, nowMicro int64) {
	if l.gate.Due(l.opts) {
		_, _ = l.prune(ctx, nowMicro)
	}
}

// Prune removes expired buckets, in up to MaxPruneRounds batches of PruneBatch,
// and returns how many. One call is bounded work yet keeps up with a sustained
// stream of new keys; Allow also calls it after every PruneBatch hits.
func (l *Limiter) Prune(ctx context.Context) (int64, error) {
	return l.prune(ctx, l.now().UnixMicro())
}

func (l *Limiter) prune(ctx context.Context, nowMicro int64) (int64, error) {
	var removed int64
	for range ratelimit.MaxPruneRounds {
		result, err := l.db.ExecContext(ctx, `
			DELETE FROM forge_rate_limits
			WHERE bucket IN (
				SELECT bucket FROM forge_rate_limits
				WHERE expires_at <= ?
				ORDER BY expires_at
				LIMIT ?
			)
		`, nowMicro, l.opts.PruneBatch)
		if err != nil {
			return removed, forgesqlite.Translate(fmt.Errorf("prune rate limits: %w", err))
		}
		n, err := result.RowsAffected()
		if err != nil {
			return removed, err
		}
		removed += n
		if n < int64(l.opts.PruneBatch) {
			break
		}
	}
	return removed, nil
}
