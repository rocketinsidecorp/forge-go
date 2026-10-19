// Package mysql is the MySQL and MariaDB adapter of Forge's shared cache
// (ADR 0027): entries live in a table addressed by the SHA-256 of the cache
// key, all expiry arithmetic uses the database clock (UTC_TIMESTAMP), and
// pruning removes expired entries and, above MaxEntries, the oldest writes, in
// bounded batches.
package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/rocketinsidecorp/forge-go/cache"
	forgemysql "github.com/rocketinsidecorp/forge-go/mysql"
	"github.com/rocketinsidecorp/forge-go/sqldb"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations returns the cache's migration rooted at ".". Compose it with the
// application's own through
// mysql.CommandsFromSets(app.Migrations(), cachemysql.Migrations()).
func Migrations() fs.FS {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

// Store is the MySQL/MariaDB implementation of cache.Cache and cache.Pruner.
// It is safe for concurrent use and starts no goroutines.
type Store struct {
	db     *sqldb.DB
	opts   cache.StoreOptions
	flight cache.Flight
	gate   cache.PruneGate
}

var (
	_ cache.Cache  = (*Store)(nil)
	_ cache.Pruner = (*Store)(nil)
)

// NewStore returns a cache on db, which mysql.Open returns. The table comes
// from Migrations.
func NewStore(db *sqldb.DB, opts cache.StoreOptions) (*Store, error) {
	if db == nil {
		return nil, errors.New("cache MySQL database is required")
	}
	opts, err := opts.Normalize()
	if err != nil {
		return nil, err
	}
	return &Store{db: db, opts: opts}, nil
}

func hash(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// Get implements cache.Cache.
func (s *Store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := cache.CheckKey(key); err != nil {
		return nil, false, err
	}
	var value []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT value FROM forge_cache_entries WHERE key_hash = ? AND expires_at > UTC_TIMESTAMP(6)
	`, hash(key)).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, forgemysql.Translate(fmt.Errorf("read cache entry: %w", err))
	}
	if value == nil {
		value = []byte{}
	}
	return value, true, nil
}

// Set implements cache.Cache.
func (s *Store) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ttl, err := s.opts.CheckSet(key, value, ttl)
	if err != nil {
		return err
	}
	if value == nil {
		value = []byte{} // nil would be sent as NULL
	}
	// VALUES(col) is deprecated in MySQL 8.0.20+ in favor of a row alias, which
	// MariaDB does not support; VALUES() works on both.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO forge_cache_entries (key_hash, value, created_at, expires_at)
		VALUES (?, ?, UTC_TIMESTAMP(6), DATE_ADD(UTC_TIMESTAMP(6), INTERVAL ? MICROSECOND))
		ON DUPLICATE KEY UPDATE
			value = VALUES(value), created_at = VALUES(created_at), expires_at = VALUES(expires_at)
	`, hash(key), value, ttl.Microseconds()); err != nil {
		return forgemysql.Translate(fmt.Errorf("write cache entry: %w", err))
	}
	s.maybePrune(ctx)
	return nil
}

// Delete implements cache.Cache.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cache.CheckKey(key); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM forge_cache_entries WHERE key_hash = ?`, hash(key)); err != nil {
		return forgemysql.Translate(fmt.Errorf("delete cache entry: %w", err))
	}
	return nil
}

// Fetch implements cache.Cache.
func (s *Store) Fetch(ctx context.Context, key string, ttl time.Duration, compute func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	return cache.FetchThrough(ctx, s, &s.flight, key, ttl, compute)
}

// maybePrune prunes when the gate says so: once per PruneInterval, or sooner
// after PruneBatch writes. It ignores errors: the write it follows has succeeded.
func (s *Store) maybePrune(ctx context.Context) {
	if s.gate.Due(s.opts) {
		_, _ = s.Prune(ctx)
	}
}

// Prune removes expired entries (up to MaxPruneRounds batches of PruneBatch)
// and, when more than MaxEntries remain, the oldest writes (likewise bounded).
// It returns how many entries it removed. One call is bounded work yet keeps up
// with sustained writes of new keys, and the stores call it after every
// PruneBatch writes, so the table stays within about one batch of MaxEntries per
// process. Counting the entries is a scan; with millions of entries, raise
// PruneBatch to count less often.
func (s *Store) Prune(ctx context.Context) (int64, error) {
	var removed int64
	for range cache.MaxPruneRounds {
		result, err := s.db.ExecContext(ctx, `
			DELETE FROM forge_cache_entries
			WHERE expires_at <= UTC_TIMESTAMP(6)
			ORDER BY expires_at
			LIMIT ?
		`, s.opts.PruneBatch)
		if err != nil {
			return removed, forgemysql.Translate(fmt.Errorf("prune expired cache entries: %w", err))
		}
		n, err := result.RowsAffected()
		if err != nil {
			return removed, err
		}
		removed += n
		if n < int64(s.opts.PruneBatch) {
			break
		}
	}

	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM forge_cache_entries`).Scan(&count); err != nil {
		return removed, forgemysql.Translate(fmt.Errorf("count cache entries: %w", err))
	}
	excess := count - int64(s.opts.MaxEntries)
	for range cache.MaxPruneRounds {
		if excess <= 0 {
			break
		}
		result, err := s.db.ExecContext(ctx, `
			DELETE FROM forge_cache_entries
			ORDER BY created_at
			LIMIT ?
		`, min(excess, int64(s.opts.PruneBatch)))
		if err != nil {
			return removed, forgemysql.Translate(fmt.Errorf("evict cache entries: %w", err))
		}
		n, err := result.RowsAffected()
		if err != nil {
			return removed, err
		}
		if n == 0 {
			break
		}
		removed += n
		excess -= n
	}
	return removed, nil
}
