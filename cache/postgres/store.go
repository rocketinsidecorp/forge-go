// Package postgres is the PostgreSQL adapter of Forge's shared cache
// (ADR 0027), the equivalent of Solid Cache: entries live in a table keyed by
// the SHA-256 of the cache key, all expiry arithmetic uses the database
// clock, and pruning removes expired entries and, above MaxEntries, the oldest
// writes, in bounded batches.
package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/cache"
	forgepostgres "github.com/rocketinsidecorp/forge-go/postgres"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations returns the cache's migration rooted at ".". Compose it with the
// application's own through
// postgres.CommandsFromSets(app.Migrations(), cachepostgres.Migrations()).
func Migrations() fs.FS {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

// Store is the PostgreSQL implementation of cache.Cache and cache.Pruner. It
// is safe for concurrent use and starts no goroutines.
type Store struct {
	db     *forgepostgres.DB
	opts   cache.StoreOptions
	flight cache.Flight
	gate   cache.PruneGate
}

var (
	_ cache.Cache  = (*Store)(nil)
	_ cache.Pruner = (*Store)(nil)
)

// NewStore returns a cache on db. The table comes from Migrations.
func NewStore(db *forgepostgres.DB, opts cache.StoreOptions) (*Store, error) {
	if db == nil {
		return nil, errors.New("cache PostgreSQL database is required")
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
	err := s.db.QueryRow(ctx, `
		SELECT value FROM forge_cache_entries
		WHERE key_hash = $1 AND expires_at > clock_timestamp()
	`, hash(key)).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, forgepostgres.Translate(fmt.Errorf("read cache entry: %w", err))
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
	if _, err := s.db.Exec(ctx, `
		INSERT INTO forge_cache_entries (key_hash, value, created_at, expires_at)
		VALUES ($1, $2, now(), now() + ($3 * interval '1 microsecond'))
		ON CONFLICT (key_hash) DO UPDATE
		SET value = EXCLUDED.value, created_at = EXCLUDED.created_at, expires_at = EXCLUDED.expires_at
	`, hash(key), value, ttl.Microseconds()); err != nil {
		return forgepostgres.Translate(fmt.Errorf("write cache entry: %w", err))
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
	if _, err := s.db.Exec(ctx, `DELETE FROM forge_cache_entries WHERE key_hash = $1`, hash(key)); err != nil {
		return forgepostgres.Translate(fmt.Errorf("delete cache entry: %w", err))
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
		tag, err := s.db.Exec(ctx, `
			DELETE FROM forge_cache_entries
			WHERE key_hash IN (
				SELECT key_hash FROM forge_cache_entries
				WHERE expires_at <= clock_timestamp()
				ORDER BY expires_at
				LIMIT $1
				FOR UPDATE SKIP LOCKED
			)
		`, s.opts.PruneBatch)
		if err != nil {
			return removed, forgepostgres.Translate(fmt.Errorf("prune expired cache entries: %w", err))
		}
		removed += tag.RowsAffected()
		if tag.RowsAffected() < int64(s.opts.PruneBatch) {
			break
		}
	}

	var count int64
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM forge_cache_entries`).Scan(&count); err != nil {
		return removed, forgepostgres.Translate(fmt.Errorf("count cache entries: %w", err))
	}
	excess := count - int64(s.opts.MaxEntries)
	for range cache.MaxPruneRounds {
		if excess <= 0 {
			break
		}
		tag, err := s.db.Exec(ctx, `
			DELETE FROM forge_cache_entries
			WHERE key_hash IN (
				SELECT key_hash FROM forge_cache_entries
				ORDER BY created_at
				LIMIT $1
				FOR UPDATE SKIP LOCKED
			)
		`, min(excess, int64(s.opts.PruneBatch)))
		if err != nil {
			return removed, forgepostgres.Translate(fmt.Errorf("evict cache entries: %w", err))
		}
		if tag.RowsAffected() == 0 {
			break
		}
		removed += tag.RowsAffected()
		excess -= tag.RowsAffected()
	}
	return removed, nil
}
