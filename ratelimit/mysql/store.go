// Package mysql is the MySQL and MariaDB adapter of Forge's shared rate
// limiter (ADR 0026). A hit is one transaction: an INSERT ... ON DUPLICATE
// KEY UPDATE that leaves the bucket's row locked, then a read of that row,
// so the count a caller sees is its own even under concurrency. All window
// arithmetic uses the database clock (UTC_TIMESTAMP), so replicas with
// skewed clocks agree.
package mysql

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	forgemysql "github.com/rocketinsidecorp/forge-go/mysql"
	"github.com/rocketinsidecorp/forge-go/ratelimit"
	"github.com/rocketinsidecorp/forge-go/sqldb"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations returns the rate limiter's migration rooted at ".". Compose it
// with the application's own through
// mysql.CommandsFromSets(app.Migrations(), ratelimitmysql.Migrations()).
func Migrations() fs.FS {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

// hitAttempts reruns a transaction that lost a deadlock.
const hitAttempts = 3

// Limiter is the MySQL/MariaDB implementation of ratelimit.Limiter and
// ratelimit.Pruner. It is safe for concurrent use and starts no goroutines.
type Limiter struct {
	db   *sqldb.DB
	opts ratelimit.StoreOptions
	gate ratelimit.PruneGate
}

var (
	_ ratelimit.Limiter = (*Limiter)(nil)
	_ ratelimit.Pruner  = (*Limiter)(nil)
)

// NewLimiter returns a limiter on db, which mysql.Open returns. The table
// comes from Migrations.
func NewLimiter(db *sqldb.DB, opts ratelimit.StoreOptions) (*Limiter, error) {
	if db == nil {
		return nil, errors.New("rate limit MySQL database is required")
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
	var hits, retryMicros int64
	err := l.db.InTxWith(ctx, sqldb.TxOptions{Attempts: hitAttempts}, func(tx *sql.Tx) error {
		// Assignments run left to right and both read the expiry as stored
		// before this statement changes it: hits is assigned first, expires_at
		// second, and neither assignment reads the other's column.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO forge_rate_limits (bucket, hits, expires_at)
			VALUES (?, 1, DATE_ADD(UTC_TIMESTAMP(6), INTERVAL ? MICROSECOND))
			ON DUPLICATE KEY UPDATE
				hits = IF(expires_at <= UTC_TIMESTAMP(6), 1, hits + 1),
				expires_at = IF(expires_at <= UTC_TIMESTAMP(6), DATE_ADD(UTC_TIMESTAMP(6), INTERVAL ? MICROSECOND), expires_at)
		`, key, limit.Per.Microseconds(), limit.Per.Microseconds()); err != nil {
			return err
		}
		// The upsert holds the row's lock until commit: this read is our own
		// write, not a later hit's.
		return tx.QueryRowContext(ctx, `
			SELECT hits, TIMESTAMPDIFF(MICROSECOND, UTC_TIMESTAMP(6), expires_at)
			FROM forge_rate_limits WHERE bucket = ?
		`, key).Scan(&hits, &retryMicros)
	})
	if err != nil {
		return ratelimit.Decision{}, forgemysql.Translate(fmt.Errorf("count rate limit hit: %w", err))
	}
	l.maybePrune(ctx)
	if hits > int64(limit.N) {
		return ratelimit.Decision{RetryAfter: max(time.Duration(retryMicros)*time.Microsecond, time.Millisecond)}, nil
	}
	return ratelimit.Decision{Allowed: true, Remaining: limit.N - int(hits)}, nil
}

// maybePrune prunes when the gate says so: once per PruneInterval, or sooner
// after PruneBatch hits. It ignores errors: the hit it follows has been decided.
func (l *Limiter) maybePrune(ctx context.Context) {
	if l.gate.Due(l.opts) {
		_, _ = l.Prune(ctx)
	}
}

// Prune removes expired buckets, in up to MaxPruneRounds batches of PruneBatch,
// and returns how many. One call is bounded work yet keeps up with a sustained
// stream of new keys; Allow also calls it after every PruneBatch hits.
func (l *Limiter) Prune(ctx context.Context) (int64, error) {
	var removed int64
	for range ratelimit.MaxPruneRounds {
		result, err := l.db.ExecContext(ctx, `
			DELETE FROM forge_rate_limits
			WHERE expires_at <= UTC_TIMESTAMP(6)
			ORDER BY expires_at
			LIMIT ?
		`, l.opts.PruneBatch)
		if err != nil {
			return removed, forgemysql.Translate(fmt.Errorf("prune rate limits: %w", err))
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
