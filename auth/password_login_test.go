package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

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
		stored, err := parseStoredPasswordHash(hasher.dummy())
		if err != nil || stored.algorithm != hasher.Algorithm() || hasher.needsRehash(stored, hasher.Algorithm()) {
			t.Fatalf("dummy %q: %+v err=%v", hasher.dummy(), stored, err)
		}
		if match, _, err := hasher.Verify(context.Background(), hasher.dummy(), "any password"); err != nil || match {
			t.Fatalf("dummy matched: %v %v", match, err)
		}
	}
	if DefaultPasswordAlgorithm() == PasswordAlgorithmArgon2id && !strings.HasPrefix(DefaultPasswordHasher().dummy(), "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("default dummy %q is not at the production Argon2id cost", DefaultPasswordHasher().dummy())
	}

	limiter, err := NewPasswordWorkLimiter(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	hasher := cheapPasswordHasher(t, WithPasswordWorkLimiter(limiter))
	service, err := NewLoginService(newRehashStore(t, hasher.dummy()), &sessionStub{}, WithLoginPasswordHasher(hasher))
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
