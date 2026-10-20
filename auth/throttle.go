package auth

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
)

var ErrLoginThrottled = fault.New("login_throttled", "too many login attempts", fault.CategoryRateLimited, 0)

// LoginThrottleKey is the digest identity of one attempt. The throttle never
// sees a login or an address, only these SHA-256 digests.
//
// Account is the identity the attempt names (the canonical login), Source the
// network source (IPv6 addresses are counted by their /64 prefix) and Pair
// the two together. Attempts are counted against all three, each with its own
// limit, so one source cannot lock a known login out for everybody (the
// tight limit is per login and source), guessing one login from many sources
// is still capped (the account limit), and password spraying from one source
// is bounded (the source limit).
type LoginThrottleKey struct {
	Account   [sha256.Size]byte
	Source    [sha256.Size]byte
	HasSource bool
	Pair      [sha256.Size]byte
	HasPair   bool
}

type LoginThrottleDecision struct {
	Allowed    bool
	RetryAfter time.Duration
}

// LoginThrottler counts attempts. Attempt counts one attempt, or refuses it
// without counting it when any of its counters is exhausted; Success clears
// the attempt's pair and account counters but never its source counter, so
// one good password does not reset the pressure of a source spraying many
// accounts.
type LoginThrottler interface {
	Attempt(ctx context.Context, key LoginThrottleKey) (LoginThrottleDecision, error)
	Success(ctx context.Context, key LoginThrottleKey) error
}

type LoginThrottledError struct {
	RetryAfter time.Duration
}

func (e *LoginThrottledError) Error() string { return ErrLoginThrottled.Error() }

func (e *LoginThrottledError) Unwrap() error { return ErrLoginThrottled }

// LoginThrottleConfig is a throttle policy. Every limit counts attempts in
// one Window.
type LoginThrottleConfig struct {
	Window time.Duration
	// PairLimit is the number of attempts per account and source. It is the
	// tight limit, so a guesser is stopped quickly while the owner, from
	// another source, is not. Zero disables the pair counter (the account
	// limit then applies to every source together, as it did before the
	// pair counter existed).
	PairLimit int
	// AccountLimit caps the attempts per account from all sources together.
	AccountLimit int
	// SourceLimit caps the attempts per source across accounts.
	SourceLimit int
	// MaxEntries bounds the counters the in-memory throttler holds. When it
	// is reached the oldest counters that block nothing are evicted; only
	// when every counter is blocking are new attempts refused. The
	// PostgreSQL throttler ignores it: it prunes expired counters as it
	// counts (auth/postgres).
	MaxEntries int
}

// DefaultLoginThrottleConfig is the sign-in policy: in 15 minutes, 5 attempts
// per login and source, 50 per login and 300 per source, with at most 20,000
// in-memory counters.
func DefaultLoginThrottleConfig() LoginThrottleConfig {
	return LoginThrottleConfig{
		Window:       15 * time.Minute,
		PairLimit:    5,
		AccountLimit: 50,
		SourceLimit:  300,
		MaxEntries:   20_000,
	}
}

func (config LoginThrottleConfig) Validate() error {
	if config.Window <= 0 {
		return errors.New("login throttle window must be positive")
	}
	if config.AccountLimit < 1 || config.SourceLimit < 1 {
		return errors.New("login throttle limits must be positive")
	}
	if config.PairLimit < 0 || config.PairLimit > config.AccountLimit {
		return errors.New("login throttle pair limit must be between zero (disabled) and the account limit")
	}
	if config.MaxEntries < 3 {
		return errors.New("login throttle max entries must be at least 3")
	}
	return nil
}

// Throttle counter scopes. auth/postgres stores them in forge_login_throttle.
const (
	AccountThrottleScope int16 = 1
	SourceThrottleScope  int16 = 2
	PairThrottleScope    int16 = 3
)

// ThrottleCounter is one counter an attempt is counted against.
type ThrottleCounter struct {
	Scope  int16
	Digest [sha256.Size]byte
	Limit  int
}

