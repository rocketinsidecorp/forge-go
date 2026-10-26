package auth

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
)

// A memory-hard hash holds its whole working set while it runs: Forge's
// Argon2id policy takes 19 MiB per hash or verification. Every sign-in
// attempt hashes (unknown logins too, to keep timing uniform), and the
// per-account and per-source throttles bound attempts per key, not in total,
// so without a process-wide bound a burst of attempts could reserve
// gigabytes. PasswordWorkLimiter is that bound, as forge::auth's
// PasswordWorkLimiter is in Forge-Rust.
const (
	// MaxDefaultPasswordWork is the most hashes DefaultPasswordWorkLimiter
	// runs at once, whatever the CPU count: 16 × 19 MiB is about 300 MiB.
	MaxDefaultPasswordWork = 16
	// DefaultPasswordWorkWait is how long DefaultPasswordWorkLimiter lets an
	// attempt wait for a slot before it fails with
	// ErrPasswordHashingUnavailable.
	DefaultPasswordWorkWait = 2 * time.Second

	// passwordWorkRetryAfter is the Retry-After of the 503 a saturated
	// limiter causes.
	passwordWorkRetryAfter = 5 * time.Second
)

// ErrPasswordHashingUnavailable means every password-hashing slot stayed busy
// for the limiter's whole wait. HTTP adapters answer it 503 with Retry-After.
var ErrPasswordHashingUnavailable = &fault.Error{
	Code:      "authentication_unavailable",
	Message:   "service temporarily unavailable",
	Category:  fault.CategoryUnavailable,
	Retryable: true,
}

// PasswordWorkLimiter bounds how many password hashes and verifications run at
// once in the process. An attempt that finds every slot busy waits up to the
// limiter's wait and then fails with ErrPasswordHashingUnavailable, so waiting
// work cannot pile up either. The slot is held by the hashing itself: it is
// released only when the hash ends, whether or not the caller still waits.
type PasswordWorkLimiter struct {
	slots   chan struct{}
	maxWait time.Duration
}

// NewPasswordWorkLimiter returns a limiter running at most maxConcurrent
// hashes at once; an attempt waits at most maxWait for a slot (zero: fail at
// once when every slot is busy).
func NewPasswordWorkLimiter(maxConcurrent int, maxWait time.Duration) (*PasswordWorkLimiter, error) {
	if maxConcurrent < 1 {
		return nil, errors.New("password work limiter needs at least one slot")
	}
	if maxWait < 0 {
		return nil, errors.New("password work wait must not be negative")
	}
	return &PasswordWorkLimiter{slots: make(chan struct{}, maxConcurrent), maxWait: maxWait}, nil
}

var defaultPasswordWorkLimiter = sync.OnceValue(func() *PasswordWorkLimiter {
	return &PasswordWorkLimiter{slots: make(chan struct{}, defaultPasswordWorkSlots()), maxWait: DefaultPasswordWorkWait}
})

// DefaultPasswordWorkLimiter returns the limiter every default PasswordHasher
// of the process shares: one slot per CPU the process may use (a hash is
// CPU-bound, so more would not finish sooner), at most
// MaxDefaultPasswordWork, waiting up to DefaultPasswordWorkWait.
func DefaultPasswordWorkLimiter() *PasswordWorkLimiter {
	return defaultPasswordWorkLimiter()
}

// defaultPasswordWorkSlots is GOMAXPROCS (which follows the container's CPU
// limit) bounded to 1..MaxDefaultPasswordWork.
func defaultPasswordWorkSlots() int {
	return min(max(runtime.GOMAXPROCS(0), 1), MaxDefaultPasswordWork)
}

// MaxConcurrent returns the bound on concurrent hashes.
func (l *PasswordWorkLimiter) MaxConcurrent() int {
	return cap(l.slots)
}

// Do runs work once a slot is free. It returns ErrPasswordHashingUnavailable
// when no slot frees up within the wait, or ctx's error when ctx ends first;
// work has not run then.
func (l *PasswordWorkLimiter) Do(ctx context.Context, work func()) error {
	select {
	case l.slots <- struct{}{}:
	default:
		if l.maxWait == 0 {
			return ErrPasswordHashingUnavailable
		}
		timer := time.NewTimer(l.maxWait)
		defer timer.Stop()
		select {
		case l.slots <- struct{}{}:
		case <-timer.C:
			return ErrPasswordHashingUnavailable
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer func() { <-l.slots }()
	work()
	return nil
}

// setPasswordWorkRetryAfter sets Retry-After when err is a saturated
// PasswordWorkLimiter, whose 503 the caller then writes.
func setPasswordWorkRetryAfter(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrPasswordHashingUnavailable) {
		w.Header().Set("Retry-After", strconv.Itoa(int(passwordWorkRetryAfter/time.Second)))
	}
}
