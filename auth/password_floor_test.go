package auth

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

// testPBKDF2Cost stands for one PBKDF2-600k verification in the floor
// tests, and testLegacyFloor is the floor it gives. Inside a synctest bubble
// hashing takes no time, so every padded failure takes exactly the floor and
// every unpadded answer none.
const (
	testPBKDF2Cost  = 120 * time.Millisecond
	testLegacyFloor = testPBKDF2Cost * (100 + legacyFloorMarginPercent) / 100
)

// floorHasher is a cheap Argon2id hasher with the legacy timing floor on,
// calibrated to testLegacyFloor. Built inside a bubble, its limiter and
// timers are the bubble's.
func floorHasher(t *testing.T, options ...PasswordHasherOption) *PasswordHasher {
	t.Helper()
	limiter, err := NewPasswordWorkLimiter(4, 0)
	if err != nil {
		t.Fatal(err)
	}
	hasher := cheapPasswordHasher(t, append([]PasswordHasherOption{WithPasswordWorkLimiter(limiter), WithLegacyHashTimingFloor(true)}, options...)...)
	hasher.floor.calibrate = func() time.Duration { return testPBKDF2Cost }
	return hasher
}

// accountsStore holds accounts by email in tenant acme.
type accountsStore struct {
	mu       sync.Mutex
	accounts map[string]*PasswordIdentity
	replaced int
}

func (s *accountsStore) LookupPassword(_ context.Context, email, tenant string) (PasswordIdentity, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	identity, ok := s.accounts[email]
	if !ok || tenant != "acme" {
		return PasswordIdentity{}, false, nil
	}
	return *identity, true, nil
}

func (s *accountsStore) ReplacePasswordHash(_ context.Context, subjectID uuid.UUID, previous, next string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, identity := range s.accounts {
		if identity.SubjectID == subjectID && identity.PasswordHash == previous {
			identity.PasswordHash = next
			s.replaced++
			return true, nil
		}
	}
	return false, nil
}

func newAccountsStore(t *testing.T, hashes map[string]string) *accountsStore {
	t.Helper()
	store := &accountsStore{accounts: map[string]*PasswordIdentity{}}
	for email, hash := range hashes {
		store.accounts[email] = &PasswordIdentity{SubjectID: uuid.MustNew(), MembershipID: uuid.MustNew(), PasswordHash: hash, CredentialVersion: 1}
	}
	return store
}

func legacyPasswordHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := hashPassword(password, minAcceptedPasswordIterations, bytes.NewReader(make([]byte, passwordSaltBytes)))
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

