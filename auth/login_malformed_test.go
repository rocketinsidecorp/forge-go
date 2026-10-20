package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

// lookupRecorder records every credential lookup of a sign-in.
type lookupRecorder struct {
	loginStoreStub
	lookups [][2]string
}

func (s *lookupRecorder) LookupPassword(ctx context.Context, email, tenant string) (PasswordIdentity, bool, error) {
	s.lookups = append(s.lookups, [2]string{email, tenant})
	return s.loginStoreStub.LookupPassword(ctx, email, tenant)
}

func TestLoginRunsTheSameLookupForMalformedAndUnknownIdentities(t *testing.T) {
	for _, input := range []LoginInput{
		{Email: "nobody@example.com", Password: "wrong password", TenantSlug: "acme"},       // unknown
		{Email: "nobody@example.com", Password: "wrong password", TenantSlug: "../global"},  // malformed tenant
		{Email: "nobody@example.com", Password: "wrong password", TenantSlug: "AC\u212aME"}, // look-alike tenant
		{Email: "nobody@\u0130cloud.com", Password: "wrong password", TenantSlug: "acme"},   // non-ASCII login
		{Email: "not an email", Password: "wrong password", TenantSlug: "acme"},             // malformed login
	} {
		store := &lookupRecorder{}
		auditor := &securityAuditorStub{}
		service, err := NewLoginService(store, &sessionStub{}, WithSecurityAuditor(auditor))
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.Login(context.Background(), input)
		if !errors.Is(err, ErrCredentialsInvalid) {
			t.Fatalf("%+v: error=%v, want ErrCredentialsInvalid", input, err)
		}
		// Malformed input must cost the same credential lookup as a
		// well-formed miss; before, it returned before touching the store.
		if len(store.lookups) != 1 {
			t.Fatalf("%+v: %d credential lookups, want 1", input, len(store.lookups))
		}
		if len(auditor.events) != 1 || auditor.events[0].Kind != SecurityLoginFailed || auditor.events[0].Outcome != SecurityOutcomeDenied {
			t.Fatalf("%+v: audit=%+v", input, auditor.events)
		}
	}
}

func TestLoginNeverLooksUpMalformedValuesAsTyped(t *testing.T) {
	store := &lookupRecorder{}
	service, err := NewLoginService(store, &sessionStub{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(context.Background(), LoginInput{Email: "victim@\u0130cloud.com", Password: "wrong password", TenantSlug: "../global"}); !errors.Is(err, ErrCredentialsInvalid) {
		t.Fatal(err)
	}
	email, tenant := store.lookups[0][0], store.lookups[0][1]
	if strings.Contains(email, "victim") || !tenantSlugPattern.MatchString(tenant) || !strings.HasSuffix(email, "@invalid.example") {
		t.Fatalf("lookup=(%q,%q): malformed input must be replaced by random identifiers", email, tenant)
	}
	other := &lookupRecorder{}
	service, _ = NewLoginService(other, &sessionStub{})
	_, _ = service.Login(context.Background(), LoginInput{Email: "victim@\u0130cloud.com", Password: "wrong password", TenantSlug: "../global"})
	if other.lookups[0] == store.lookups[0] {
		t.Fatal("the random lookup identifiers repeat")
	}
}

func TestLoginCountsMalformedAttemptsAgainstTheSource(t *testing.T) {
	limiter, _ := newTestThrottler(t, LoginThrottleConfig{Window: time.Minute, PairLimit: 5, AccountLimit: 50, SourceLimit: 3, MaxEntries: 100})
	service, err := NewLoginService(&lookupRecorder{}, &sessionStub{}, WithLoginThrottler(limiter))
	if err != nil {
		t.Fatal(err)
	}
	// Each attempt costs a password hash, so malformed ones are not free:
	// the source limit stops a flood of them like any other.
	var throttled *LoginThrottledError
	for i := 0; i < 3; i++ {
		_, err := service.Login(context.Background(), LoginInput{Email: "bad login " + string(rune('a'+i)), Password: "wrong password", TenantSlug: "acme", Source: "192.0.2.9"})
		if !errors.Is(err, ErrCredentialsInvalid) {
			t.Fatalf("attempt %d error=%v", i, err)
		}
	}
	_, err = service.Login(context.Background(), LoginInput{Email: "bad login d", Password: "wrong password", TenantSlug: "acme", Source: "192.0.2.9"})
	if !errors.As(err, &throttled) {
		t.Fatalf("fourth malformed attempt error=%v, want throttling", err)
	}
}

func TestLoginLetterCaseVariantsShareOneThrottleBudget(t *testing.T) {
	limiter, _ := newTestThrottler(t, LoginThrottleConfig{Window: time.Minute, PairLimit: 2, AccountLimit: 10, SourceLimit: 100, MaxEntries: 100})
	store := &lookupRecorder{loginStoreStub: loginStoreStub{found: false}}
	service, err := NewLoginService(store, &sessionStub{}, WithLoginThrottler(limiter))
	if err != nil {
		t.Fatal(err)
	}
	variants := []string{"Owner@Example.com", "OWNER@EXAMPLE.COM", " owner@example.com "}
	for i, typed := range variants[:2] {
		if _, err := service.Login(context.Background(), LoginInput{Email: typed, Password: "wrong password", TenantSlug: "acme", Source: "192.0.2.9"}); !errors.Is(err, ErrCredentialsInvalid) {
			t.Fatalf("variant %d error=%v", i, err)
		}
	}
	_, err = service.Login(context.Background(), LoginInput{Email: variants[2], Password: "wrong password", TenantSlug: "acme", Source: "192.0.2.9"})
	var throttled *LoginThrottledError
	if !errors.As(err, &throttled) {
		t.Fatalf("a third spelling of one login got a fresh guess: %v", err)
	}
	// The tenant field is not part of the budget either.
	_, err = service.Login(context.Background(), LoginInput{Email: "owner@example.com", Password: "wrong password", TenantSlug: "other-tenant", Source: "192.0.2.9"})
	if !errors.As(err, &throttled) {
		t.Fatalf("rotating the tenant gave a fresh guess: %v", err)
	}
}

func TestLoginStillSignsInAfterTheMalformedPathChanges(t *testing.T) {
	passwordHash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	store := &lookupRecorder{loginStoreStub: loginStoreStub{
		found:    true,
		identity: PasswordIdentity{SubjectID: uuid.MustNew(), MembershipID: uuid.MustNew(), PasswordHash: passwordHash, CredentialVersion: 1},
	}}
	service, err := NewLoginService(store, &sessionStub{token: token})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(context.Background(), LoginInput{Email: "Alice@Example.com", Password: "correct horse battery staple", TenantSlug: "ACME"}); err != nil {
		t.Fatal(err)
	}
	if got := store.lookups; len(got) != 1 || got[0] != [2]string{"alice@example.com", "acme"} {
		t.Fatalf("lookups=%v", got)
	}
}
