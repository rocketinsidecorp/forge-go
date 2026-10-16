package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/fabriciobonjorno/forge-go/uuid"
	"github.com/fabriciobonjorno/forge-go/web"
)

type revokeStub struct {
	token Token
	err   error
}

func (s *revokeStub) RevokeSession(_ context.Context, token Token) error {
	s.token = token
	return s.err
}

func TestLoginHandlerReturnsBearerToken(t *testing.T) {
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
	now := time.Date(2026, 9, 27, 13, 0, 0, 0, time.UTC)
	service, err := NewLoginService(store, sessions, WithLoginClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLoginHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"email":"alice@example.com","password":"secret","tenant":"acme"}`)
	request := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("cache headers: Cache-Control=%q Pragma=%q", recorder.Header().Get("Cache-Control"), recorder.Header().Get("Pragma"))
	}
	var response loginResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.AccessToken != token.Reveal() || response.TokenType != "Bearer" || !response.ExpiresAt.Equal(now.Add(defaultSessionTTL)) {
		t.Fatalf("response=%+v", response)
	}
}

func TestLoginHandlerHidesAuthenticationMiss(t *testing.T) {
	service, err := NewLoginService(&loginStoreStub{}, &sessionStub{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLoginHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"email":"nobody@example.com","password":"wrong","tenant":"acme"}`)
	request := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("status=%d authenticate=%q body=%s", recorder.Code, recorder.Header().Get("WWW-Authenticate"), recorder.Body.String())
	}
}

func TestLogoutHandlerIsStrictAndIdempotent(t *testing.T) {
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	revoker := &revokeStub{}
	handler, err := NewLogoutHandler(revoker)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	request.Header.Set("Authorization", "Bearer "+token.Reveal())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || revoker.token.Digest() != token.Digest() {
		t.Fatalf("status=%d revoked=%v", recorder.Code, revoker.token)
	}

	bad := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	bad.Header.Set("Authorization", "Bearer malformed")
	badRecorder := httptest.NewRecorder()
	handler.ServeHTTP(badRecorder, bad)
	if badRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("bad status=%d body=%s", badRecorder.Code, badRecorder.Body.String())
	}
}

func TestLoginHandlerReturnsRetryAfterWhenThrottled(t *testing.T) {
	limiter, err := newMemoryLoginThrottler(LoginThrottleConfig{
		Window:       time.Minute,
		AccountLimit: 1,
		SourceLimit:  10,
		MaxEntries:   100,
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewLoginService(&loginStoreStub{}, &sessionStub{}, WithLoginThrottler(limiter))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLoginHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"email":"nobody@example.com","password":"wrong","tenant":"acme"}`)
	first := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewReader(body))
	first.Header.Set("Content-Type", "application/json")
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("first status=%d body=%s", firstRecorder.Code, firstRecorder.Body.String())
	}

	second := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewReader(body))
	second.Header.Set("Content-Type", "application/json")
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusTooManyRequests || secondRecorder.Header().Get("Retry-After") == "" {
		t.Fatalf("second status=%d retry-after=%q body=%s", secondRecorder.Code, secondRecorder.Header().Get("Retry-After"), secondRecorder.Body.String())
	}
}

func TestRequestSourceUsesPeerAddressNotForwardedHeaders(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	request.RemoteAddr = "203.0.113.7:4321"
	request.Header.Set("X-Forwarded-For", "198.51.100.9")
	if got := requestSource(request); got != "203.0.113.7" {
		t.Fatalf("source=%q", got)
	}
}

func TestRequestSourceUsesValidatedProxyClientAddress(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPost, "/login", nil)
	request.RemoteAddr = "10.0.0.2:4321"
	request = request.WithContext(web.WithProxyInfo(request.Context(), web.ProxyInfo{
		ClientIP: netip.MustParseAddr("198.51.100.9"),
		Scheme:   "https",
	}))
	if got := requestSource(request); got != "198.51.100.9" {
		t.Fatalf("request source=%q, want trusted client address", got)
	}
}

func TestAuditedLogoutRecordsCredentialDigest(t *testing.T) {
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	revoker := &revokeStub{}
	auditor := &securityAuditorStub{}
	handler, err := NewAuditedLogoutHandler(revoker, auditor)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	request.Header.Set("Authorization", "Bearer "+token.Reveal())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(auditor.events) != 1 ||
		auditor.events[0].Kind != SecuritySessionRevoked ||
		auditor.events[0].Outcome != SecurityOutcomeSucceeded ||
		auditor.events[0].CredentialDigest != token.Digest() {
		t.Fatalf("events=%+v", auditor.events)
	}
}

func TestAuditedLogoutStaysIdempotentWhenAuditFails(t *testing.T) {
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	revoker := &revokeStub{}
	auditor := &securityAuditorStub{err: errors.New("audit unavailable")}
	handler, err := NewAuditedLogoutHandler(revoker, auditor)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	request.Header.Set("Authorization", "Bearer "+token.Reveal())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if revoker.token.Digest() != token.Digest() {
		t.Fatalf("revoked=%v", revoker.token)
	}
	if len(auditor.events) != 1 {
		t.Fatalf("audit calls=%d", len(auditor.events))
	}
}