// Every failed check, whatever the account and its hash, waits for the
// floor; a successful one is not padded.
func TestFailedChecksWaitForTheLegacyTimingFloor(t *testing.T) {
	legacy := legacyPasswordHash(t, "correct horse")
	synctest.Test(t, func(t *testing.T) {
		hasher := floorHasher(t)
		migrated, err := hasher.Hash(context.Background(), "correct horse")
		if err != nil {
			t.Fatal(err)
		}
		store := newAccountsStore(t, map[string]string{"legacy@example.com": legacy, "migrated@example.com": migrated})
		service, err := NewLoginService(store, &sessionStub{}, WithLoginPasswordHasher(hasher))
		if err != nil {
			t.Fatal(err)
		}
		timed := func(input LoginInput) (time.Duration, error) {
			start := time.Now()
			_, err := service.Login(context.Background(), input)
			return time.Since(start), err
		}
		for name, input := range map[string]LoginInput{
			"unknown account":                 {Email: "nobody@example.com", Password: "correct horse", TenantSlug: "acme"},
			"unknown tenant":                  {Email: "migrated@example.com", Password: "correct horse", TenantSlug: "other"},
			"malformed tenant":                {Email: "migrated@example.com", Password: "correct horse", TenantSlug: "-"},
			"malformed login":                 {Email: "not a login", Password: "correct horse", TenantSlug: "acme"},
			"migrated account wrong password": {Email: "migrated@example.com", Password: "wrong", TenantSlug: "acme"},
			"legacy account wrong password":   {Email: "legacy@example.com", Password: "wrong", TenantSlug: "acme"},
		} {
			if took, err := timed(input); !errors.Is(err, ErrCredentialsInvalid) || took != testLegacyFloor {
				t.Errorf("%s: took %v err=%v, want ErrCredentialsInvalid after exactly %v", name, took, err, testLegacyFloor)
			}
		}
		for _, email := range []string{"migrated@example.com", "legacy@example.com"} {
			if took, err := timed(LoginInput{Email: email, Password: "correct horse", TenantSlug: "acme"}); err != nil || took != 0 {
				t.Errorf("successful sign-in of %s: took %v err=%v, want no padding", email, took, err)
			}
		}
		if store.replaced != 1 {
			t.Errorf("legacy account rehashed %d times", store.replaced)
		}

		// The step-up of the two-factor settings (MFAStepUpGate, behind
		// authhttp.Browser) is padded the same way.
		f := floorStepUp(t, hasher)
		for name, subject := range map[string]uuid.UUID{"unknown subject": uuid.MustNew(), "wrong password": f.ada} {
			start := time.Now()
			if err := f.enroll(context.Background(), subject, "wrong"); !errors.Is(err, ErrMFAStepUpInvalid) || time.Since(start) != testLegacyFloor {
				t.Errorf("step-up %s: err=%v took %v", name, err, time.Since(start))
			}
		}
		if f.enroll(context.Background(), f.ada, mfaTestPassword) != nil {
			t.Error("the right password was refused")
		}
		if denied := f.deniedEvents(SecurityMFAEnrollmentStarted); denied != 2 {
			t.Errorf("%d step-up refusals audited, want 2", denied)
		}
	})
}

// stepUpFloor is an MFAStepUpGate whose login service hashes with hasher,
// over one account (ada) with mfaTestPassword, recording audit events.
type stepUpFloor struct {
	gate   *MFAStepUpGate
	ada    uuid.UUID
	mu     sync.Mutex
	events []SecurityEvent
}

func floorStepUp(t *testing.T, hasher *PasswordHasher) *stepUpFloor {
	t.Helper()
	hash, err := cheapPasswordHash(mfaTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	f := &stepUpFloor{ada: uuid.MustNew()}
	accounts := &memoryMFAAccounts{accounts: map[uuid.UUID]*mfaAccount{f.ada: {login: "ada@example.com", hash: hash}}}
	cipher, err := NewAESGCMSecretCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	auditor := SecurityAuditorFunc(func(_ context.Context, event SecurityEvent) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.events = append(f.events, event)
		return nil
	})
	throttler, err := NewMemoryLoginThrottler(DefaultMFAThrottleConfig())
	if err != nil {
		t.Fatal(err)
	}
	mfa, err := NewMFAService(accounts, &sessionStub{}, cipher, WithMFAAuditor(auditor), WithMFAThrottler(throttler))
	if err != nil {
		t.Fatal(err)
	}
	login, err := NewLoginService(newAccountsStore(t, nil), &sessionStub{}, WithLoginPasswordHasher(hasher))
	if err != nil {
		t.Fatal(err)
	}
	if f.gate, err = NewMFAStepUpGate(login, mfa, accounts); err != nil {
		t.Fatal(err)
	}
	return f
}

// enroll is the step-up of turning two-factor authentication on (no code)
// with password.
func (f *stepUpFloor) enroll(ctx context.Context, subject uuid.UUID, password string) error {
	_, err := f.gate.Check(ctx, subject, "192.0.2.1", SecurityMFAEnrollmentStarted, MFAStepUp{Password: password}, false, nil)
	return err
}

func (f *stepUpFloor) deniedEvents(kind SecurityEventKind) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, event := range f.events {
		if event.Kind == kind && event.Outcome == SecurityOutcomeDenied {
			count++
		}
	}
	return count
}

