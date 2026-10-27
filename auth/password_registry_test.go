package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/rocketinsidecorp/forge-go/fault"
)

// The first registration wins: a later one, or a nil function, changes
// nothing, so a second identity adapter (or anything else) cannot swap the
// implementation once hashes depend on it.
func TestRegisterArgon2idKeepsTheFirstImplementation(t *testing.T) {
	t.Parallel()
	var registry argon2Registry
	if registry.load() != nil || registry.defaultAlgorithm() != PasswordAlgorithmPBKDF2SHA256 {
		t.Fatal("an empty registry has an implementation")
	}
	if registry.register(nil) || registry.load() != nil {
		t.Fatal("a nil implementation was registered")
	}
	first := func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
		return bytes.Repeat([]byte{1}, int(keyLen))
	}
	second := func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
		return bytes.Repeat([]byte{2}, int(keyLen))
	}
	if !registry.register(first) {
		t.Fatal("the first registration was refused")
	}
	if registry.register(second) || registry.register(nil) {
		t.Fatal("a later registration was kept")
	}
	if key := registry.load()(nil, nil, 1, 8, 1, 4); !bytes.Equal(key, []byte{1, 1, 1, 1}) {
		t.Fatalf("registered implementation returned %v, want the first one's", key)
	}
	if !pbkdf2Build && registry.defaultAlgorithm() != PasswordAlgorithmArgon2id {
		t.Fatalf("default algorithm with Argon2id registered: %q", registry.defaultAlgorithm())
	}

	// The process-wide registry already holds the fake (password_cheap_test.go):
	// RegisterArgon2id with another Argon2id (here one that knows the
	// checked answer and nothing else) is ignored, and so is nil.
	other := func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
		if bytes.Equal(password, argon2idKnownPassword) {
			return fakeArgon2idKDF(password, salt, time, memory, threads, keyLen)
		}
		return make([]byte, keyLen)
	}
	if RegisterArgon2id(other) || RegisterArgon2id(nil) {
		t.Fatal("a later or nil registration reported that it took effect")
	}
	salt := bytes.Repeat([]byte{9}, passwordSaltBytes)
	if got, want := argon2id.load()([]byte("secret"), salt, 1, 8, 1, passwordKeyBytes), fakeArgon2idKDF([]byte("secret"), salt, 1, 8, 1, passwordKeyBytes); !bytes.Equal(got, want) {
		t.Fatal("a second RegisterArgon2id replaced the first implementation")
	}
}

// RegisterArgon2id refuses, loudly, a function that does not compute
// Argon2id: argon2.Key (Argon2i) has the same signature, and hashes it wrote
// as $argon2id$ would verify nowhere else.
func TestRegisterArgon2idPanicsOnAFunctionThatIsNotArgon2id(t *testing.T) {
	t.Parallel()
	for name, kdf := range map[string]argon2idKDF{
		"zeros": func(_, _ []byte, _, _ uint32, _ uint8, keyLen uint32) []byte { return make([]byte, keyLen) },
		"short": func(_, _ []byte, _, _ uint32, _ uint8, _ uint32) []byte { return argon2idKnownAnswers[0].key[:16] },
		// Right for the first answer's parameters, which it always uses:
		// hashes it wrote would claim a cost they did not pay.
		"cost clamped to the first answer's": func(password, salt []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
			first := argon2idKnownAnswers[0]
			return fakeArgon2idKDF(password, salt, first.time, first.memory, first.threads, keyLen)
		},
		"key length ignored": func(password, salt []byte, time, memory uint32, threads uint8, _ uint32) []byte {
			return fakeArgon2idKDF(password, salt, time, memory, threads, 32)
		},
		"fake without the known answer": func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
			return fakeArgon2idKDF(append(password, 0), salt, time, memory, threads, keyLen)
		},
	} {
		func() {
			defer func() {
				if recovered := recover(); recovered == nil || !strings.Contains(fmt.Sprint(recovered), "argon2.IDKey") {
					t.Errorf("%s: RegisterArgon2id did not panic naming the fix: %v", name, recovered)
				}
			}()
			RegisterArgon2id(kdf)
		}()
	}
	salt := bytes.Repeat([]byte{9}, passwordSaltBytes)
	if got := argon2id.load()([]byte("x"), salt, 1, 8, 1, 32); !bytes.Equal(got, fakeArgon2idKDF([]byte("x"), salt, 1, 8, 1, 32)) {
		t.Fatal("a refused function was registered")
	}
}

