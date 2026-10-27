package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestLoginServiceConstructionValidatesItsOptions(t *testing.T) {
	t.Parallel()
	store, sessions := &loginStoreStub{}, &sessionStub{}
	if _, err := NewLoginService(nil, sessions); err == nil {
		t.Error("a nil password store was accepted")
	}
	if _, err := NewLoginService(store, nil); err == nil {
		t.Error("a nil session creator was accepted")
	}
	for name, option := range map[string]LoginOption{
		"nil option":    nil,
		"short ttl":     WithSessionTTL(time.Second),
		"long ttl":      WithSessionTTL(365 * 24 * time.Hour),
		"nil clock":     WithLoginClock(nil),
		"nil throttler": WithLoginThrottler(nil),
		"nil issuer":    WithMFAChallengeIssuer(nil),
		"nil auditor":   WithSecurityAuditor(nil),
	} {
		if _, err := NewLoginService(store, sessions, option); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestLoginFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	input := LoginInput{Email: "alice@example.com", Password: mfaTestPassword, TenantSlug: "acme", Source: "192.0.2.1"}
	newService := func(store PasswordStore, sessions CredentialSessionCreator, options ...LoginOption) *LoginService {
		service, err := NewLoginService(store, sessions, options...)
		if err != nil {
			t.Fatal(err)
		}
		return service
	}

	// An empty or oversized password never reaches the store.
	store := cachedIdentity(t, false)
	service := newService(store, &sessionStub{})
	for _, password := range []string{"", strings.Repeat("x", maxPasswordBytes+1)} {
		bad := input
		bad.Password = password
		if _, err := service.Login(ctx, bad); !errors.Is(err, ErrCredentialsInvalid) {
			t.Errorf("password of %d bytes: %v", len(password), err)
		}
	}
	if store.lookEmail != "" {
		t.Error("an unusable password reached the store")
	}

	// Throttler failures.
	service = newService(cachedIdentity(t, false), &sessionStub{}, WithLoginThrottler(failingThrottler{attemptErr: errInjected}))
	if _, err := service.Login(ctx, input); !errors.Is(err, errInjected) {
		t.Errorf("attempt failure: %v", err)
	}
	sessions := &sessionStub{}
	service = newService(cachedIdentity(t, false), sessions, WithLoginThrottler(failingThrottler{allowed: true, successErr: errInjected}))
	if _, err := service.Login(ctx, input); !errors.Is(err, errInjected) {
		t.Errorf("success failure: %v", err)
	}
	if sessions.membership != (uuid.UUID{}) {
		t.Error("a session was created although the throttler failed")
	}

	// A denied attempt is throttled, and its audit failure is reported too.
	auditor := &securityAuditorStub{err: errors.New("audit down")}
	service = newService(cachedIdentity(t, false), &sessionStub{}, WithLoginThrottler(failingThrottler{}), WithSecurityAuditor(auditor))
	_, err := service.Login(ctx, input)
	var throttled *LoginThrottledError
	var auditFailure *SecurityAuditError
	if !errors.As(err, &throttled) || !errors.As(err, &auditFailure) || !hasEvent(auditor, SecurityLoginThrottled, SecurityOutcomeDenied) {
		t.Errorf("throttled login: %v events=%+v", err, auditor.events)
	}

	// Store failure.
	store = cachedIdentity(t, false)
	store.lookupErr = errInjected
	if _, err := newService(store, &sessionStub{}).Login(ctx, input); !errors.Is(err, errInjected) {
		t.Errorf("lookup failure: %v", err)
	}

	// An unknown account and a malformed login are the same refusal and
	// audited as denied.
	auditor = &securityAuditorStub{}
	unknown := &loginStoreStub{}
	service = newService(unknown, &sessionStub{}, WithSecurityAuditor(auditor))
	if _, err := service.Login(ctx, input); !errors.Is(err, ErrCredentialsInvalid) {
		t.Errorf("unknown account: %v", err)
	}
	malformed := input
	malformed.Email = "not an email"
	if _, err := service.Login(ctx, malformed); !errors.Is(err, ErrCredentialsInvalid) {
		t.Errorf("malformed login: %v", err)
	}
	if !strings.HasSuffix(unknown.lookEmail, "@invalid.example") {
		t.Errorf("a malformed login must look up a random identity, looked up %q", unknown.lookEmail)
	}
	if len(auditor.events) != 2 || !hasEvent(auditor, SecurityLoginFailed, SecurityOutcomeDenied) {
		t.Errorf("events=%+v", auditor.events)
	}
	// A very long malformed login is bounded in its throttle account.
	if account := invalidLoginAccount(strings.Repeat("x", 1000)); len(account) > 300 {
		t.Errorf("invalid login account is %d bytes", len(account))
	}

	// A store returning an unusable identity is an error that signs nobody in.
	for name, mutate := range map[string]func(*PasswordIdentity){
		"bad subject":    func(i *PasswordIdentity) { i.SubjectID = uuid.UUID{} },
		"bad membership": func(i *PasswordIdentity) { i.MembershipID = uuid.UUID{} },
		"bad version":    func(i *PasswordIdentity) { i.CredentialVersion = 0 },
	} {
		store = cachedIdentity(t, false)
		mutate(&store.identity)
		sessions = &sessionStub{}
		if _, err := newService(store, sessions).Login(ctx, input); err == nil || !strings.Contains(err.Error(), "invalid identity") {
			t.Errorf("%s: %v", name, err)
		}
		if sessions.membership != (uuid.UUID{}) {
			t.Errorf("%s: a session was created", name)
		}
	}

	// A corrupted stored hash is a server failure, not a refusal.
	store = cachedIdentity(t, false)
	store.identity.PasswordHash = "$garbage"
	if _, err := newService(store, &sessionStub{}).Login(ctx, input); err == nil || errors.Is(err, ErrCredentialsInvalid) {
		t.Errorf("corrupted hash: %v", err)
	}

	// A wrong password is refused and audited with the account.
	auditor = &securityAuditorStub{}
	store = cachedIdentity(t, false)
	wrong := input
	wrong.Password = "wrong password"
	if _, err := newService(store, &sessionStub{}, WithSecurityAuditor(auditor)).Login(ctx, wrong); !errors.Is(err, ErrCredentialsInvalid) {
		t.Errorf("wrong password: %v", err)
	}
	if len(auditor.events) != 1 || auditor.events[0].SubjectID != store.identity.SubjectID {
		t.Errorf("events=%+v", auditor.events)
	}
}

func TestLoginRehashesAnOlderHashAndFailsClosedOnStoreErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	oldHash, err := hashPassword(mfaTestPassword, minAcceptedPasswordIterations, zeroReader{})
	if err != nil {
		t.Fatal(err)
	}
	newStore := func() *loginStoreStub {
		return &loginStoreStub{found: true, replaced: true, identity: PasswordIdentity{
			SubjectID: uuid.MustNew(), MembershipID: uuid.MustNew(), PasswordHash: oldHash, CredentialVersion: 1,
		}}
	}
	input := LoginInput{Email: "alice@example.com", Password: mfaTestPassword, TenantSlug: "acme"}
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}

	store := newStore()
	sessions := &sessionStub{token: token}
	service, err := NewLoginService(store, sessions)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Login(ctx, input)
	if err != nil || result.Token.Reveal() != token.Reveal() {
		t.Fatalf("rehash login: %+v err=%v", result, err)
	}

	store = newStore()
	store.replaceErr = errInjected
	sessions = &sessionStub{token: token}
	service, _ = NewLoginService(store, sessions)
	if _, err := service.Login(ctx, input); !errors.Is(err, errInjected) {
		t.Errorf("replace failure: %v", err)
	}
	if sessions.membership != (uuid.UUID{}) {
		t.Error("a session was created although the rehash failed")
	}
}

func TestLoginMFAAndSessionFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	input := LoginInput{Email: "alice@example.com", Password: mfaTestPassword, TenantSlug: "acme"}

	// MFA required without an issuer fails closed and creates no session.
	sessions := &sessionStub{}
	service, err := NewLoginService(cachedIdentity(t, true), sessions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(ctx, input); err == nil || !strings.Contains(err.Error(), "no challenge issuer") {
		t.Errorf("no issuer: %v", err)
	}
	if sessions.membership != (uuid.UUID{}) {
		t.Error("a session was created for an account that needs MFA")
	}
	// The issuer failing is an error.
	issuer := &mfaIssuerStub{err: errInjected}
	service, _ = NewLoginService(cachedIdentity(t, true), sessions, WithMFAChallengeIssuer(issuer))
	if _, err := service.Login(ctx, input); !errors.Is(err, errInjected) {
		t.Errorf("issuer failure: %v", err)
	}

	// A session store failure is an error.
	service, _ = NewLoginService(cachedIdentity(t, false), &sessionStub{err: errInjected})
	if _, err := service.Login(ctx, input); !errors.Is(err, errInjected) {
		t.Errorf("session failure: %v", err)
	}

	// An audit failure on success revokes the session it just created.
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	revoking := &revokingSessionStub{sessionStub: sessionStub{token: token}}
	service, _ = NewLoginService(cachedIdentity(t, false), revoking, WithSecurityAuditor(&securityAuditorStub{err: errors.New("audit down")}))
	if _, err := service.Login(ctx, input); err == nil {
		t.Error("a login whose audit failed was accepted")
	}
	if revoking.revoked.Reveal() != token.Reveal() {
		t.Error("the session of a login whose audit failed was not revoked")
	}
	// Without a revoker the audit failure is simply returned.
	service, _ = NewLoginService(cachedIdentity(t, false), &sessionStub{token: token}, WithSecurityAuditor(&securityAuditorStub{err: errors.New("audit down")}))
	var auditFailure *SecurityAuditError
	if _, err := service.Login(ctx, input); !errors.As(err, &auditFailure) {
		t.Errorf("audit failure without a revoker: %v", err)
	}
}

