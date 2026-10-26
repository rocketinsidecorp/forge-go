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
	"testing"

	"github.com/rocketinsidecorp/forge-go/csrf"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// Written by Forge-Rust's generated Argon2idPasswordHasher (argon2 0.6.0,
// Argon2id v19, m=19456, t=2, p=1, 32-byte output, a random 16-byte salt)
// for a test-only password.
const (
	forgeRustArgon2idFixture         = "$argon2id$v=19$m=19456,t=2,p=1$66DIwPvWLVzp/qkTyDQLqw$AMsFRwhoUEWdrArkL3sE7DfMOkuFGT8kuA6RIuNqZdU" // #nosec G101 -- the hash of a public test-only password
	forgeRustArgon2idFixturePassword = "forge-cross-framework-fixture"                                                                     // #nosec G101 -- a public test-only password
)

func TestArgon2idHashWrittenByForgeRustVerifies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hasher, err := NewPasswordHasher(WithPasswordAlgorithm(PasswordAlgorithmArgon2id))
	if err != nil {
		t.Fatal(err)
	}
	match, rehash, err := hasher.Verify(ctx, forgeRustArgon2idFixture, forgeRustArgon2idFixturePassword)
	if err != nil || !match || rehash {
		t.Fatalf("Forge-Rust hash: (%v, %v, %v), want a match in the current parameters", match, rehash, err)
	}
	if match, _, err := hasher.Verify(ctx, forgeRustArgon2idFixture, forgeRustArgon2idFixturePassword+"x"); err != nil || match {
		t.Fatalf("wrong password: match=%v err=%v", match, err)
	}
	// Forge-Go writes the same shape, so Forge-Rust reads it back.
	ours, err := hasher.Hash(ctx, forgeRustArgon2idFixturePassword)
	if err != nil {
		t.Fatal(err)
	}
	if len(ours) != len(forgeRustArgon2idFixture) || !strings.HasPrefix(ours, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("Forge-Go hash %q does not have Forge-Rust's shape", ours)
	}
}

// rehashStore is a PasswordStore holding one account whose hash
// ReplacePasswordHash really replaces.
type rehashStore struct {
	mu       sync.Mutex
	identity PasswordIdentity
	replaced []string
}

func (s *rehashStore) LookupPassword(_ context.Context, email, tenant string) (PasswordIdentity, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identity, email == "ada@example.com" && tenant == "acme", nil
}

func (s *rehashStore) ReplacePasswordHash(_ context.Context, subjectID uuid.UUID, previous, next string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if subjectID != s.identity.SubjectID || previous != s.identity.PasswordHash {
		return false, nil
	}
	s.identity.PasswordHash = next
	s.replaced = append(s.replaced, next)
	return true, nil
}

func newRehashStore(t *testing.T, hash string) *rehashStore {
	t.Helper()
	return &rehashStore{identity: PasswordIdentity{
		SubjectID: uuid.MustNew(), MembershipID: uuid.MustNew(), PasswordHash: hash, CredentialVersion: 1,
	}}
}

// An account stored before Argon2id signs in with its PBKDF2 hash; the
// sign-in stores the password as Argon2id, and the next one verifies that
// hash and changes nothing.
func TestLoginRehashesAPBKDF2HashToArgon2id(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	legacy, err := hashPassword("correct horse", minAcceptedPasswordIterations, bytes.NewReader(make([]byte, passwordSaltBytes)))
	if err != nil {
		t.Fatal(err)
	}
	store := newRehashStore(t, legacy)
	hasher := cheapPasswordHasher(t)
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewLoginService(store, &sessionStub{token: token}, WithLoginPasswordHasher(hasher))
	if err != nil {
		t.Fatal(err)
	}
	input := LoginInput{Email: "ada@example.com", Password: "correct horse", TenantSlug: "acme"}
	if _, err := service.Login(ctx, input); err != nil {
		t.Fatalf("first sign-in: %v", err)
	}
	if len(store.replaced) != 1 || !strings.HasPrefix(store.identity.PasswordHash, "$argon2id$v=19$m=8,t=1,p=1$") {
		t.Fatalf("stored hash %q after %d replacements, want Argon2id", store.identity.PasswordHash, len(store.replaced))
	}
	match, rehash, err := hasher.Verify(ctx, store.identity.PasswordHash, "correct horse")
	if err != nil || !match || rehash {
		t.Fatalf("rehashed credential: (%v, %v, %v)", match, rehash, err)
	}
	if _, err := service.Login(ctx, input); err != nil {
		t.Fatalf("second sign-in: %v", err)
	}
	if len(store.replaced) != 1 {
		t.Fatalf("a current hash was replaced again (%d replacements)", len(store.replaced))
	}
	// A wrong password never rewrites the credential.
	store = newRehashStore(t, legacy)
	service, err = NewLoginService(store, &sessionStub{token: token}, WithLoginPasswordHasher(hasher))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(ctx, LoginInput{Email: "ada@example.com", Password: "wrong", TenantSlug: "acme"}); !errors.Is(err, ErrCredentialsInvalid) {
		t.Fatalf("wrong password: %v", err)
	}
	if len(store.replaced) != 0 {
		t.Fatal("a wrong password rewrote the credential")
	}
	if _, err := NewLoginService(store, &sessionStub{}, WithLoginPasswordHasher(nil)); err == nil {
		t.Fatal("nil hasher accepted")
	}
}