// With nothing registered, a stored Argon2id hash fails closed with
// ErrPasswordAlgorithmUnavailable: not a mismatch, no rehash, decided before
// any hashing (the limiter here has no free slot), and a 503 for HTTP, not a
// retryable saturation. New hashes are PBKDF2 with the standard parameters.
func TestArgon2idWithNothingRegisteredFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stored := argon2Hash("secret", defaultArgon2Params, bytes.Repeat([]byte{4}, passwordSaltBytes), passwordKeyBytes)
	limiter, err := NewPasswordWorkLimiter(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	release := holdSlot(t, limiter)
	for name, hasher := range map[string]*PasswordHasher{
		"default":  unregisteredHasher(t, WithPasswordWorkLimiter(limiter)),
		"Argon2id": unregisteredHasher(t, WithPasswordWorkLimiter(limiter), WithPasswordAlgorithm(PasswordAlgorithmArgon2id)),
		"PBKDF2":   unregisteredHasher(t, WithPasswordWorkLimiter(limiter), WithPasswordAlgorithm(PasswordAlgorithmPBKDF2SHA256)),
	} {
		for _, password := range []string{"secret", "wrong"} {
			match, rehash, err := hasher.Verify(ctx, stored, password)
			if match || rehash || !errors.Is(err, ErrPasswordAlgorithmUnavailable) {
				t.Errorf("%s, %s: Verify = (%v, %v, %v), want ErrPasswordAlgorithmUnavailable", name, password, match, rehash, err)
			}
		}
	}
	release()

	if errors.Is(ErrPasswordAlgorithmUnavailable, ErrPasswordHashingUnavailable) || errors.Is(ErrPasswordHashingUnavailable, ErrPasswordAlgorithmUnavailable) {
		t.Fatal("an unavailable algorithm must not pass for saturated hashing (no Retry-After, no skipped rehash)")
	}
	public := fault.From(ErrPasswordAlgorithmUnavailable)
	if public == nil || public.Status() != http.StatusServiceUnavailable || public.Retryable ||
		!strings.Contains(public.Cause.Error(), "RegisterArgon2id") || strings.Contains(public.Message, "argon2") {
		t.Fatalf("ErrPasswordAlgorithmUnavailable = %+v, want a 503 whose logged cause names the fix and whose message does not", public)
	}

	// New hashes: the default falls back to PBKDF2-600k; an explicit
	// Argon2id hasher refuses rather than writing something else.
	hasher := unregisteredHasher(t)
	if hasher.Algorithm() != PasswordAlgorithmPBKDF2SHA256 || hasher.activeFloor() != nil {
		t.Fatalf("algorithm %q, floor %v", hasher.Algorithm(), hasher.activeFloor())
	}
	hash, err := hasher.Hash(ctx, "secret")
	if err != nil || !strings.HasPrefix(hash, "$pbkdf2-sha256$v=1$i=600000$") {
		t.Fatalf("Hash = %q, %v; want PBKDF2 with the default work factor", hash, err)
	}
	if match, rehash, err := hasher.Verify(ctx, hash, "secret"); err != nil || !match || rehash {
		t.Fatalf("fallback hash: Verify = (%v, %v, %v)", match, rehash, err)
	}
	if err := hasher.verifyDummy(ctx, "secret"); err != nil {
		t.Fatalf("the dummy of an unknown account must be PBKDF2 and verifiable: %v", err)
	}
	strict := unregisteredHasher(t, WithPasswordAlgorithm(PasswordAlgorithmArgon2id))
	if hash, err := strict.Hash(ctx, "secret"); hash != "" || !errors.Is(err, ErrPasswordAlgorithmUnavailable) {
		t.Fatalf("Argon2id hasher without an implementation: Hash = %q, %v", hash, err)
	}
	if err := strict.verifyDummy(ctx, "secret"); !errors.Is(err, ErrPasswordAlgorithmUnavailable) {
		t.Fatalf("Argon2id hasher without an implementation: dummy = %v", err)
	}
}