func TestPrincipalValidationAndPermissions(t *testing.T) {
	t.Parallel()
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewPrincipal(uuid.UUID{}, tenant); err == nil {
		t.Error("a non-UUIDv7 subject was accepted")
	}
	if _, err := NewPrincipal(uuid.MustNew(), tenancy.Tenant{}); !errors.Is(err, tenancy.ErrInvalid) {
		t.Errorf("an empty tenant: %v", err)
	}
	if _, err := NewPrincipal(uuid.MustNew(), tenant, Permission("Bad Permission")); err == nil {
		t.Error("an invalid permission was accepted")
	}
	if _, err := NewPermission("NOPE"); err == nil {
		t.Error("an invalid permission was accepted")
	}
	principal, err := NewPrincipal(uuid.MustNew(), tenant, "reports:write", "reports:read", "reports:read")
	if err != nil {
		t.Fatal(err)
	}
	got := principal.Permissions()
	if fmt.Sprint(got) != "[reports:read reports:write]" {
		t.Errorf("permissions=%v, want sorted and unique", got)
	}
	got[0] = "tampered:x"
	if !principal.Can("reports:read") || principal.Can("tampered:x") || principal.Can("Not A Permission") {
		t.Error("Permissions() leaked its backing array, or Can accepted an invalid permission")
	}
}

