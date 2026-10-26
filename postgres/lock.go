package postgres

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// LockKey derives a stable advisory lock key from a descriptive name, such as
// "billing:invoice-run". Names are hashed with 64-bit FNV-1a; use distinct,
// namespaced names to keep collisions negligible.
func LockKey(name string) int64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(name)) // hash.Hash writes never fail
	return int64(hash.Sum64())      // #nosec G115 -- bit reinterpretation into PostgreSQL's bigint key space is intended
}

// AdvisoryXactLock blocks until it holds the transaction-scoped advisory lock
// for key. PostgreSQL releases it at commit or rollback, so it cannot leak.
// Use it to serialize work across processes, such as a scheduled job that
// must not run twice.
func AdvisoryXactLock(ctx context.Context, tx pgx.Tx, key int64) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", key); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	return nil
}

// TryAdvisoryXactLock is AdvisoryXactLock without waiting: it reports whether
// the lock was acquired.
func TryAdvisoryXactLock(ctx context.Context, tx pgx.Tx, key int64) (bool, error) {
	var acquired bool
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", key).Scan(&acquired); err != nil {
		return false, fmt.Errorf("try advisory lock: %w", err)
	}
	return acquired, nil
}

// SchedulerLock is the jobs.Lock of PostgreSQL: one transaction-scoped
// advisory lock per tick of a scheduled job, keyed on LockKey of the job name
// and the tick time. Every replica that uses the same database agrees on it.
type SchedulerLock struct {
	db *DB
}

// NewSchedulerLock returns a scheduler lock on db's pool, for
// jobs.SchedulerOptions.Lock.
func NewSchedulerLock(db *DB) *SchedulerLock { return &SchedulerLock{db: db} }

// schedulerLockKey keys the lock of one tick of one job.
func schedulerLockKey(job string, tick time.Time) int64 {
	return LockKey("forge.jobs.tick\x00" + job + "\x00" + strconv.FormatInt(tick.UTC().UnixNano(), 10))
}

// TryLock takes the tick's lock without waiting. The lock lives in a
// transaction that holds one pooled connection until unlock rolls it back; if
// the rollback fails, pgx closes the connection, which releases the lock too,
// so the lock cannot leak into the pool. It works through PgBouncer in
// transaction pooling mode.
func (l *SchedulerLock) TryLock(ctx context.Context, job string, tick time.Time) (func(context.Context) error, bool, error) {
	if l == nil || l.db == nil {
		return nil, false, errors.New("scheduler lock needs a database")
	}
	tx, err := l.db.Pool().Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin scheduler lock transaction: %w", err)
	}
	unlock := func(ctx context.Context) error {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			return fmt.Errorf("release scheduler lock: %w", err)
		}
		return nil
	}
	acquired, err := TryAdvisoryXactLock(ctx, tx, schedulerLockKey(job, tick))
	if err != nil || !acquired {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = unlock(rollbackCtx)
		return nil, false, err
	}
	return unlock, true, nil
}
