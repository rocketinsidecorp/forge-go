// Package ratelimit limits how often something may happen per key: Rails'
// rate_limit, with fail-closed defaults (ADR 0031).
//
// A Limiter counts hits per key in fixed windows. The window of a key starts
// with its first hit and lasts Limit.Per; at most Limit.N hits are allowed in
// it. Fixed windows are cheap and atomic on every database, at the price of
// the known boundary effect: a client that times its requests around a window
// edge can send up to 2N within Per. Choose N accordingly.
//
// Memory is a bounded, process-local limiter with no background work.
// Applications with several replicas use the shared limiter of
// ratelimit/postgres, ratelimit/mysql or ratelimit/sqlite so that the limit
// holds across processes instead of being multiplied by their number.
//
// Middleware applies a Limit to HTTP requests. What identifies a client is
// the application's decision (a KeyFunc); ByClientIP, the client address
// resolved under the trusted-proxy policy (ADR 0037), is the usual choice.
//
//	limiter, _ := ratelimit.NewMemory(ratelimit.DefaultMemoryOptions())
//	create, err := ratelimit.Middleware(limiter, ratelimit.ByClientIP, ratelimit.Limit{N: 10, Per: 3 * time.Minute})
//	if err != nil { ... }
//	router.Handle("POST /comments", create(http.HandlerFunc(createComment)))
package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
)

// Limit allows N hits per window of length Per.
type Limit struct {
	N   int
	Per time.Duration
}

const (
	// MaxN is the largest Limit.N.
	MaxN = 1 << 30
	// MinPer and MaxPer bound Limit.Per.
	MinPer = time.Millisecond
	MaxPer = 30 * 24 * time.Hour
	// MaxKeyLength is the longest key a Limiter accepts. Keys made by Key are
	// 64 characters.
	MaxKeyLength = 128
)

// Validate reports whether the limit is usable.
func (l Limit) Validate() error {
	if l.N < 1 || l.N > MaxN {
		return fmt.Errorf("rate limit N must be between 1 and %d", MaxN)
	}
	if l.Per < MinPer || l.Per > MaxPer {
		return fmt.Errorf("rate limit window must be between %s and %s", MinPer, MaxPer)
	}
	return nil
}

// Decision is the outcome of one hit.
type Decision struct {
	// Allowed is false when the hit exceeded the limit.
	Allowed bool
	// Remaining hits in the current window, never negative.
	Remaining int
	// RetryAfter is how long until the window ends and hits are allowed
	// again. It is positive only when Allowed is false.
	RetryAfter time.Duration
}

// Limiter counts one hit for key and reports whether it is within limit.
//
// Keys are opaque ASCII tokens of at most MaxKeyLength characters; derive
// them with Key, which also keeps personal data (addresses, e-mail
// addresses) out of shared stores. Every call counts as a hit, including
// calls that are denied. Implementations are safe for concurrent use. A
// non-nil error means the limiter could not decide: callers must choose
// whether to fail closed (Middleware's default) or open.
type Limiter interface {
	Allow(ctx context.Context, key string, limit Limit) (Decision, error)
}

// Pruner is implemented by limiters with persistent storage, which keep
// expired buckets until pruned. Each call removes at most one bounded batch
// and returns how many buckets it removed; call it periodically (for
// example from jobs.Scheduler) and the stores also prune opportunistically.
type Pruner interface {
	Prune(ctx context.Context) (int64, error)
}

// StoreOptions tunes a database-backed limiter. Zero values select the
// defaults.
type StoreOptions struct {
	// PruneInterval is the longest time between the opportunistic prunes that
	// Allow runs inline; a prune also runs after every PruneBatch hits, so
	// expired buckets cannot pile up faster than they are removed. Zero means
	// one minute; a negative value disables inline pruning (then call Prune
	// yourself).
	PruneInterval time.Duration
	// PruneBatch is the size of one prune batch; a Prune call runs at most
	// MaxPruneRounds batches. Zero means 500.
	PruneBatch int
}

// Normalize applies the defaults and validates the result.
func (o StoreOptions) Normalize() (StoreOptions, error) {
	if o.PruneInterval == 0 {
		o.PruneInterval = time.Minute
	}
	if o.PruneBatch == 0 {
		o.PruneBatch = 500
	}
	if o.PruneBatch < 0 || o.PruneBatch > 100_000 {
		return StoreOptions{}, errors.New("rate limit prune batch must be between 1 and 100000")
	}
	return o, nil
}

// ErrInvalidKey is returned for an empty, too long or non-ASCII key.
var ErrInvalidKey = errors.New("ratelimit: key must be 1 to 128 printable ASCII characters")

// CheckRequest validates the arguments of Allow. Limiter implementations call
// it first so that every store rejects the same inputs.
func CheckRequest(key string, limit Limit) error {
	if len(key) == 0 || len(key) > MaxKeyLength {
		return ErrInvalidKey
	}
	for i := range len(key) {
		if c := key[i]; c <= ' ' || c >= 0x7f {
			return ErrInvalidKey
		}
	}
	return limit.Validate()
}

// RetryAfterSeconds returns d as the whole seconds a Retry-After header
// carries, rounded up and at least 1.
func RetryAfterSeconds(d time.Duration) int {
	seconds := int((d + time.Second - 1) / time.Second)
	return max(seconds, 1)
}

// Key derives a store key from a scope (what is limited, such as the route)
// and the parts that identify the client. The result is a SHA-256 digest in
// hex: fixed length, free of personal data, and unambiguous because every
// part is length-prefixed ("a","bc" and "ab","c" differ).
func Key(scope string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte("forge.ratelimit.v1"))
	write := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	write(scope)
	for _, part := range parts {
		write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// MemoryOptionsFromConfig returns the Memory options FORGE_RATE_LIMIT_MAX_KEYS
// selects.
func MemoryOptionsFromConfig(cfg config.RateLimit) MemoryOptions {
	return MemoryOptions{MaxKeys: cfg.EffectiveMaxKeys()}
}
