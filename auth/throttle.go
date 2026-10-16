package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
)

var ErrLoginThrottled = fault.New("login_throttled", "too many login attempts", fault.CategoryRateLimited, 0)

type LoginThrottleKey struct {
	Account   [sha256.Size]byte
	Source    [sha256.Size]byte
	HasSource bool
}

type LoginThrottleDecision struct {
	Allowed    bool
	RetryAfter time.Duration
}

type LoginThrottler interface {
	Attempt(ctx context.Context, key LoginThrottleKey) (LoginThrottleDecision, error)
	Success(ctx context.Context, key LoginThrottleKey) error
}

type LoginThrottledError struct {
	RetryAfter time.Duration
}

func (e *LoginThrottledError) Error() string { return ErrLoginThrottled.Error() }

func (e *LoginThrottledError) Unwrap() error { return ErrLoginThrottled }

type LoginThrottleConfig struct {
	Window       time.Duration
	AccountLimit int
	SourceLimit  int
	MaxEntries   int
}

func DefaultLoginThrottleConfig() LoginThrottleConfig {
	return LoginThrottleConfig{
		Window:       15 * time.Minute,
		AccountLimit: 5,
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
	if config.MaxEntries < 2 {
		return errors.New("login throttle max entries must be at least 2")
	}
	return nil
}

type MemoryLoginThrottler struct {
	mu      sync.Mutex
	config  LoginThrottleConfig
	now     func() time.Time
	buckets map[throttleBucketKey]throttleBucket
}

type throttleBucketKey struct {
	scope  byte
	digest [sha256.Size]byte
}

type throttleBucket struct {
	count   int
	resetAt time.Time
}

const (
	accountThrottleScope byte = 1
	sourceThrottleScope  byte = 2
)

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
		buckets: make(map[throttleBucketKey]throttleBucket),
	}, nil
}

func (m *MemoryLoginThrottler) Attempt(ctx context.Context, key LoginThrottleKey) (LoginThrottleDecision, error) {
	if err := ctx.Err(); err != nil {
		return LoginThrottleDecision{}, err
	}

	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune(now)

	keys := []struct {
		key   throttleBucketKey
		limit int
	}{
		{key: throttleBucketKey{scope: accountThrottleScope, digest: key.Account}, limit: m.config.AccountLimit},
	}
	if key.HasSource {
		keys = append(keys, struct {
			key   throttleBucketKey
			limit int
		}{key: throttleBucketKey{scope: sourceThrottleScope, digest: key.Source}, limit: m.config.SourceLimit})
	}

	var retryAfter time.Duration
	for _, candidate := range keys {
		if bucket, ok := m.buckets[candidate.key]; ok && bucket.count >= candidate.limit {
			retry := bucket.resetAt.Sub(now)
			if retry > retryAfter {
				retryAfter = retry
			}
		}
	}
	if retryAfter > 0 {
		return LoginThrottleDecision{Allowed: false, RetryAfter: retryAfter}, nil
	}

	missing := 0
	for _, candidate := range keys {
		if _, ok := m.buckets[candidate.key]; !ok {
			missing++
		}
	}
	if len(m.buckets)+missing > m.config.MaxEntries {
		return LoginThrottleDecision{Allowed: false, RetryAfter: m.config.Window}, nil
	}

	for _, candidate := range keys {
		bucket, ok := m.buckets[candidate.key]
		if !ok {
			bucket.resetAt = now.Add(m.config.Window)
		}
		bucket.count++
		m.buckets[candidate.key] = bucket
	}
	return LoginThrottleDecision{Allowed: true}, nil
}

func (m *MemoryLoginThrottler) Success(ctx context.Context, key LoginThrottleKey) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.buckets, throttleBucketKey{scope: accountThrottleScope, digest: key.Account})
	m.mu.Unlock()
	return nil
}

func (m *MemoryLoginThrottler) prune(now time.Time) {
	for key, bucket := range m.buckets {
		if !now.Before(bucket.resetAt) {
			delete(m.buckets, key)
		}
	}
}

func deriveLoginThrottleKey(email, tenant, source string) LoginThrottleKey {
	key := LoginThrottleKey{
		Account: sha256.Sum256([]byte("account\x00" + tenant + "\x00" + email)),
	}
	if source != "" {
		key.Source = sha256.Sum256([]byte("source\x00" + source))
		key.HasSource = true
	}
	return key
}
