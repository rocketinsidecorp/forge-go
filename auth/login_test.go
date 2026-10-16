package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

type loginStoreStub struct {
	identity   PasswordIdentity
	found      bool
	lookupErr  error
	replaceErr error
	replaced   bool
	lookEmail  string
	lookTenant string
}

func (s *loginStoreStub) LookupPassword(_ context.Context, email, tenant string) (PasswordIdentity, bool, error) {
	s.lookEmail, s.lookTenant = email, tenant
	return s.identity, s.found, s.lookupErr
}

func (s *loginStoreStub) ReplacePasswordHash(_ context.Context, subjectID uuid.UUID, previous, next string) (bool, error) {
	if subjectID != s.identity.SubjectID || previous != s.identity.PasswordHash || next == "" || next == previous {
		return false, errors.New("unexpected password replacement")
	}
	return s.replaced, s.replaceErr
}

type sessionStub struct {
	membership        uuid.UUID
	credentialVersion int64
	expiresAt         time.Time
	token             Token
	err               error
}

func (s *sessionStub) CreateSession(_ context.Context, membership uuid.UUID, expiresAt time.Time) (Token, error) {
	s.membership, s.expiresAt = membership, expiresAt
	return s.token, s.err
}

func (s *sessionStub) CreateSessionAtVersion(_ context.Context, membership uuid.UUID, credentialVersion int64, expiresAt time.Time) (Token, error) {
	s.membership, s.credentialVersion, s.expiresAt = membership, credentialVersion, expiresAt
	return s.token, s.err
}

