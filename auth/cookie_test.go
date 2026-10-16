package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestCookieLoginSetsSecureHostCookies(t *testing.T) {
	hash, err := HashPassword("secret")
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
	sessions := &sessionStub{token: token}
	now := time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC)
	service, err := NewLoginService(store, sessions, WithLoginClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewCookieLoginHandler(service)
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"email":"alice@example.com","password":"secret","tenant":"acme"}`)
	request := httptest.NewRequest(http.MethodPost, "https://app.example.com/auth/login", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://app.example.com")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("cache headers: Cache-Control=%q Pragma=%q", recorder.Header().Get("Cache-Control"), recorder.Header().Get("Pragma"))
	}
	if strings.Contains(recorder.Body.String(), token.Reveal()) {
		t.Fatal("cookie login exposed the session token in the response body")
	}
	var response cookieLoginResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.CSRFToken == "" || !response.ExpiresAt.Equal(now.Add(defaultSessionTTL)) {
		t.Fatalf("response=%+v", response)
	}

	var sessionCookie, csrfCookie *http.Cookie
	for _, cookie := range recorder.Result().Cookies() {
		switch cookie.Name {
		case SessionCookieName:
			sessionCookie = cookie
		case CSRFCookieName:
			csrfCookie = cookie
		}
	}
	if sessionCookie == nil || sessionCookie.Value != token.Reveal() || !sessionCookie.Secure || !sessionCookie.HttpOnly ||
		sessionCookie.Path != "/" || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie=%+v", sessionCookie)
	}
	if csrfCookie == nil || csrfCookie.Value != response.CSRFToken || !csrfCookie.Secure || csrfCookie.HttpOnly ||
		csrfCookie.Path != "/" || csrfCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("csrf cookie=%+v", csrfCookie)
	}
}

func TestCookieLoginRequiresSameOriginHTTPS(t *testing.T) {
	service, err := NewLoginService(&loginStoreStub{}, &sessionStub{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewCookieLoginHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"email":"alice@example.com","password":"secret","tenant":"acme"}`)
	for _, origin := range []string{"", "http://app.example.com", "https://evil.example.com"} {
		request := httptest.NewRequest(http.MethodPost, "https://app.example.com/auth/login", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("origin=%q status=%d body=%s", origin, recorder.Code, recorder.Body.String())
		}
	}
}

func TestAuthenticateCookieUsesOnlySessionCookie(t *testing.T) {
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	principal, err := NewPrincipal(uuid.MustNew(), tenant, Permission("tasks:read"))
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := NewMiddleware(ResolverFunc(func(_ context.Context, digest Digest) (Session, bool, error) {
		if digest != token.Digest() {
			t.Fatalf("digest=%x", digest)
		}
		return Session{Principal: principal, ExpiresAt: time.Now().Add(time.Hour)}, true, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	nextCalled := false
	handler := middleware.AuthenticateCookie(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		got, ok := FromContext(r.Context())
		if !ok || got.SubjectID() != principal.SubjectID() || got.Tenant() != tenant {
			t.Fatalf("principal=%+v ok=%v", got, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "https://app.example.com/private", nil)
	request.Header.Set("Authorization", "Bearer definitely-not-a-token")
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token.Reveal()})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || !nextCalled {
		t.Fatalf("status=%d next=%v body=%s", recorder.Code, nextCalled, recorder.Body.String())
	}
}

func TestAuthenticateCookieRejectsDuplicateCredentials(t *testing.T) {
	middleware, err := NewMiddleware(ResolverFunc(func(context.Context, Digest) (Session, bool, error) {
		t.Fatal("resolver must not run for duplicate cookies")
		return Session{}, false, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	handler := middleware.AuthenticateCookie(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next must not run")
	}))
	request := httptest.NewRequest(http.MethodGet, "https://app.example.com/private", nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "one"})
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "two"})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAuthenticateCookieAuditsRejectedSession(t *testing.T) {
	t.Parallel()
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	auditor := &securityAuditorStub{}
	middleware, err := NewMiddleware(
		ResolverFunc(func(_ context.Context, digest Digest) (Session, bool, error) {
			if digest != token.Digest() {
				t.Fatalf("digest=%x", digest)
			}
			return Session{}, false, nil
		}),
		WithAuthenticationAuditor(auditor),
	)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://app.example.com/private", nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token.Reveal()})
	recorder := httptest.NewRecorder()
	middleware.AuthenticateCookie(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler called for rejected cookie session")
	})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || len(auditor.events) != 1 {
		t.Fatalf("status=%d events=%+v body=%s", recorder.Code, auditor.events, recorder.Body.String())
	}
	event := auditor.events[0]
	if event.Kind != SecuritySessionRejected || event.Outcome != SecurityOutcomeDenied ||
		event.CredentialDigest != token.Digest() || event.ActorID != (uuid.UUID{}) || event.SubjectID != (uuid.UUID{}) {
		t.Fatalf("event=%+v", event)
	}
}

func TestRequireCSRFProtectsUnsafeMethods(t *testing.T) {
	token, err := newCSRFToken()
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	protected, err := RequireCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusNoContent)
	}))
	if err != nil {
		t.Fatal(err)
	}

	valid := httptest.NewRequest(http.MethodPost, "https://app.example.com/tasks", nil)
	valid.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: token})
	valid.Header.Set(CSRFHeaderName, token)
	validRecorder := httptest.NewRecorder()
	protected.ServeHTTP(validRecorder, valid)
	if validRecorder.Code != http.StatusNoContent || called != 1 {
		t.Fatalf("valid status=%d called=%d", validRecorder.Code, called)
	}

	mismatch := httptest.NewRequest(http.MethodDelete, "https://app.example.com/tasks/1", nil)
	mismatch.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: token})
	other, _ := newCSRFToken()
	mismatch.Header.Set(CSRFHeaderName, other)
	mismatchRecorder := httptest.NewRecorder()
	protected.ServeHTTP(mismatchRecorder, mismatch)
	if mismatchRecorder.Code != http.StatusForbidden || called != 1 {
		t.Fatalf("mismatch status=%d called=%d", mismatchRecorder.Code, called)
	}

	safe := httptest.NewRequest(http.MethodGet, "https://app.example.com/tasks", nil)
	safeRecorder := httptest.NewRecorder()
	protected.ServeHTTP(safeRecorder, safe)
	if safeRecorder.Code != http.StatusNoContent || called != 2 {
		t.Fatalf("safe status=%d called=%d", safeRecorder.Code, called)
	}
}