// Counters returns the counters of key in a fixed order (account, source,
// pair), each with its limit from config. A pair counter exists only when
// the key has one and the config enables it. Throttlers must lock or touch
// the counters in this order.
func (config LoginThrottleConfig) Counters(key LoginThrottleKey) []ThrottleCounter {
	counters := make([]ThrottleCounter, 0, 3)
	counters = append(counters, ThrottleCounter{Scope: AccountThrottleScope, Digest: key.Account, Limit: config.AccountLimit})
	if key.HasSource {
		counters = append(counters, ThrottleCounter{Scope: SourceThrottleScope, Digest: key.Source, Limit: config.SourceLimit})
	}
	if key.HasPair && config.PairLimit > 0 {
		counters = append(counters, ThrottleCounter{Scope: PairThrottleScope, Digest: key.Pair, Limit: config.PairLimit})
	}
	return counters
}

// MemoryLoginThrottler is the bounded in-process LoginThrottler. Counters of
// one process do not limit another: replicated deployments use
// auth/postgres.LoginThrottler.
//
// Counters live in a fixed window from their first attempt. They are kept in
// creation order, which is also expiry order, so expired counters are
// removed from the front in constant time per counter and, when the table is
// full, the oldest counters that block nothing are evicted: filling the
// table from many sources neither flushes a blocked attacker's counter nor
// refuses everybody.
type MemoryLoginThrottler struct {
	mu       sync.Mutex
	config   LoginThrottleConfig
	now      func() time.Time
	buckets  map[throttleBucketKey]*throttleBucket
	order    *list.List // *throttleBucket, oldest first
	blocking int        // buckets with count >= limit
}

type throttleBucketKey struct {
	scope  int16
	digest [sha256.Size]byte
}

type throttleBucket struct {
	key     throttleBucketKey
	count   int
	limit   int
	resetAt time.Time
	element *list.Element
}

func (b *throttleBucket) blocking() bool { return b.count >= b.limit }

func NewMemoryLoginThrottler(config LoginThrottleConfig) (*MemoryLoginThrottler, error) {
	return newMemoryLoginThrottler(config, time.Now)
}

func newMemoryLoginThrottler(config LoginThrottleConfig, now func() time.Time) (*MemoryLoginThrottler, error) {
	if now == nil {
		return nil, errors.New("login throttle clock is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &MemoryLoginThrottler{
		config:  config,
		now:     now,
		buckets: make(map[throttleBucketKey]*throttleBucket),
		order:   list.New(),
	}, nil
}

func (m *MemoryLoginThrottler) Attempt(ctx context.Context, key LoginThrottleKey) (LoginThrottleDecision, error) {
	if err := ctx.Err(); err != nil {
		return LoginThrottleDecision{}, err
	}

	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expire(now)

	counters := m.config.Counters(key)
	var retryAfter time.Duration
	missing, presentCurrent := 0, 0
	for _, counter := range counters {
		bucket := m.live(throttleBucketKey{scope: counter.Scope, digest: counter.Digest}, now)
		if bucket == nil {
			missing++
			continue
		}
		presentCurrent++
		if bucket.blocking() {
			if retry := bucket.resetAt.Sub(now); retry > retryAfter {
				retryAfter = retry
			}
		}
	}
	if retryAfter > 0 {
		return LoginThrottleDecision{Allowed: false, RetryAfter: retryAfter}, nil
	}

	if overflow := len(m.buckets) + missing - m.config.MaxEntries; overflow > 0 {
		// Every present counter of this attempt is idle (a blocking one was
		// refused above) and must survive, so only the others can go.
		if len(m.buckets)-m.blocking-presentCurrent < overflow || !m.evict(counters, overflow) {
			return LoginThrottleDecision{Allowed: false, RetryAfter: m.config.Window}, nil
		}
	}

	for _, counter := range counters {
		k := throttleBucketKey{scope: counter.Scope, digest: counter.Digest}
		bucket := m.buckets[k]
		if bucket == nil {
			bucket = &throttleBucket{key: k, limit: counter.Limit, resetAt: now.Add(m.config.Window)}
			bucket.element = m.order.PushBack(bucket)
			m.buckets[k] = bucket
		}
		bucket.count++
		if bucket.count == bucket.limit {
			m.blocking++
		}
	}
	return LoginThrottleDecision{Allowed: true}, nil
}

func (m *MemoryLoginThrottler) Success(ctx context.Context, key LoginThrottleKey) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.remove(m.buckets[throttleBucketKey{scope: AccountThrottleScope, digest: key.Account}])
	if key.HasPair {
		m.remove(m.buckets[throttleBucketKey{scope: PairThrottleScope, digest: key.Pair}])
	}
	return nil
}

