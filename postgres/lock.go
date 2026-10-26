package postgres

import (
	"context"
	"fmt"
	"hash/fnv"

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