// An Argon2id hash weaker than the current parameters is upgraded the same
// way, here to the production cost.
func TestLoginRehashesAWeakerArgon2idHash(t *testing.T) {
	t.Parallel()
	weak := argon2Hash("correct horse", testArgon2Params, bytes.Repeat([]byte{9}, passwordSaltBytes), passwordKeyBytes)
	store := newRehashStore(t, weak)
	production, err := NewPasswordHasher(WithPasswordAlgorithm(PasswordAlgorithmArgon2id))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewLoginService(store, &sessionStub{}, WithLoginPasswordHasher(production))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(context.Background(), LoginInput{Email: "ada@example.com", Password: "correct horse", TenantSlug: "acme"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(store.identity.PasswordHash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("stored hash %q, want the production parameters", store.identity.PasswordHash)
	}
}

// The dummy hash of an unknown account has the writer's current algorithm
// and cost, matches no password, and is really computed: with no free slot
// the miss fails like a known account instead of answering at once.
func TestLoginMissVerifiesADummyHashAtTheCurrentCost(t *testing.T) {
	t.Parallel()
	for _, hasher := range []*PasswordHasher{cheapPasswordHasher(t), DefaultPasswordHasher()} {
		stored, err := parseStoredPasswordHash(hasher.dummy)
		if err != nil || stored.algorithm != hasher.Algorithm() || hasher.needsRehash(stored) {
			t.Fatalf("dummy %q: %+v err=%v", hasher.dummy, stored, err)
		}
		if match, _, err := hasher.Verify(context.Background(), hasher.dummy, "any password"); err != nil || match {
			t.Fatalf("dummy matched: %v %v", match, err)
		}
	}
	if DefaultPasswordAlgorithm == PasswordAlgorithmArgon2id && !strings.HasPrefix(DefaultPasswordHasher().dummy, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("default dummy %q is not at the production Argon2id cost", DefaultPasswordHasher().dummy)
	}

	limiter, err := NewPasswordWorkLimiter(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	hasher := cheapPasswordHasher(t, WithPasswordWorkLimiter(limiter))
	service, err := NewLoginService(newRehashStore(t, hasher.dummy), &sessionStub{}, WithLoginPasswordHasher(hasher))
	if err != nil {
		t.Fatal(err)
	}
	release := holdSlot(t, limiter)
	defer release()
	for name, input := range map[string]LoginInput{
		"unknown account":  {Email: "nobody@example.com", Password: "secret", TenantSlug: "acme"},
		"unknown tenant":   {Email: "ada@example.com", Password: "secret", TenantSlug: "other"},
		"malformed tenant": {Email: "ada@example.com", Password: "secret", TenantSlug: "-"},
		"malformed login":  {Email: "not a login", Password: "secret", TenantSlug: "acme"},
	} {
		if _, err := service.Login(context.Background(), input); !errors.Is(err, ErrPasswordHashingUnavailable) {
			t.Errorf("%s: %v, want the dummy hash to need a slot", name, err)
		}
	}
}

// A saturated limiter answers the JSON and browser sign-ins 503 with
// Retry-After, for a known and an unknown account alike, never 401/422, so a
// full limiter tells nothing about the credentials.
func TestSaturatedPasswordHashingAnswers503WithRetryAfter(t *testing.T) {
	t.Parallel()
	limiter, err := NewPasswordWorkLimiter(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	hasher := cheapPasswordHasher(t, WithPasswordWorkLimiter(limiter))
	known, err := cheapPasswordHasher(t).Hash(context.Background(), "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewLoginService(newRehashStore(t, known), &sessionStub{}, WithLoginPasswordHasher(hasher))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLoginHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	browser, err := NewBrowser(BrowserOptions{Login: service, Sessions: &revokeStub{}})
	if err != nil {
		t.Fatal(err)
	}
	app := browserApp(t, browser, csrf.Options{})
	release := holdSlot(t, limiter)
	defer release()

	for _, email := range []string{"ada@example.com", "nobody@example.com"} {
		body := `{"email":"` + email + `","password":"correct horse","tenant":"acme"}`
		request := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "5" ||
			!strings.Contains(recorder.Body.String(), "authentication_unavailable") {
			t.Errorf("JSON sign-in of %s: status=%d retry-after=%q body=%s", email, recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
		}

		form := url.Values{"email": {email}, "password": {"correct horse"}, "tenant": {"acme"}}
		recorder = submit(app, "/session", form, true)
		if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "5" {
			t.Errorf("browser sign-in of %s: status=%d retry-after=%q", email, recorder.Code, recorder.Header().Get("Retry-After"))
		}
	}
}

// A reset writes the new password with the recovery service's hasher, so it
// is Argon2id, and a saturated limiter answers 503 with Retry-After.
func TestPasswordResetHashesThroughTheRecoveryHasher(t *testing.T) {
	t.Parallel()
	token, err := NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := NewPasswordWorkLimiter(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	store := &recoveryStoreStub{consumeResult: true}
	service, err := NewRecoveryService(store, &recoverySenderStub{}, WithRecoveryPasswordHasher(cheapPasswordHasher(t, WithPasswordWorkLimiter(limiter))))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewPasswordResetHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	reset := func() *httptest.ResponseRecorder {
		body := `{"token":"` + token.Reveal() + `","new_password":"replacement password"}`
		request := httptest.NewRequest(http.MethodPost, "/auth/reset-password", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	release := holdSlot(t, limiter)
	if recorder := reset(); recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "5" || store.consumedHash != "" {
		t.Fatalf("saturated reset: status=%d retry-after=%q consumed=%q", recorder.Code, recorder.Header().Get("Retry-After"), store.consumedHash)
	}
	release()
	if recorder := reset(); recorder.Code != http.StatusNoContent || !strings.HasPrefix(store.consumedHash, "$argon2id$v=19$") {
		t.Fatalf("reset: status=%d hash=%q", recorder.Code, store.consumedHash)
	}
	if _, err := NewRecoveryService(store, &recoverySenderStub{}, WithRecoveryPasswordHasher(nil)); err == nil {
		t.Fatal("nil hasher accepted")
	}
}