func TestCookieLogoutRequiresCSRFRevokesAndClearsCookies(t *testing.T) {
	sessionToken, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	csrfToken, err := newCSRFToken()
	if err != nil {
		t.Fatal(err)
	}
	revoker := &revokeStub{}
	handler, err := NewCookieLogoutHandler(revoker)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "https://app.example.com/auth/logout", nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionToken.Reveal()})
	request.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: csrfToken})
	request.Header.Set(CSRFHeaderName, csrfToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || revoker.token.Digest() != sessionToken.Digest() {
		t.Fatalf("status=%d revoked=%v body=%s", recorder.Code, revoker.token, recorder.Body.String())
	}
	cleared := map[string]bool{}
	for _, cookie := range recorder.Result().Cookies() {
		if (cookie.Name == SessionCookieName || cookie.Name == CSRFCookieName) && cookie.MaxAge < 0 {
			cleared[cookie.Name] = true
		}
	}
	if !cleared[SessionCookieName] || !cleared[CSRFCookieName] {
		t.Fatalf("cleared=%v", cleared)
	}

	missingCSRF := httptest.NewRequest(http.MethodPost, "https://app.example.com/auth/logout", nil)
	missingCSRF.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionToken.Reveal()})
	missingRecorder := httptest.NewRecorder()
	handler.ServeHTTP(missingRecorder, missingCSRF)
	if missingRecorder.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d body=%s", missingRecorder.Code, missingRecorder.Body.String())
	}
}

func TestAuditedCookieLogoutRecordsCredentialDigestAndStaysIdempotent(t *testing.T) {
	sessionToken, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	csrfToken, err := newCSRFToken()
	if err != nil {
		t.Fatal(err)
	}
	for _, auditErr := range []error{nil, errors.New("audit unavailable")} {
		revoker := &revokeStub{}
		auditor := &securityAuditorStub{err: auditErr}
		handler, err := NewAuditedCookieLogoutHandler(revoker, auditor)
		if err != nil {
			t.Fatal(err)
		}

		request := httptest.NewRequest(http.MethodPost, "https://app.example.com/auth/logout", nil)
		request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionToken.Reveal()})
		request.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: csrfToken})
		request.Header.Set(CSRFHeaderName, csrfToken)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusNoContent || revoker.token.Digest() != sessionToken.Digest() {
			t.Fatalf("audit err=%v status=%d revoked=%v body=%s", auditErr, recorder.Code, revoker.token, recorder.Body.String())
		}
		if len(auditor.events) != 1 ||
			auditor.events[0].Kind != SecuritySessionRevoked ||
			auditor.events[0].Outcome != SecurityOutcomeSucceeded ||
			auditor.events[0].CredentialDigest != sessionToken.Digest() {
			t.Fatalf("audit err=%v events=%+v", auditErr, auditor.events)
		}
	}
}