// A sign-in against an Argon2id hash while nothing is registered fails with
// ErrPasswordAlgorithmUnavailable: not ErrCredentialsInvalid, no failure
// audited as a wrong password, and the stored hash is never rewritten.
func TestLoginWithAnArgon2idHashAndNothingRegisteredIsUnavailable(t *testing.T) {
	t.Parallel()
	stored := argon2Hash("correct horse battery staple", defaultArgon2Params, bytes.Repeat([]byte{5}, passwordSaltBytes), passwordKeyBytes)
	store := newRehashStore(t, stored)
	auditor := &securityAuditorStub{}
	service, err := NewLoginService(store, &sessionStub{}, WithLoginPasswordHasher(unregisteredHasher(t)), WithSecurityAuditor(auditor))
	if err != nil {
		t.Fatal(err)
	}
	for _, password := range []string{"correct horse battery staple", "wrong password"} {
		result, err := service.Login(context.Background(), LoginInput{Email: "ada@example.com", Password: password, TenantSlug: "acme"})
		if !errors.Is(err, ErrPasswordAlgorithmUnavailable) || errors.Is(err, ErrCredentialsInvalid) || !result.ExpiresAt.IsZero() {
			t.Fatalf("%q: Login = %+v, %v; want ErrPasswordAlgorithmUnavailable", password, result, err)
		}
	}
	if len(store.replaced) != 0 || store.identity.PasswordHash != stored {
		t.Fatalf("the Argon2id hash was rewritten: %v", store.replaced)
	}
	// Each attempt is audited as a login that failed (it could not be
	// checked), never as denied (a wrong password) or succeeded.
	if len(auditor.events) != 2 {
		t.Fatalf("audited %+v, want two events", auditor.events)
	}
	for _, event := range auditor.events {
		if event.Kind != SecurityLoginFailed || event.Outcome != SecurityOutcomeFailed || event.SubjectID != store.identity.SubjectID {
			t.Fatalf("audited %+v, want auth.login.failed with the outcome failed for the account", event)
		}
	}
}

// A hasher set to Argon2id with nothing registered still verifies PBKDF2
// hashes. The right password signs in and the hash is left as it is (the
// rehash cannot run); answering it 503 only after the password matched
// would tell a right password from a wrong one, which gets the usual
// ErrCredentialsInvalid.
func TestLoginSkipsTheRehashWhenArgon2idIsNotRegistered(t *testing.T) {
	t.Parallel()
	legacy, err := hashPassword("correct horse battery staple", minAcceptedPasswordIterations, bytes.NewReader(make([]byte, passwordSaltBytes)))
	if err != nil {
		t.Fatal(err)
	}
	store := newRehashStore(t, legacy)
	hasher := unregisteredHasher(t, WithPasswordAlgorithm(PasswordAlgorithmArgon2id), WithLegacyHashTimingFloor(false))
	sessions := &sessionStub{}
	service, err := NewLoginService(store, sessions, WithLoginPasswordHasher(hasher))
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Login(context.Background(), LoginInput{Email: "ada@example.com", Password: "correct horse battery staple", TenantSlug: "acme"})
	if err != nil || result.ExpiresAt.IsZero() || sessions.membership != store.identity.MembershipID {
		t.Fatalf("right password: %+v, %v; want a session for the account", result, err)
	}
	if len(store.replaced) != 0 || store.identity.PasswordHash != legacy {
		t.Fatalf("the PBKDF2 hash was rewritten: %v", store.replaced)
	}
	if _, err := service.Login(context.Background(), LoginInput{Email: "ada@example.com", Password: "wrong password", TenantSlug: "acme"}); !errors.Is(err, ErrCredentialsInvalid) {
		t.Fatalf("wrong password: %v, want ErrCredentialsInvalid", err)
	}
}