func TestLoginNormalizesIdentifierAndCreatesSession(t *testing.T) {
	passwordHash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	subjectID, membershipID := uuid.MustNew(), uuid.MustNew()
	store := &loginStoreStub{
		identity: PasswordIdentity{SubjectID: subjectID, MembershipID: membershipID, PasswordHash: passwordHash, CredentialVersion: 1},
		found:    true,
	}
	sessions := &sessionStub{token: token}
	now := time.Date(2026, 9, 27, 13, 0, 0, 0, time.UTC)
	service, err := NewLoginService(store, sessions, WithLoginClock(func() time.Time { return now }), WithSessionTTL(12*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.Login(context.Background(), LoginInput{
		Email:      "  Alice@Example.COM ",
		Password:   "correct horse battery staple",
		TenantSlug: " ACME ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.lookEmail != "alice@example.com" || store.lookTenant != "acme" {
		t.Fatalf("lookup=(%q,%q)", store.lookEmail, store.lookTenant)
	}
	if sessions.membership != membershipID || sessions.credentialVersion != 1 || sessions.expiresAt != now.Add(12*time.Hour) {
		t.Fatalf("session membership=%s version=%d expires=%v", sessions.membership, sessions.credentialVersion, sessions.expiresAt)
	}
	if result.Token.Digest() != token.Digest() || result.ExpiresAt != now.Add(12*time.Hour) {
		t.Fatalf("result=%+v", result)
	}
}

func TestLoginFailsClosedForAuthenticationMisses(t *testing.T) {
	validHash, err := HashPassword("right-password")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		input LoginInput
		store *loginStoreStub
	}{
		{"unknown identity", LoginInput{Email: "a@example.com", Password: "wrong", TenantSlug: "acme"}, &loginStoreStub{}},
		{"wrong password", LoginInput{Email: "a@example.com", Password: "wrong", TenantSlug: "acme"}, &loginStoreStub{found: true, identity: PasswordIdentity{SubjectID: uuid.MustNew(), MembershipID: uuid.MustNew(), PasswordHash: validHash, CredentialVersion: 1}}},
		{"invalid email", LoginInput{Email: "not-email", Password: "wrong", TenantSlug: "acme"}, &loginStoreStub{}},
		{"invalid tenant", LoginInput{Email: "a@example.com", Password: "wrong", TenantSlug: "../global"}, &loginStoreStub{}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			sessions := &sessionStub{}
			service, err := NewLoginService(test.store, sessions)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Login(context.Background(), test.input); !errors.Is(err, ErrCredentialsInvalid) {
				t.Fatalf("error=%v", err)
			}
			if sessions.membership != (uuid.UUID{}) {
				t.Fatal("session created for invalid credentials")
			}
		})
	}
}

func TestLoginRehashRaceDoesNotIssueSession(t *testing.T) {
	oldHash, err := hashPassword("secret", minAcceptedPasswordIterations, zeroReader{})
	if err != nil {
		t.Fatal(err)
	}
	store := &loginStoreStub{
		found:    true,
		identity: PasswordIdentity{SubjectID: uuid.MustNew(), MembershipID: uuid.MustNew(), PasswordHash: oldHash, CredentialVersion: 1},
		replaced: false,
	}
	sessions := &sessionStub{}
	service, err := NewLoginService(store, sessions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(context.Background(), LoginInput{Email: "a@example.com", Password: "secret", TenantSlug: "acme"}); !errors.Is(err, ErrCredentialsInvalid) {
		t.Fatalf("error=%v", err)
	}
	if sessions.membership != (uuid.UUID{}) {
		t.Fatal("session created after credential race")
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type securityAuditorStub struct {
	events []SecurityEvent
	err    error
}

func (s *securityAuditorStub) RecordSecurityEvent(_ context.Context, event SecurityEvent) error {
	s.events = append(s.events, event)
	return s.err
}

type revokingSessionStub struct {
	sessionStub
	revoked   Token
	revokeErr error
}

func (s *revokingSessionStub) RevokeSession(_ context.Context, token Token) error {
	s.revoked = token
	return s.revokeErr
}

func TestLoginAuditsDeniedAndSuccessfulOutcomes(t *testing.T) {
	hash, err := HashPassword("correct-password")
	if err != nil {
		t.Fatal(err)
	}
	subjectID, membershipID := uuid.MustNew(), uuid.MustNew()
	store := &loginStoreStub{
		found: true,
		identity: PasswordIdentity{
			SubjectID: subjectID, MembershipID: membershipID, PasswordHash: hash,
			CredentialVersion: 1,
		},
	}
	auditor := &securityAuditorStub{}
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	sessions := &sessionStub{token: token}
	service, err := NewLoginService(store, sessions, WithSecurityAuditor(auditor))
	if err != nil {
		t.Fatal(err)
	}

	input := LoginInput{
		Email: "alice@example.com", Password: "wrong-password",
		TenantSlug: "acme", Source: "192.0.2.10",
	}
	if _, err := service.Login(context.Background(), input); !errors.Is(err, ErrCredentialsInvalid) {
		t.Fatalf("denied error=%v", err)
	}
	if len(auditor.events) != 1 || auditor.events[0].Kind != SecurityLoginFailed ||
		auditor.events[0].Outcome != SecurityOutcomeDenied ||
		auditor.events[0].SubjectID != subjectID ||
		auditor.events[0].MembershipID != membershipID ||
		auditor.events[0].AccountDigest == (Digest{}) ||
		auditor.events[0].SourceDigest == (Digest{}) {
		t.Fatalf("denied audit=%+v", auditor.events)
	}

	input.Password = "correct-password"
	result, err := service.Login(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Token.Digest() != token.Digest() {
		t.Fatalf("token=%v", result.Token)
	}
	if len(auditor.events) != 2 || auditor.events[1].Kind != SecurityLoginSucceeded ||
		auditor.events[1].Outcome != SecurityOutcomeSucceeded ||
		auditor.events[1].SubjectID != subjectID ||
		auditor.events[1].MembershipID != membershipID {
		t.Fatalf("success audit=%+v", auditor.events)
	}
}

func TestLoginAuditsThrottleWithoutCredentialLookup(t *testing.T) {
	limiter, err := newMemoryLoginThrottler(LoginThrottleConfig{
		Window: time.Minute, AccountLimit: 1, SourceLimit: 10, MaxEntries: 100,
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	store := &loginStoreStub{}
	auditor := &securityAuditorStub{}
	service, err := NewLoginService(store, &sessionStub{},
		WithLoginThrottler(limiter), WithSecurityAuditor(auditor))
	if err != nil {
		t.Fatal(err)
	}
	input := LoginInput{Email: "a@example.com", Password: "wrong", TenantSlug: "acme", Source: "192.0.2.10"}
	if _, err := service.Login(context.Background(), input); !errors.Is(err, ErrCredentialsInvalid) {
		t.Fatalf("first error=%v", err)
	}
	store.lookupErr = errors.New("lookup must not run")
	_, err = service.Login(context.Background(), input)
	var throttled *LoginThrottledError
	if !errors.As(err, &throttled) {
		t.Fatalf("second error=%v", err)
	}
	if len(auditor.events) != 2 || auditor.events[1].Kind != SecurityLoginThrottled ||
		auditor.events[1].Outcome != SecurityOutcomeDenied {
		t.Fatalf("events=%+v", auditor.events)
	}
}

func TestLoginRevokesCreatedSessionWhenSuccessAuditFails(t *testing.T) {
	hash, err := HashPassword("correct-password")
	if err != nil {
		t.Fatal(err)
	}
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	store := &loginStoreStub{
		found: true,
		identity: PasswordIdentity{
			SubjectID: uuid.MustNew(), MembershipID: uuid.MustNew(), PasswordHash: hash,
			CredentialVersion: 1,
		},
	}
	sessions := &revokingSessionStub{sessionStub: sessionStub{token: token}}
	auditErr := errors.New("audit unavailable")
	auditor := &securityAuditorStub{err: auditErr}
	service, err := NewLoginService(store, sessions, WithSecurityAuditor(auditor))
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.Login(context.Background(), LoginInput{
		Email: "alice@example.com", Password: "correct-password", TenantSlug: "acme",
	})
	if !errors.Is(err, auditErr) {
		t.Fatalf("error=%v", err)
	}
	if result.Token != (Token{}) {
		t.Fatalf("token delivered despite audit failure: %v", result.Token)
	}
	if sessions.revoked.Digest() != token.Digest() {
		t.Fatalf("revoked=%v want token digest=%x", sessions.revoked, token.Digest())
	}
}

func TestDeniedLoginRemainsDeniedWhenAuditFails(t *testing.T) {
	service, err := NewLoginService(
		&loginStoreStub{},
		&sessionStub{},
		WithSecurityAuditor(&securityAuditorStub{err: errors.New("audit unavailable")}),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Login(context.Background(), LoginInput{
		Email: "nobody@example.com", Password: "wrong", TenantSlug: "acme",
	})
	if !errors.Is(err, ErrCredentialsInvalid) {
		t.Fatalf("error=%v", err)
	}
}