// The padding is a timer that ends with the request: a client that hangs up
// during it stops the wait, and the failure it learned of is still audited,
// for a sign-in and for a step-up.
func TestLegacyTimingFloorStopsWhenTheContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// cancelAfter cancels the returned context d of bubble time from now.
		cancelAfter := func(d time.Duration) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				// fake-clock: cancels the request part way into the padding, in bubble time
				time.Sleep(d)
				cancel()
			}()
			return ctx
		}
		hasher := floorHasher(t)
		auditor := &securityAuditorStub{}
		service, err := NewLoginService(newAccountsStore(t, map[string]string{"ada@example.com": legacyPasswordHash(t, "correct horse")}), &sessionStub{},
			WithLoginPasswordHasher(hasher), WithSecurityAuditor(auditor))
		if err != nil {
			t.Fatal(err)
		}
		for name, email := range map[string]string{"unknown account": "nobody@example.com", "wrong password": "ada@example.com"} {
			auditor.events = nil
			start := time.Now()
			_, err = service.Login(cancelAfter(10*time.Millisecond), LoginInput{Email: email, Password: "wrong", TenantSlug: "acme"})
			if !errors.Is(err, context.Canceled) || time.Since(start) != 10*time.Millisecond {
				t.Errorf("cancelled sign-in, %s: err=%v after %v", name, err, time.Since(start))
			}
			if len(auditor.events) != 1 || auditor.events[0].Kind != SecurityLoginFailed || auditor.events[0].Outcome != SecurityOutcomeDenied {
				t.Errorf("cancelled sign-in, %s: audit %+v, want one denied sign-in", name, auditor.events)
			}
		}

		f := floorStepUp(t, hasher)
		start := time.Now()
		if err := f.enroll(cancelAfter(10*time.Millisecond), f.ada, "wrong"); !errors.Is(err, context.Canceled) || time.Since(start) != 10*time.Millisecond {
			t.Errorf("cancelled step-up: err=%v after %v", err, time.Since(start))
		}
		if denied := f.deniedEvents(SecurityMFAEnrollmentStarted); denied != 1 {
			t.Errorf("cancelled step-up: %d refusals audited, want 1", denied)
		}
	})
}

func TestLegacyTimingFloorCalibrationAndDefaults(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]struct {
		options []PasswordHasherOption
		on      bool
	}{
		"Argon2id default":    {[]PasswordHasherOption{WithPasswordAlgorithm(PasswordAlgorithmArgon2id)}, true},
		"Argon2id turned off": {[]PasswordHasherOption{WithPasswordAlgorithm(PasswordAlgorithmArgon2id), WithLegacyHashTimingFloor(false)}, false},
		"PBKDF2 default":      {[]PasswordHasherOption{WithPasswordAlgorithm(PasswordAlgorithmPBKDF2SHA256)}, false},
		"PBKDF2 turned on":    {[]PasswordHasherOption{WithPasswordAlgorithm(PasswordAlgorithmPBKDF2SHA256), WithLegacyHashTimingFloor(true)}, true},
	} {
		hasher, err := NewPasswordHasher(want.options...)
		if err != nil {
			t.Fatal(err)
		}
		if (hasher.floor != nil) != want.on {
			t.Errorf("%s: floor on=%v", name, hasher.floor != nil)
		}
	}
	if fresh, err := NewPasswordHasher(); err != nil || (fresh.floor != nil) != (DefaultPasswordAlgorithm == PasswordAlgorithmArgon2id) {
		t.Fatalf("NewPasswordHasher() floor=%v err=%v", fresh.floor, err)
	}

	// The slowest calibration counts, then the slowest legacy verification
	// observed, within maxLegacyFloorFactor.
	hasher := cheapPasswordHasher(t, WithLegacyHashTimingFloor(true))
	var calls atomic.Int64
	hasher.floor.calibrate = func() time.Duration {
		return []time.Duration{80 * time.Millisecond, 100 * time.Millisecond}[calls.Add(1)-1]
	}
	withMargin := func(d time.Duration) time.Duration { return d * (100 + legacyFloorMarginPercent) / 100 }
	if got := hasher.floor.floor(); got != withMargin(100*time.Millisecond) || calls.Load() != legacyFloorCalibrations {
		t.Fatalf("floor=%v after %d calibrations", got, calls.Load())
	}
	clock := time.Unix(0, 0)
	hasher.floor.now = func() time.Time {
		clock = clock.Add(150 * time.Millisecond)
		return clock
	}
	if match, _, err := hasher.Verify(context.Background(), legacyPasswordHash(t, "secret"), "secret"); err != nil || !match {
		t.Fatalf("legacy verify: %v %v", match, err)
	}
	if got := hasher.floor.floor(); got != withMargin(150*time.Millisecond) || calls.Load() != legacyFloorCalibrations {
		t.Fatalf("floor after a 150 ms legacy verification = %v (calibrated again: %v)", got, calls.Load() != legacyFloorCalibrations)
	}
	// A check that already took longer than the floor waits no more.
	if err := hasher.floor.pad(context.Background(), time.Unix(0, 0)); err != nil {
		t.Fatalf("pad after the floor: %v", err)
	}
	hasher.floor.observe(time.Hour)
	if got := hasher.floor.floor(); got != withMargin(maxLegacyFloorFactor*100*time.Millisecond) {
		t.Fatalf("an outlier raised the floor to %v", got)
	}
	// The real measurement times a PBKDF2 derivation.
	if measured := measurePBKDF2Verification(); measured <= 0 {
		t.Fatalf("measured %v", measured)
	}
}

