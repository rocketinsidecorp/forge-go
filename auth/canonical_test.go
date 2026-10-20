package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestCanonicalLoginTrimsAndLowercasesASCII(t *testing.T) {
	login, ok := CanonicalLogin("  Ada.Lovelace+x@Example.COM \n")
	if !ok || login != "ada.lovelace+x@example.com" {
		t.Fatalf("login=%q ok=%v", login, ok)
	}
}

func TestCanonicalLoginRefusesLookAlikesAndMalformedLogins(t *testing.T) {
	for _, invalid := range []string{
		"",
		"   ",
		"no-at-sign",
		"two@@example.com",
		"a@b@example.com",
		"@example.com",
		"user@",
		"user@\u0130cloud.com",    // LATIN CAPITAL LETTER I WITH DOT ABOVE
		"\u212aelvin@example.com", // KELVIN SIGN lowercases to ASCII k
		"caf\u00e9@example.com",   // non-ASCII local part
		"user@icloud.com\u0000",   // NUL
		"us er@example.com",       // inner space
		"user@ex\tample.com",      // control
		"user@example.com\u00a0x", // inner no-break space
		"user@example.com\x7f",    // DEL
		"user@\uff45xample.com",   // fullwidth e
		strings.Repeat("a", MaxLoginBytes) + "@x", // over the length limit
	} {
		if login, ok := CanonicalLogin(invalid); ok {
			t.Errorf("CanonicalLogin(%q) = %q, want refusal", invalid, login)
		}
	}
	if _, ok := CanonicalLogin(strings.Repeat("a", MaxLoginBytes-2) + "@x"); !ok {
		t.Error("a login of exactly MaxLoginBytes was refused")
	}
}

func TestNormalizeIdentityLowercasesTenantsAsASCIIOnly(t *testing.T) {
	email, tenant, ok := NormalizeIdentity(" Owner@Example.com ", " ACME-1 ")
	if !ok || email != "owner@example.com" || tenant != "acme-1" {
		t.Fatalf("identity=(%q,%q,%v)", email, tenant, ok)
	}
	// strings.ToLower would turn "ac\u212ae" into "acke", a different tenant.
	if _, _, ok := NormalizeIdentity("a@example.com", "ac\u212ae"); ok {
		t.Fatal("a tenant slug with a Kelvin sign was accepted")
	}
}

func TestLoginTreatsLookAlikeLoginsAsUnknownAccounts(t *testing.T) {
	passwordHash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	store := &canonicalLoginStore{
		login:    "kelvin@example.com",
		identity: PasswordIdentity{SubjectID: uuid.MustNew(), MembershipID: uuid.MustNew(), PasswordHash: passwordHash, CredentialVersion: 1},
	}
	sessions := &sessionStub{}
	service, err := NewLoginService(store, sessions)
	if err != nil {
		t.Fatal(err)
	}
	// strings.ToLower folds the Kelvin sign to an ASCII 'k', so the previous
	// normalization signed this look-alike in as the real account.
	for _, typed := range []string{"\u212aelvin@example.com", "kelvin@\u0130xample.com"} {
		_, err := service.Login(context.Background(), LoginInput{Email: typed, Password: "correct horse battery staple", TenantSlug: "acme"})
		if !errors.Is(err, ErrCredentialsInvalid) {
			t.Fatalf("%q error=%v, want ErrCredentialsInvalid", typed, err)
		}
		if store.matched {
			t.Fatalf("%q reached the real account", typed)
		}
	}
	if sessions.membership != (uuid.UUID{}) {
		t.Fatal("a look-alike login created a session")
	}
	if _, err := service.Login(context.Background(), LoginInput{Email: "KELVIN@example.com", Password: "correct horse battery staple", TenantSlug: "acme"}); err == nil && !store.matched {
		t.Fatal("the genuine account did not match")
	}
}

// canonicalLoginStore knows one account and records whether a lookup named it.
type canonicalLoginStore struct {
	login    string
	identity PasswordIdentity
	matched  bool
}

func (s *canonicalLoginStore) LookupPassword(_ context.Context, email, tenant string) (PasswordIdentity, bool, error) {
	if email == s.login && tenant == "acme" {
		s.matched = true
		return s.identity, true, nil
	}
	return PasswordIdentity{}, false, nil
}

func (s *canonicalLoginStore) ReplacePasswordHash(context.Context, uuid.UUID, string, string) (bool, error) {
	return false, errors.New("unexpected password replacement")
}

func TestRecoveryNeverLooksUpOrDeliversToALookAlike(t *testing.T) {
	victim := uuid.MustNew()
	store := &recoveryStoreStub{
		found:    true,
		identity: RecoveryIdentity{SubjectID: victim, Email: "user@icloud.com"},
	}
	sender := &recoverySenderStub{}
	service, err := NewRecoveryService(store, sender)
	if err != nil {
		t.Fatal(err)
	}
	// PostgreSQL can fold the typed address to the victim's (\u0130 -> i), and a
	// mail system can deliver it to the same mailbox.
	for _, typed := range []string{"user@\u0130cloud.com", "USER@\u0131cloud.com", "\u212aser@icloud.com"} {
		if err := service.Request(context.Background(), RecoveryRequest{Email: typed, TenantSlug: "acme", Source: "192.0.2.10"}); err != nil {
			t.Fatalf("%q error=%v", typed, err)
		}
	}
	if store.lookupEmail != "" || store.createdDigest != (Digest{}) || sender.calls != 0 {
		t.Fatalf("a look-alike reached the store or the sender: lookup=%q created=%x sends=%d", store.lookupEmail, store.createdDigest, sender.calls)
	}
}

func TestRecoveryDeliversOnlyToTheStoredAddress(t *testing.T) {
	store := &recoveryStoreStub{
		found:    true,
		identity: RecoveryIdentity{SubjectID: uuid.MustNew(), Email: "User@iCloud.com"},
	}
	sender := &recoverySenderStub{}
	service, err := NewRecoveryService(store, sender)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Request(context.Background(), RecoveryRequest{Email: " USER@ICLOUD.COM ", TenantSlug: "acme"}); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 1 || sender.message.Email != "User@iCloud.com" {
		t.Fatalf("delivered to %q (%d sends), want the stored address", sender.message.Email, sender.calls)
	}
}
