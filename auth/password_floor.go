package auth

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"sync"
	"sync/atomic"
	"time"
)

// While accounts still have PBKDF2 hashes, a wrong password against one costs
// a PBKDF2-600k verification (about 120 ms) and a wrong password against an
// Argon2id hash, or an unknown account, about 30 ms: the difference tells
// accounts that exist (and have not signed in since the upgrade) from the
// rest. legacyTimingFloor closes it: every failed password check is padded,
// by a timer that holds no limiter slot and burns no CPU, up to the cost of
// one PBKDF2 verification.
const (
	// legacyFloorCalibrations is how many PBKDF2 verifications calibrate the
	// floor; the slowest counts.
	legacyFloorCalibrations = 2
	// maxLegacyFloorFactor bounds how far an observed legacy verification can
	// raise the floor above the calibrated one. Observations only ever raise
	// it, so one slow outlier (a GC pause, a busy CPU) would otherwise slow
	// every later failure for the life of the process. A legacy hash stored
	// with more than about twice the default iterations can therefore still
	// take longer than the floor; Forge never wrote one.
	maxLegacyFloorFactor = 2
	// legacyFloorMarginPercent is added to the measured cost: one PBKDF2
	// verification varies by some 10-20% from call to call, and a legacy
	// check that ends after the floor would still stand out.
	legacyFloorMarginPercent = 25
)

// legacyTimingFloor is the least time a failed password check takes.
type legacyTimingFloor struct {
	// calibrate measures one PBKDF2 verification at the default work factor.
	calibrate func() time.Duration
	now       func() time.Time
	// wait blocks for d or until ctx ends.
	wait func(ctx context.Context, d time.Duration) error

	once       sync.Once
	calibrated time.Duration
	observed   atomic.Int64 // the slowest legacy verification seen, in ns
}

func newLegacyTimingFloor() *legacyTimingFloor {
	return &legacyTimingFloor{calibrate: measurePBKDF2Verification, now: time.Now, wait: waitContext}
}

// measurePBKDF2Verification times one PBKDF2-HMAC-SHA256 derivation at
// DefaultPasswordIterations, what verifying a legacy hash costs.
func measurePBKDF2Verification() time.Duration {
	salt := make([]byte, passwordSaltBytes)
	start := time.Now()
	_, _ = pbkdf2.Key(sha256.New, "forge-legacy-timing-floor", salt, DefaultPasswordIterations, passwordKeyBytes)
	return time.Since(start)
}

func waitContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// floor returns the current floor, calibrating it on first use: the slowest
// of legacyFloorCalibrations PBKDF2 verifications, raised by the slowest
// legacy verification observed since (within maxLegacyFloorFactor), plus
// legacyFloorMarginPercent. Calibration is not capped: a CPU flood during the
// first failure after start makes the floor, and so every failed check,
// slower until the process restarts. That costs latency only.
func (f *legacyTimingFloor) floor() time.Duration {
	f.once.Do(func() {
		for range legacyFloorCalibrations {
			f.calibrated = max(f.calibrated, f.calibrate())
		}
	})
	observed := min(time.Duration(f.observed.Load()), maxLegacyFloorFactor*f.calibrated)
	return max(f.calibrated, observed) * (100 + legacyFloorMarginPercent) / 100
}

// observe records how long one legacy (PBKDF2) verification took.
func (f *legacyTimingFloor) observe(d time.Duration) {
	for {
		seen := f.observed.Load()
		if int64(d) <= seen || f.observed.CompareAndSwap(seen, int64(d)) {
			return
		}
	}
}

// pad waits until the floor has passed since started, or ctx ends.
func (f *legacyTimingFloor) pad(ctx context.Context, started time.Time) error {
	remaining := f.floor() - f.now().Sub(started)
	if remaining <= 0 {
		return nil
	}
	return f.wait(ctx, remaining)
}

// WithLegacyHashTimingFloor turns the legacy-hash timing floor on or off.
// While it is on (the default when the hasher writes Argon2id), a failed
// password check of LoginService and of the step-up of auth.Browser (an
// unknown account or tenant, a malformed login, a wrong password against any
// hash) does not answer before one PBKDF2-600k verification would have
// finished (plus 25%, for its variation), so a wrong password against an
// account that still has a $pbkdf2-sha256$ hash takes no longer than any
// other failure. The failure is audited first; the padding is a timer that
// holds no PasswordWorkLimiter slot, burns no CPU and ends with the
// request's context. Successful checks are never padded. Turn it off once no
// $pbkdf2-sha256$ hash remains (see docs/AUTHENTICATION.md), and in an
// application created with this release or later, which never stored one; a
// hasher that writes PBKDF2 needs none, every check already costs PBKDF2.
func WithLegacyHashTimingFloor(enabled bool) PasswordHasherOption {
	return func(h *PasswordHasher) error {
		h.floorSet, h.floorEnabled = true, enabled
		return nil
	}
}

// padFailedCheck pads a failed password check that started at started up to
// the legacy timing floor, when the floor is on.
func (h *PasswordHasher) padFailedCheck(ctx context.Context, started time.Time) error {
	if h.floor == nil {
		return nil
	}
	return h.floor.pad(ctx, started)
}

// checkStarted is the time a password check starts, on the floor's clock.
func (h *PasswordHasher) checkStarted() time.Time {
	if h.floor == nil {
		return time.Time{}
	}
	return h.floor.now()
}
