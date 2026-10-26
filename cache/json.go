package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rocketinsidecorp/forge-go/web"
)

// GetJSON decodes the value under key into a T. A value that no longer
// decodes (for example after the type changed in a deploy, or a poisoned
// entry) is a miss, so a stale entry is recomputed, not served or turned into
// an error; it is counted (DecodeFailures) and logged once per key (see
// DecodeFailures).
func GetJSON[T any](ctx context.Context, c Cache, key string) (T, bool, error) {
	return getJSON[T](ctx, c, key, true)
}

// getJSON is GetJSON; report false leaves a decode failure unrecorded, for a
// caller that reads the entry again and records it then.
func getJSON[T any](ctx context.Context, c Cache, key string, report bool) (T, bool, error) {
	var zero T
	raw, ok, err := c.Get(ctx, key)
	if err != nil || !ok {
		return zero, false, err
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		if report {
			recordDecodeFailure(ctx, key)
		}
		return zero, false, nil
	}
	return value, true, nil
}

// decodeFailures counts the cached values GetJSON and FetchJSON could not
// decode.
var decodeFailures atomic.Uint64

// maxLoggedDecodeKeys bounds the keys whose first decode failure is logged,
// and so the memory the log-once set holds.
const maxLoggedDecodeKeys = 1024

// loggedDecodeKeys holds the hashes of the keys whose decode failure was
// already logged.
var loggedDecodeKeys = struct {
	sync.Mutex
	seen map[string]struct{}
}{seen: make(map[string]struct{})}

// DecodeFailures reports how many cached values GetJSON and FetchJSON found
// that no longer decoded into the requested type since the process started.
// Each was treated as a miss. The first failure of each key (up to 1024
// keys per process) is also logged at warn level, with the key's SHA-256
// prefix (key_sha256), never the key or the value, through the request's
// logger (web.Logger) or, outside a request, slog.Default. A growing count
// means a type changed without a new key version (see docs/CACHING.md) or
// an entry was written by something else.
func DecodeFailures() uint64 { return decodeFailures.Load() }

func recordDecodeFailure(ctx context.Context, key string) {
	decodeFailures.Add(1)
	sum := sha256.Sum256([]byte(key))
	hashed := hex.EncodeToString(sum[:8])
	loggedDecodeKeys.Lock()
	_, logged := loggedDecodeKeys.seen[hashed]
	if !logged && len(loggedDecodeKeys.seen) < maxLoggedDecodeKeys {
		loggedDecodeKeys.seen[hashed] = struct{}{}
	} else {
		logged = true
	}
	loggedDecodeKeys.Unlock()
	if logged {
		return
	}
	logger := web.Logger(ctx)
	if !logger.Enabled(ctx, slog.LevelWarn) {
		logger = slog.Default()
	}
	logger.WarnContext(ctx, "cached value does not decode; treated as a miss (logged once per key)", "key_sha256", hashed)
}

// SetJSON stores value as JSON under key for ttl.
func SetJSON[T any](ctx context.Context, c Cache, key string, value T, ttl time.Duration) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.Set(ctx, key, raw, ttl)
}

// FetchJSON is Fetch for a typed value: the result of compute is stored as
// JSON, and a cached value that no longer decodes is recomputed.
func FetchJSON[T any](ctx context.Context, c Cache, key string, ttl time.Duration, compute func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	// A value that does not decode is recorded below, where Fetch reads it
	// again.
	if value, ok, err := getJSON[T](ctx, c, key, false); err == nil && ok {
		return value, nil
	}
	raw, err := c.Fetch(ctx, key, ttl, func(ctx context.Context) ([]byte, error) {
		value, err := compute(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(value)
	})
	if err != nil {
		return zero, err
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		// Fetch returned an entry that does not decode: replace it.
		recordDecodeFailure(ctx, key)
		value, err = compute(ctx)
		if err != nil {
			return zero, err
		}
		if err := SetJSON(ctx, c, key, value, ttl); err != nil {
			return zero, err
		}
	}
	return value, nil
}