// expire removes the counters whose window ended. Creation order is expiry
// order, so it stops at the first live one.
func (m *MemoryLoginThrottler) expire(now time.Time) {
	for front := m.order.Front(); front != nil; front = m.order.Front() {
		bucket := front.Value.(*throttleBucket)
		if now.Before(bucket.resetAt) {
			return
		}
		m.remove(bucket)
	}
}

// live returns the counter of k, removing it first when its window ended
// (the clock can step backwards, which expire alone would not notice).
func (m *MemoryLoginThrottler) live(k throttleBucketKey, now time.Time) *throttleBucket {
	bucket := m.buckets[k]
	if bucket != nil && !now.Before(bucket.resetAt) {
		m.remove(bucket)
		return nil
	}
	return bucket
}

func (m *MemoryLoginThrottler) remove(bucket *throttleBucket) {
	if bucket == nil {
		return
	}
	if bucket.blocking() {
		m.blocking--
	}
	m.order.Remove(bucket.element)
	delete(m.buckets, bucket.key)
}

// evict removes the n oldest counters that block nothing and are not part of
// the current attempt, and reports false (removing nothing) if fewer exist.
func (m *MemoryLoginThrottler) evict(current []ThrottleCounter, n int) bool {
	victims := make([]*throttleBucket, 0, n)
	for element := m.order.Front(); element != nil && len(victims) < n; element = element.Next() {
		bucket := element.Value.(*throttleBucket)
		if bucket.blocking() || isCurrent(current, bucket.key) {
			continue
		}
		victims = append(victims, bucket)
	}
	if len(victims) < n {
		return false
	}
	for _, victim := range victims {
		m.remove(victim)
	}
	return true
}

func isCurrent(current []ThrottleCounter, k throttleBucketKey) bool {
	for _, counter := range current {
		if counter.Scope == k.scope && counter.Digest == k.digest {
			return true
		}
	}
	return false
}

// newThrottleKey derives the key of an attempt. namespace separates
// independent limits (sign-in, recovery); account is the identity the
// attempt names whether or not it exists; source is the network source as
// the request adapter reports it ("" for none).
func newThrottleKey(namespace, account, source string, withPair bool) LoginThrottleKey {
	key := LoginThrottleKey{Account: throttleDigest(namespace+"-account", account)}
	if source = throttleSource(source); source != "" {
		key.Source = throttleDigest(namespace+"-source", source)
		key.HasSource = true
		if withPair {
			key.Pair = throttleDigest(namespace+"-pair", account, source)
			key.HasPair = true
		}
	}
	return key
}

// throttleDigest hashes the parts with a domain label and length prefixes,
// so no two different (label, parts) share an input.
func throttleDigest(label string, parts ...string) [sha256.Size]byte {
	h := sha256.New()
	var length [binary.MaxVarintLen64]byte
	for _, part := range append([]string{label}, parts...) {
		h.Write(length[:binary.PutUvarint(length[:], uint64(len(part)))])
		h.Write([]byte(part))
	}
	var digest [sha256.Size]byte
	h.Sum(digest[:0])
	return digest
}

// throttleSource is the text a source is counted by: an IPv4 address (an
// IPv4-mapped IPv6 address counts as its IPv4 address) or an IPv6 /64, one
// subscriber's usual allocation, so rotating through the addresses of a /64
// does not give an attacker fresh counters. Text that is not an IP address
// is counted as it is.
func throttleSource(source string) string {
	source = strings.TrimSpace(source)
	address, err := netip.ParseAddr(source)
	if err != nil {
		return source
	}
	address = address.WithZone("").Unmap()
	if address.Is6() {
		prefix, err := address.Prefix(64)
		if err != nil {
			return source
		}
		return prefix.String()
	}
	return address.String()
}

func deriveLoginThrottleKey(account, source string) LoginThrottleKey {
	return newThrottleKey("login", account, source, true)
}