func TestCookieLoginReturnsMFAChallengeWithoutSessionCookies(t *testing.T) {
	hash, err := HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := NewMFAChallengeToken()
	if err != nil {
		t.Fatal(err)
	}
	membershipID := uuid.MustNew()
	now := time.Date(2026, 9, 27, 21, 0, 0, 0, time.UTC)
	store := &loginStoreStub{
		found: true,
		identity: PasswordIdentity{
			SubjectID:         uuid.MustNew(),
			MembershipID:      membershipID,
			PasswordHash:      hash,
			CredentialVersion: 7,
			MFARequired:       true,
		},
	}
	sessions := &sessionStub{}
	issuer := &mfaIssuerStub{token: challenge, expiresAt: now.Add(5 * time.Minute)}
	service, err := NewLoginService(
		store,
		sessions,
		WithLoginClock(func() time.Time { return now }),
		WithMFAChallengeIssuer(issuer),
	)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewCookieLoginHandler(service)
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"email":"alice@example.com","password":"secret","tenant":"acme"}`)
	request := httptest.NewRequest(http.MethodPost, "https://app.example.com/auth/browser/login", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://app.example.com")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(recorder.Result().Cookies()) != 0 {
		t.Fatalf("cookies issued before MFA completion: %+v", recorder.Result().Cookies())
	}
	if sessions.membership != (uuid.UUID{}) {
		t.Fatalf("session created before MFA completion: %s", sessions.membership)
	}
	var response mfaRequiredResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.MFARequired || response.ChallengeToken != challenge.Reveal() || !response.ExpiresAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("response=%+v", response)
	}
	if issuer.membershipID != membershipID || issuer.credentialVersion != 7 {
		t.Fatalf("issuer membership=%s version=%d", issuer.membershipID, issuer.credentialVersion)
	}
}

func TestCookieMFACompletionSetsSessionAndCSRFCookies(t *testing.T) {
	now := time.Date(2026, 9, 27, 21, 15, 0, 0, time.UTC)
	challenge, err := NewMFAChallengeToken()
	if err != nil {
		t.Fatal(err)
	}
	backupCodes, _, err := newMFABackupCodes(1)
	if err != nil {
		t.Fatal(err)
	}
	sessionToken, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	membershipID := uuid.MustNew()
	store := &mfaStoreStub{
		challengeFound: true,
		challenge: MFAChallengeRecord{
			MembershipID:      membershipID,
			CredentialVersion: 3,
			ExpiresAt:         now.Add(5 * time.Minute),
			SessionExpiresAt:  now.Add(time.Hour),
		},
		consumeResult:     true,
		consumeMembership: membershipID,
	}
	cipher, err := NewAESGCMSecretCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	sessions := &sessionStub{token: sessionToken}
	service, err := NewMFAService(store, sessions, cipher, WithMFAClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewCookieMFACompletionHandler(service)
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"challenge_token":"` + challenge.Reveal() + `","code":"` + backupCodes[0].Reveal() + `"}`)
	request := httptest.NewRequest(http.MethodPost, "https://app.example.com/auth/browser/mfa", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://app.example.com")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), sessionToken.Reveal()) {
		t.Fatal("cookie MFA completion exposed the bearer session token")
	}
	if sessions.membership != membershipID || sessions.credentialVersion != 3 {
		t.Fatalf("session membership=%s version=%d", sessions.membership, sessions.credentialVersion)
	}
	var response cookieLoginResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.CSRFToken == "" || !response.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("response=%+v", response)
	}
	var sessionCookie, csrfCookie *http.Cookie
	for _, cookie := range recorder.Result().Cookies() {
		switch cookie.Name {
		case SessionCookieName:
			sessionCookie = cookie
		case CSRFCookieName:
			csrfCookie = cookie
		}
	}
	if sessionCookie == nil || sessionCookie.Value != sessionToken.Reveal() || !sessionCookie.Secure || !sessionCookie.HttpOnly {
		t.Fatalf("session cookie=%+v", sessionCookie)
	}
	if csrfCookie == nil || csrfCookie.Value != response.CSRFToken || !csrfCookie.Secure || csrfCookie.HttpOnly {
		t.Fatalf("csrf cookie=%+v", csrfCookie)
	}
}
