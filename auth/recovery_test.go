package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

type recoveryStoreStub struct {
	identity       RecoveryIdentity
	found          bool
	lookupErr      error
	createErr      error
	invalidateErr  error
	consumeErr     error
	consumeResult  bool
	lookupEmail    string
	lookupTenant   string
	createdSubject uuid.UUID
	createdDigest  Digest
	createdExpiry  time.Time
	invalidated    Digest
	consumedDigest Digest
	consumedHash   string
}

func (s *recoveryStoreStub) LookupRecovery(_ context.Context, email, tenant string) (RecoveryIdentity, bool, error) {
	s.lookupEmail, s.lookupTenant = email, tenant
	return s.identity, s.found, s.lookupErr
}

func (s *recoveryStoreStub) CreateRecovery(_ context.Context, subjectID uuid.UUID, digest Digest, expiresAt time.Time) error {
	s.createdSubject, s.createdDigest, s.createdExpiry = subjectID, digest, expiresAt
	return s.createErr
}

func (s *recoveryStoreStub) InvalidateRecovery(_ context.Context, digest Digest) error {
	s.invalidated = digest
	return s.invalidateErr
}

func (s *recoveryStoreStub) ConsumeRecovery(_ context.Context, digest Digest, hash string) (bool, error) {
	s.consumedDigest, s.consumedHash = digest, hash
	return s.consumeResult, s.consumeErr
}

type recoverySenderStub struct {
	message PasswordRecoveryMessage
	err     error
	calls   int
}

func (s *recoverySenderStub) SendPasswordRecovery(_ context.Context, message PasswordRecoveryMessage) error {
	s.message = message
	s.calls++
	return s.err
}