func TestThrottlerValidationAndEdgeCases(t *testing.T) {
	t.Parallel()
	config := LoginThrottleConfig{Window: time.Minute, PairLimit: 1, AccountLimit: 2, SourceLimit: 3, MaxEntries: 10}
	if _, err := newMemoryLoginThrottler(config, nil); err == nil {
		t.Error("a nil clock was accepted")
	}
	if _, err := newMemoryLoginThrottler(LoginThrottleConfig{}, time.Now); err == nil {
		t.Error("an empty config was accepted")
	}

	// A canceled context fails Attempt and Success without counting.
	throttler, err := NewMemoryLoginThrottler(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	key := deriveLoginThrottleKey("alice@example.com", "192.0.2.1")
	if _, err := throttler.Attempt(ctx, key); !errors.Is(err, context.Canceled) {
		t.Errorf("Attempt: %v", err)
	}
	if err := throttler.Success(ctx, key); !errors.Is(err, context.Canceled) {
		t.Errorf("Success: %v", err)
	}

	// Success on a key that was never counted is a no-op, and a counter whose
	// window ended is dropped by the next attempt (a clock stepping back
	// included).
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	clock := now
	throttler, err = newMemoryLoginThrottler(config, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	if err := throttler.Success(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if decision, err := throttler.Attempt(context.Background(), key); err != nil || !decision.Allowed {
		t.Fatalf("first attempt: %+v err=%v", decision, err)
	}
	if decision, _ := throttler.Attempt(context.Background(), key); decision.Allowed {
		t.Fatal("the pair limit of 1 did not block")
	}
	clock = now.Add(2 * time.Minute)
	if decision, err := throttler.Attempt(context.Background(), key); err != nil || !decision.Allowed {
		t.Fatalf("after the window: %+v err=%v", decision, err)
	}
	clock = now.Add(-time.Hour) // a clock stepping backwards
	if decision, _ := throttler.Attempt(context.Background(), key); decision.Allowed {
		t.Fatal("a stepped-back clock must not forgive a counter that still blocks")
	}

	// Source text that is not an IP address is counted as typed; an IPv6
	// address counts as its /64 and an IPv4-mapped one as its IPv4.
	if got := throttleSource("  not-an-ip "); got != "not-an-ip" {
		t.Errorf("non-IP source=%q", got)
	}
	if throttleSource("2001:db8::1") != throttleSource("2001:db8::ffff") {
		t.Error("two addresses of one /64 must count together")
	}
	if got := throttleSource("::ffff:192.0.2.9"); got != "192.0.2.9" {
		t.Errorf("mapped address=%q", got)
	}
}

func TestPasswordEdgeCases(t *testing.T) {
	t.Parallel()
	if match, rehash, err := VerifyPassword("$pbkdf2-sha256$v=1$i=1$x$y", ""); match || rehash || err != nil {
		t.Errorf("empty password: %v %v %v", match, rehash, err)
	}
	hash := cachedPasswordHash(t)
	if _, _, err := VerifyPassword(hash, strings.Repeat("x", maxPasswordBytes+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Errorf("oversized password: %v", err)
	}
	if _, _, err := VerifyPassword("not a hash", mfaTestPassword); err == nil {
		t.Error("a malformed hash was accepted")
	}
	if _, err := hashPassword("x", 1, bytes.NewReader(nil)); err == nil {
		t.Error("too few iterations were accepted")
	}
	if _, err := hashPassword("x", minAcceptedPasswordIterations, nil); err == nil {
		t.Error("a nil entropy source was accepted")
	}
	if _, err := hashPassword("x", minAcceptedPasswordIterations, bytes.NewReader(nil)); err == nil {
		t.Error("an exhausted entropy source was accepted")
	}
}

func TestMFABackupCodeEdgeCases(t *testing.T) {
	t.Parallel()
	codes, _, err := newMFABackupCodes(1)
	if err != nil {
		t.Fatal(err)
	}
	if text := fmt.Sprintf("%#v", codes[0]); strings.Contains(text, codes[0].Reveal()) || !strings.Contains(text, "REDACTED") {
		t.Errorf("GoString leaks: %q", text)
	}
	if got := codes[0].LogValue().Kind(); got != slog.KindString || codes[0].LogValue().String() != "[REDACTED]" {
		t.Errorf("LogValue=%v", codes[0].LogValue())
	}
	for _, count := range []int{0, -1, 101} {
		if _, _, err := newMFABackupCodes(count); err == nil {
			t.Errorf("count %d was accepted", count)
		}
	}
	// A code of the right length whose Base32 text is not canonical (stray
	// trailing bits) is not a backup code.
	canonical := strings.ReplaceAll(codes[0].Reveal(), "-", "")
	if _, ok := parseMFABackupCode(canonical[:len(canonical)-1] + "="); ok {
		t.Error("a padded code was accepted")
	}
	if _, ok := parseMFABackupCode("11111-11111-11111-11111"); ok {
		t.Error("a code with characters outside Base32 was accepted")
	}
}