// A rehash refused because hashing is saturated is skipped: the right
// password still signs in and the legacy hash stays for the next time.
func TestLoginSkipsARehashRefusedBySaturatedHashing(t *testing.T) {
	t.Parallel()
	limiter, err := NewPasswordWorkLimiter(4, 0)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	limiter.refuse = func() bool { return calls.Add(1) == 2 } // the verification passes, the rehash is refused
	legacy := legacyPasswordHash(t, "correct horse")
	store := newAccountsStore(t, map[string]string{"ada@example.com": legacy})
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewLoginService(store, &sessionStub{token: token}, WithLoginPasswordHasher(cheapPasswordHasher(t, WithPasswordWorkLimiter(limiter))))
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Login(context.Background(), LoginInput{Email: "ada@example.com", Password: "correct horse", TenantSlug: "acme"})
	if err != nil || result.Token.Reveal() != token.Reveal() {
		t.Fatalf("sign-in with a refused rehash: %+v err=%v", result, err)
	}
	if store.replaced != 0 || store.accounts["ada@example.com"].PasswordHash != legacy || calls.Load() != 2 {
		t.Fatalf("replaced=%d calls=%d", store.replaced, calls.Load())
	}

	// Any other rehash failure still fails the sign-in, and a corrupt stored
	// hash fails a step-up, unpadded.
	broken := cheapPasswordHasher(t)
	broken.entropy = bytes.NewReader(nil)
	service, err = NewLoginService(store, &sessionStub{token: token}, WithLoginPasswordHasher(broken))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(context.Background(), LoginInput{Email: "ada@example.com", Password: "correct horse", TenantSlug: "acme"}); err == nil || errors.Is(err, ErrCredentialsInvalid) {
		t.Fatalf("sign-in with a failing rehash: %v", err)
	}
	subject := uuid.MustNew()
	accounts := &memoryMFAAccounts{accounts: map[uuid.UUID]*mfaAccount{subject: {login: "ada@example.com", hash: "$argon2id$corrupt"}}}
	if _, ok, err := verifySubjectPassword(context.Background(), broken, accounts, subject, "correct horse"); ok || !errors.Is(err, ErrPasswordHashInvalid) {
		t.Fatalf("corrupt step-up hash: ok=%v err=%v", ok, err)
	}
	refusing, err := NewPasswordWorkLimiter(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	refusing.refuse = func() bool { return true }
	if _, ok, err := verifySubjectPassword(context.Background(), cheapPasswordHasher(t, WithPasswordWorkLimiter(refusing)), accounts, uuid.MustNew(), "correct horse"); ok || !errors.Is(err, ErrPasswordHashingUnavailable) {
		t.Fatalf("unknown subject, saturated: ok=%v err=%v", ok, err)
	}
}