func TestRecoveryRequestIssuesDigestOnlyTokenAndSendsMessage(t *testing.T) {
	userID := uuid.MustNew()
	store := &recoveryStoreStub{
		found:    true,
		identity: RecoveryIdentity{SubjectID: userID, Email: "Alice@example.com"},
	}
	sender := &recoverySenderStub{}
	now := time.Date(2026, 9, 27, 14, 30, 0, 0, time.UTC)
	service, err := NewRecoveryService(store, sender, WithRecoveryClock(func() time.Time { return now }), WithRecoveryTTL(45*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	err = service.Request(context.Background(), RecoveryRequest{
		Email:      " ALICE@EXAMPLE.COM ",
		TenantSlug: " ACME ",
		Source:     "192.0.2.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.lookupEmail != "alice@example.com" || store.lookupTenant != "acme" {
		t.Fatalf("lookup=(%q,%q)", store.lookupEmail, store.lookupTenant)
	}
	if store.createdSubject != userID || store.createdDigest != sender.message.Token.Digest() ||
		!store.createdExpiry.Equal(now.Add(45*time.Minute)) {
		t.Fatalf("created subject=%s expiry=%v", store.createdSubject, store.createdExpiry)
	}
	if sender.calls != 1 || sender.message.Email != "Alice@example.com" || sender.message.TenantSlug != "acme" ||
		sender.message.Token.Reveal() == "" || !sender.message.ExpiresAt.Equal(now.Add(45*time.Minute)) {
		t.Fatalf("message=%+v calls=%d", sender.message, sender.calls)
	}
	if strings.Contains(fmt.Sprint(sender.message.Token), sender.message.Token.Reveal()) {
		t.Fatal("recovery token leaked through String")
	}
}

func TestRecoveryRequestHidesUnknownAndInvalidIdentities(t *testing.T) {
	for _, request := range []RecoveryRequest{
		{Email: "nobody@example.com", TenantSlug: "acme", Source: "192.0.2.10"},
		{Email: "not-email", TenantSlug: "acme", Source: "192.0.2.10"},
		{Email: "nobody@example.com", TenantSlug: "../global", Source: "192.0.2.10"},
	} {
		store := &recoveryStoreStub{}
		sender := &recoverySenderStub{}
		service, err := NewRecoveryService(store, sender)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Request(context.Background(), request); err != nil {
			t.Fatalf("request=%+v error=%v", request, err)
		}
		if sender.calls != 0 || store.createdDigest != (Digest{}) {
			t.Fatalf("request=%+v sender_calls=%d created=%x", request, sender.calls, store.createdDigest)
		}
	}
}

func TestRecoveryRequestInvalidatesTokenWhenDeliveryFails(t *testing.T) {
	store := &recoveryStoreStub{
		found:    true,
		identity: RecoveryIdentity{SubjectID: uuid.MustNew(), Email: "alice@example.com"},
	}
	senderErr := errors.New("mailer unavailable")
	sender := &recoverySenderStub{err: senderErr}
	service, err := NewRecoveryService(store, sender)
	if err != nil {
		t.Fatal(err)
	}
	err = service.Request(context.Background(), RecoveryRequest{
		Email: "alice@example.com", TenantSlug: "acme", Source: "192.0.2.10",
	})
	if !errors.Is(err, senderErr) {
		t.Fatalf("error=%v", err)
	}
	if store.invalidated == (Digest{}) || store.invalidated != store.createdDigest {
		t.Fatalf("created=%x invalidated=%x", store.createdDigest, store.invalidated)
	}
}

func TestRecoveryRequestThrottlesBeforeLookup(t *testing.T) {
	limiter, err := newMemoryLoginThrottler(LoginThrottleConfig{
		Window:       time.Minute,
		AccountLimit: 1,
		SourceLimit:  10,
		MaxEntries:   100,
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	store := &recoveryStoreStub{}
	service, err := NewRecoveryService(store, &recoverySenderStub{}, WithRecoveryThrottler(limiter))
	if err != nil {
		t.Fatal(err)
	}
	request := RecoveryRequest{Email: "a@example.com", TenantSlug: "acme", Source: "192.0.2.10"}
	if err := service.Request(context.Background(), request); err != nil {
		t.Fatalf("first request=%v", err)
	}
	store.lookupErr = errors.New("lookup must not run")
	err = service.Request(context.Background(), request)
	var throttled *RecoveryThrottledError
	if !errors.As(err, &throttled) || !errors.Is(err, ErrRecoveryThrottled) || throttled.RetryAfter <= 0 {
		t.Fatalf("second error=%v", err)
	}
}

func TestRecoveryResetHashesPasswordAndConsumesToken(t *testing.T) {
	token, err := NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	store := &recoveryStoreStub{consumeResult: true}
	service, err := NewRecoveryService(store, &recoverySenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Reset(context.Background(), PasswordReset{Token: token.Reveal(), NewPassword: "new strong password"}); err != nil {
		t.Fatal(err)
	}
	if store.consumedDigest != token.Digest() || store.consumedHash == "" || strings.Contains(store.consumedHash, "new strong password") {
		t.Fatalf("digest=%x hash=%q", store.consumedDigest, store.consumedHash)
	}
	match, _, err := VerifyPassword(store.consumedHash, "new strong password")
	if err != nil || !match {
		t.Fatalf("password match=%v err=%v", match, err)
	}
}

func TestRecoveryResetFailsClosed(t *testing.T) {
	service, err := NewRecoveryService(&recoveryStoreStub{}, &recoverySenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Reset(context.Background(), PasswordReset{Token: "malformed", NewPassword: "new password"}); !errors.Is(err, ErrRecoveryInvalid) {
		t.Fatalf("malformed token error=%v", err)
	}
	token, err := NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Reset(context.Background(), PasswordReset{Token: token.Reveal(), NewPassword: "new password"}); !errors.Is(err, ErrRecoveryInvalid) {
		t.Fatalf("unknown token error=%v", err)
	}
	if err := service.Reset(context.Background(), PasswordReset{Token: token.Reveal(), NewPassword: ""}); !errors.Is(err, ErrPasswordInvalid) {
		t.Fatalf("empty password error=%v", err)
	}
}

func TestRecoveryTokenIsCanonicalAndRedacted(t *testing.T) {
	token, err := newRecoveryTokenWithEntropy(zeroReader{})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRecoveryToken(token.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Digest() != token.Digest() || token.String() != "[REDACTED]" || strings.Contains(token.GoString(), token.Reveal()) {
		t.Fatalf("token=%s go=%s", token.String(), token.GoString())
	}
	if _, err := ParseRecoveryToken(token.Reveal() + "="); !errors.Is(err, ErrInvalidRecoveryToken) {
		t.Fatalf("non-canonical error=%v", err)
	}
}

func TestRecoveryResetThrottlesBeforePasswordWork(t *testing.T) {
	limiter, err := newMemoryLoginThrottler(LoginThrottleConfig{
		Window:       time.Minute,
		AccountLimit: 1,
		SourceLimit:  1,
		MaxEntries:   100,
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	token, err := NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	store := &recoveryStoreStub{}
	service, err := NewRecoveryService(store, &recoverySenderStub{}, WithRecoveryThrottler(limiter))
	if err != nil {
		t.Fatal(err)
	}
	reset := PasswordReset{Token: token.Reveal(), NewPassword: "replacement password", Source: "192.0.2.10"}
	if err := service.Reset(context.Background(), reset); !errors.Is(err, ErrRecoveryInvalid) {
		t.Fatalf("first error=%v", err)
	}
	store.consumeErr = errors.New("consume must not run after throttle")
	err = service.Reset(context.Background(), reset)
	var throttled *RecoveryThrottledError
	if !errors.As(err, &throttled) || !errors.Is(err, ErrRecoveryThrottled) || throttled.RetryAfter <= 0 {
		t.Fatalf("second error=%v", err)
	}
}

func FuzzParseRecoveryTokenNeverPanics(f *testing.F) {
	token, _ := newRecoveryTokenWithEntropy(zeroReader{})
	for _, seed := range []string{"", "not-a-token", token.Reveal(), token.Reveal() + "="} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		_, _ = ParseRecoveryToken(value)
	})
}

func TestRecoveryAuditsKnownAndUnknownRequests(t *testing.T) {
	subjectID := uuid.MustNew()
	store := &recoveryStoreStub{
		found:    true,
		identity: RecoveryIdentity{SubjectID: subjectID, Email: "alice@example.com"},
	}
	auditor := &securityAuditorStub{}
	service, err := NewRecoveryService(
		store,
		&recoverySenderStub{},
		WithRecoveryAuditor(auditor),
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := service.Request(context.Background(), RecoveryRequest{
		Email: "alice@example.com", TenantSlug: "acme", Source: "192.0.2.10",
	}); err != nil {
		t.Fatal(err)
	}
	if len(auditor.events) != 1 ||
		auditor.events[0].Kind != SecurityRecoveryRequested ||
		auditor.events[0].Outcome != SecurityOutcomeSucceeded ||
		auditor.events[0].SubjectID != subjectID ||
		auditor.events[0].AccountDigest == (Digest{}) ||
		auditor.events[0].SourceDigest == (Digest{}) ||
		auditor.events[0].CredentialDigest == (Digest{}) {
		t.Fatalf("known recovery audit=%+v", auditor.events)
	}

	store.found = false
	if err := service.Request(context.Background(), RecoveryRequest{
		Email: "nobody@example.com", TenantSlug: "acme", Source: "192.0.2.10",
	}); err != nil {
		t.Fatal(err)
	}
	if len(auditor.events) != 2 ||
		auditor.events[1].Kind != SecurityRecoveryRequested ||
		auditor.events[1].Outcome != SecurityOutcomeDenied ||
		auditor.events[1].SubjectID != (uuid.UUID{}) ||
		auditor.events[1].AccountDigest == (Digest{}) {
		t.Fatalf("unknown recovery audit=%+v", auditor.events)
	}
}

func TestPasswordResetAuditFailureMarksCommittedOperation(t *testing.T) {
	token, err := NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	auditCause := errors.New("audit unavailable")
	auditor := &securityAuditorStub{err: auditCause}
	service, err := NewRecoveryService(
		&recoveryStoreStub{consumeResult: true},
		&recoverySenderStub{},
		WithRecoveryAuditor(auditor),
	)
	if err != nil {
		t.Fatal(err)
	}
	err = service.Reset(context.Background(), PasswordReset{
		Token: token.Reveal(), NewPassword: "replacement password", Source: "192.0.2.10",
	})
	var auditErr *SecurityAuditError
	if !errors.As(err, &auditErr) || !errors.Is(err, auditCause) || !auditErr.OperationApplied {
		t.Fatalf("error=%v audit=%+v", err, auditErr)
	}
	if len(auditor.events) != 1 ||
		auditor.events[0].Kind != SecurityPasswordReset ||
		auditor.events[0].Outcome != SecurityOutcomeSucceeded ||
		auditor.events[0].CredentialDigest != token.Digest() ||
		auditor.events[0].SourceDigest == (Digest{}) {
		t.Fatalf("events=%+v", auditor.events)
	}
}
