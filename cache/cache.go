// Package cache is Forge's application cache, the equivalent of Rails'
// Solid Cache (ADR 0027): byte values under string keys with a time to live,
// a bounded in-memory implementation, and database-backed stores
// (cache/postgres, cache/mysql, cache/sqlite) that every replica shares.
//
// A cache holds copies: it is an optimization the application must be able to
// live without. Values are opaque []byte (use GetJSON, SetJSON and FetchJSON
// for typed values), keys and values are never logged, and every store is
// bounded: a maximum value size, a maximum number of entries, and expiry.
//
// Fetch is the read-through entry point. It protects the origin from a
// stampede: concurrent callers that miss the same key in one process wait for
// a single computation instead of each running it. Replicas do not coordinate,
// so across N replicas an expensive computation can run up to N times.
//
// Cached data is only as trustworthy as whoever can write the cache: do not
// put the authorization decision of one user under a key another can choose,
// and namespace keys with Key (see docs/CACHING.md).
package cache

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
)

// Cache stores byte values under keys for a limited time.
//
// Implementations are safe for concurrent use. Get and Set copy: the caller
// may modify a slice after passing or receiving it. A miss is not an error.
type Cache interface {
	// Get returns the value stored under key and true, or false when there is
	// none or it expired.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set stores value under key for ttl, replacing any previous value. A ttl
	// of zero selects the store's default; a negative ttl is an error.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Delete removes key. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// Fetch returns the cached value of key, or on a miss calls compute, stores
	// its result for ttl and returns it. Concurrent misses of one key in the
	// same process share one compute call. An error from compute is returned
	// and nothing is stored. Failures of the cache itself never fail Fetch:
	// the value is computed without it (and a failed write is logged).
	Fetch(ctx context.Context, key string, ttl time.Duration, compute func(ctx context.Context) ([]byte, error)) ([]byte, error)
}

// Pruner is implemented by caches with persistent storage, which keep
// expired and surplus entries until pruned. Each call removes at most one
// bounded batch of each kind and returns how many entries it removed; call
// it periodically (for example from jobs.Scheduler). The stores also prune
// opportunistically on writes.
type Pruner interface {
	Prune(ctx context.Context) (int64, error)
}

// Limits and defaults shared by every store.
const (
	// MaxKeyLength is the longest key in bytes.
	MaxKeyLength = 1024
	// MaxValueBytesLimit is the largest MaxValueBytes a store accepts: 8 MiB,
	// well inside the MEDIUMBLOB of MySQL and MariaDB and the packet limits of
	// all three databases. Cache big objects with the storage package instead.
	MaxValueBytesLimit = 8 << 20

	defaultMaxEntries    = 10_000
	defaultMaxValueBytes = 1 << 20
	defaultTTL           = time.Hour
	maxEntriesLimit      = 100_000_000
	maxTTL               = 365 * 24 * time.Hour
)

var (
	// ErrInvalidKey is returned for an empty, too long or NUL-containing key.
	ErrInvalidKey = errors.New("cache: key must be 1 to 1024 bytes without NUL")
	// ErrInvalidTTL is returned for a negative or excessive time to live.
	ErrInvalidTTL = errors.New("cache: ttl must not be negative or exceed one year")
	// ErrValueTooLarge is returned when a value exceeds MaxValueBytes.
	ErrValueTooLarge = fault.New("cache_value_too_large", "cache value too large", fault.CategoryInvalid, 0)
)

// Options bounds a cache. Zero values select the defaults.
type Options struct {
	// MaxEntries bounds the number of entries (default 10000). Memory enforces
	// it on every write; the database stores enforce it when pruning, so they
	// can exceed it briefly between prunes.
	MaxEntries int
	// MaxValueBytes bounds one value (default 1 MiB, at most 8 MiB).
	MaxValueBytes int
	// DefaultTTL applies when Set or Fetch is given a ttl of zero (default 1h).
	DefaultTTL time.Duration
}

// Normalize applies the defaults and validates the result.
func (o Options) Normalize() (Options, error) {
	if o.MaxEntries == 0 {
		o.MaxEntries = defaultMaxEntries
	}
	if o.MaxValueBytes == 0 {
		o.MaxValueBytes = defaultMaxValueBytes
	}
	if o.DefaultTTL == 0 {
		o.DefaultTTL = defaultTTL
	}
	switch {
	case o.MaxEntries < 1 || o.MaxEntries > maxEntriesLimit:
		return Options{}, fmt.Errorf("cache max entries must be between 1 and %d", maxEntriesLimit)
	case o.MaxValueBytes < 1 || o.MaxValueBytes > MaxValueBytesLimit:
		return Options{}, fmt.Errorf("cache max value bytes must be between 1 and %d", MaxValueBytesLimit)
	case o.DefaultTTL < time.Millisecond || o.DefaultTTL > maxTTL:
		return Options{}, errors.New("cache default ttl must be between 1ms and one year")
	}
	return o, nil
}

// StoreOptions tunes a database-backed cache.
type StoreOptions struct {
	Options
	// PruneInterval is the minimum time between the opportunistic prunes that
	// Set runs inline. Zero means one minute; a negative value disables them
	// (then call Prune yourself).
	PruneInterval time.Duration
	// PruneBatch bounds the entries one prune removes per kind (expired,
	// surplus). Zero means 500.
	PruneBatch int
}

// Normalize applies the defaults and validates the result.
func (o StoreOptions) Normalize() (StoreOptions, error) {
	base, err := o.Options.Normalize()
	if err != nil {
		return StoreOptions{}, err
	}
	o.Options = base
	if o.PruneInterval == 0 {
		o.PruneInterval = time.Minute
	}
	if o.PruneBatch == 0 {
		o.PruneBatch = 500
	}
	if o.PruneBatch < 0 || o.PruneBatch > 100_000 {
		return StoreOptions{}, errors.New("cache prune batch must be between 1 and 100000")
	}
	return o, nil
}

// CheckKey validates a key. Stores call it first so they all reject the same
// inputs. Errors never contain the key.
func CheckKey(key string) error {
	if len(key) == 0 || len(key) > MaxKeyLength {
		return ErrInvalidKey
	}
	for i := range len(key) {
		if key[i] == 0 {
			return ErrInvalidKey
		}
	}
	return nil
}

// CheckSet validates the arguments of Set and returns the effective ttl.
func (o Options) CheckSet(key string, value []byte, ttl time.Duration) (time.Duration, error) {
	if err := CheckKey(key); err != nil {
		return 0, err
	}
	if len(value) > o.MaxValueBytes {
		return 0, ErrValueTooLarge
	}
	return o.EffectiveTTL(ttl)
}

// EffectiveTTL resolves a caller's ttl: zero is the default.
func (o Options) EffectiveTTL(ttl time.Duration) (time.Duration, error) {
	if ttl < 0 || ttl > maxTTL {
		return 0, ErrInvalidTTL
	}
	if ttl == 0 {
		return o.DefaultTTL, nil
	}
	if ttl < time.Millisecond {
		return time.Millisecond, nil
	}
	return ttl, nil
}

// Key joins a namespace and parts into one unambiguous key: every part is
// length-prefixed, so ("a", "bc") and ("ab", "c") never collide, and a part
// that comes from a user cannot reach into another namespace.
func Key(namespace string, parts ...string) string {
	var b strings.Builder
	b.WriteString(namespace)
	for _, part := range parts {
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(len(part)))
		b.WriteByte(':')
		b.WriteString(part)
	}
	return b.String()
}
