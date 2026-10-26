package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rocketinsidecorp/forge-go/csrf"
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

		// The step-up of auth.Browser is padded the same way.
		accounts := &memoryMFAAccounts{accounts: map[uuid.UUID]*mfaAccount{}}
		subject := uuid.MustNew()
		accounts.accounts[subject] = &mfaAccount{login: "ada@example.com", hash: legacy}
		for name, check := range map[string]struct {
			subject  uuid.UUID
			password string
		}{"unknown subject": {uuid.MustNew(), "correct horse"}, "wrong password": {subject, "wrong"}} {
			start := time.Now()
			if _, ok, err := verifySubjectPassword(context.Background(), hasher, accounts, check.subject, check.password); ok || err != nil || time.Since(start) != testLegacyFloor {
				t.Errorf("step-up %s: ok=%v err=%v took %v", name, ok, err, time.Since(start))
			}
		}
	})
}

// The padding is a timer that ends with the request: a cancelled sign-in
// stops waiting at once, and so does a step-up.
func TestLegacyTimingFloorStopsWhenTheContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hasher := floorHasher(t)
		service, err := NewLoginService(newAccountsStore(t, nil), &sessionStub{}, WithLoginPasswordHasher(hasher))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			// fake-clock: cancels the request 10 ms of bubble time into the padding
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()
		start := time.Now()
		_, err = service.Login(ctx, LoginInput{Email: "nobody@example.com", Password: "secret", TenantSlug: "acme"})
		if !errors.Is(err, context.Canceled) || time.Since(start) != 10*time.Millisecond {
			t.Fatalf("cancelled sign-in: err=%v after %v", err, time.Since(start))
		}

		ctx, cancel = context.WithCancel(context.Background())
		cancel()
		accounts := &memoryMFAAccounts{accounts: map[uuid.UUID]*mfaAccount{}}
		if _, ok, err := verifySubjectPassword(ctx, hasher, accounts, uuid.MustNew(), "secret"); ok || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled step-up: ok=%v err=%v", ok, err)
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

// The cookie login, the browser reset form and the browser step-up answer a
// saturated limiter 503 with Retry-After too.
func TestSaturatedHashingAnswers503OnCookieLoginResetAndStepUp(t *testing.T) {
	t.Parallel()
	limiter, err := NewPasswordWorkLimiter(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	saturated := cheapPasswordHasher(t, WithPasswordWorkLimiter(limiter))
	login := browserLogin(t, knownIdentity(t, mfaTestPassword, false), &sessionStub{}, WithLoginPasswordHasher(saturated))
	cookieLogin, err := NewCookieLoginHandler(login)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := NewRecoveryService(&recoveryStoreStub{consumeResult: true}, &recoverySenderStub{}, WithRecoveryPasswordHasher(saturated))
	if err != nil {
		t.Fatal(err)
	}
	resetBrowser, err := NewBrowser(BrowserOptions{Login: login, Sessions: &revokeStub{}, Recovery: recovery})
	if err != nil {
		t.Fatal(err)
	}
	resetApp := browserApp(t, resetBrowser, csrf.Options{})
	settings := newMFASettingsFixture(t, DefaultMFAThrottleConfig())
	stepUpBrowser, err := NewBrowser(BrowserOptions{Login: login, Sessions: &browserRevoker{}, MFA: settings.mfa, Passwords: settings.accounts})
	if err != nil {
		t.Fatal(err)
	}
	settings.app = mfaSettingsApp(t, stepUpBrowser)
	token, err := NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}

	release := holdSlot(t, limiter)
	defer release()
	request := httptest.NewRequest(http.MethodPost, "https://app.example.com/auth/login",
		strings.NewReader(`{"email":"ada@example.com","password":"`+mfaTestPassword+`","tenant":"acme"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://app.example.com")
	cookie := httptest.NewRecorder()
	cookieLogin.ServeHTTP(cookie, request)
	reset := submit(resetApp, "/passwords", url.Values{"_method": {"PATCH"}, "token": {token.Reveal()}, "password": {"replacement password"}}, true)
	stepUp := settings.post(settings.ada, "/mfa", url.Values{"password": {mfaTestPassword}}, true)
	for name, recorder := range map[string]*httptest.ResponseRecorder{"cookie login": cookie, "browser reset": reset, "browser step-up": stepUp} {
		if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "5" {
			t.Errorf("%s: status=%d retry-after=%q body=%s", name, recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
		}
	}
}