// The MFA step-up against an Argon2id hash with nothing registered fails
// with ErrPasswordAlgorithmUnavailable and is audited as failed, not denied.
func TestMFAStepUpWithNothingRegisteredIsAuditedAsFailed(t *testing.T) {
	t.Parallel()
	const action = SecurityMFAEnrollmentStarted
	f := newFaultMFA(t)
	login, err := NewLoginService(newAccountsStore(t, nil), &sessionStub{}, WithLoginPasswordHasher(unregisteredHasher(t)))
	if err != nil {
		t.Fatal(err)
	}
	gate, err := NewMFAStepUpGate(login, f.service, f.store)
	if err != nil {
		t.Fatal(err)
	}
	_, err = gate.Check(context.Background(), f.subject, "192.0.2.1", action, MFAStepUp{Password: mfaTestPassword}, false, nil)
	if !errors.Is(err, ErrPasswordAlgorithmUnavailable) {
		t.Fatalf("Check = %v, want ErrPasswordAlgorithmUnavailable", err)
	}
	if len(f.audits) != 1 || f.audits[0].Kind != action || f.audits[0].Outcome != SecurityOutcomeFailed || deniedAudits(f, action) != 0 {
		t.Fatalf("audits %+v, want one %s with the outcome failed", f.audits, action)
	}
}

// RegisterArgon2id may race with hashing and verification: every check
// sees either no implementation (ErrPasswordAlgorithmUnavailable) or the one
// that won, never a torn or swapped one. Run with -race.
func TestRegisterArgon2idConcurrentWithHashing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	registry := &argon2Registry{}
	hasher := cheapPasswordHasher(t, WithPasswordWorkLimiter(mustLimiter(t, 8)))
	hasher.kdfs = registry
	stored := argon2Hash("secret", testArgon2Params, bytes.Repeat([]byte{6}, passwordSaltBytes), passwordKeyBytes)
	var wins sync.WaitGroup
	won := make(chan int, 8)
	start := make(chan struct{})
	for i := range 8 {
		wins.Go(func() {
			<-start
			if registry.register(fakeArgon2idKDF) {
				won <- i
			}
		})
	}
	var checks sync.WaitGroup
	errs := make(chan error, 64)
	for range 16 {
		checks.Go(func() {
			<-start
			for range 4 {
				match, rehash, err := hasher.Verify(ctx, stored, "secret")
				switch {
				case errors.Is(err, ErrPasswordAlgorithmUnavailable) && !match && !rehash:
				case err == nil && match && !rehash:
				default:
					errs <- errors.Join(errors.New("Verify during registration"), err)
				}
				hash, err := hasher.Hash(ctx, "secret")
				switch {
				case errors.Is(err, ErrPasswordAlgorithmUnavailable) && hash == "":
				case err == nil && strings.HasPrefix(hash, "$argon2id$"):
				default:
					errs <- errors.Join(errors.New("Hash during registration: "+hash), err)
				}
				_ = hasher.Algorithm()
			}
		})
	}
	close(start)
	wins.Wait()
	checks.Wait()
	close(won)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if winners := len(won); winners != 1 {
		t.Fatalf("%d registrations won, want exactly one", winners)
	}
	if match, _, err := hasher.Verify(ctx, stored, "secret"); err != nil || !match {
		t.Fatalf("after registration: match=%v err=%v", match, err)
	}
}

func mustLimiter(t *testing.T, slots int) *PasswordWorkLimiter {
	t.Helper()
	limiter, err := NewPasswordWorkLimiter(slots, DefaultPasswordWorkWait)
	if err != nil {
		t.Fatal(err)
	}
	return limiter
}
