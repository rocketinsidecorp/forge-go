package authhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/web"
)

type revokeStub struct {
	token auth.Token
	err   error
}

func (s *revokeStub) RevokeSession(_ context.Context, token auth.Token) error {
	s.token = token
	return s.err
}

func TestLoginHandlerReturnsBearerToken(t *testing.T) {
	hash, err := auth.HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	store := &loginStoreStub{
		found: true,
		identity: auth.PasswordIdentity{
			SubjectID: uuid.MustNew(), MembershipID: uuid.MustNew(), PasswordHash: hash,
			CredentialVersion: 1,
		},
	}
	sessions := &sessionStub{token: token}
	now := time.Date(2026, 9, 27, 13, 0, 0, 0, time.UTC)
	service, err := auth.NewLoginService(store, sessions, auth.WithLoginClock(func() time.Time { return now }))
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
	service, err := auth.NewLoginService(&loginStoreStub{}, &sessionStub{})
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
	token, err := auth.NewToken()
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
	limiter, err := auth.NewMemoryLoginThrottler(auth.LoginThrottleConfig{
		Window:       time.Minute,
		AccountLimit: 1,
		SourceLimit:  10,
		MaxEntries:   100,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := auth.NewLoginService(&loginStoreStub{}, &sessionStub{}, auth.WithLoginThrottler(limiter))
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

func TestAuditedLogoutRecordsCredentialDigest(t *testing.T) {
	token, err := auth.NewToken()
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
		auditor.events[0].Kind != auth.SecuritySessionRevoked ||
		auditor.events[0].Outcome != auth.SecurityOutcomeSucceeded ||
		auditor.events[0].CredentialDigest != token.Digest() {
		t.Fatalf("events=%+v", auditor.events)
	}
}

func TestAuditedLogoutStaysIdempotentWhenAuditFails(t *testing.T) {
	token, err := auth.NewToken()
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

// TestSourceThrottleBehindTrustedProxyCountsEachClient is the regression
// test for forge-go#11: behind a trusted proxy every client used to share
// the proxy's source counter, so a few hundred attempts from anyone refused
// every sign-in. With the proxy trusted (ADR 0037) each client has its own
// counter, and a client cannot escape its counter with X-Forwarded-For.
func TestSourceThrottleBehindTrustedProxyCountsEachClient(t *testing.T) {
	limiter, err := auth.NewMemoryLoginThrottler(auth.LoginThrottleConfig{
		Window:       time.Minute,
		AccountLimit: 100,
		SourceLimit:  2,
		MaxEntries:   100,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := auth.NewLoginService(&loginStoreStub{}, &sessionStub{}, auth.WithLoginThrottler(limiter))
	if err != nil {
		t.Fatal(err)
	}
	login, err := NewLoginHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := web.NewTrustedProxies(netip.MustParsePrefix("172.18.0.0/16"))
	if err != nil {
		t.Fatal(err)
	}
	handler := trusted.Middleware(login)
	attempt := 0
	try := func(remote, forwardedFor string) int {
		attempt++
		// A different login every time: only the source counter can refuse.
		body := fmt.Sprintf(`{"email":"user%d@example.com","password":"wrong","tenant":"acme"}`, attempt)
		request := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.RemoteAddr = remote
		if forwardedFor != "" {
			request.Header.Set("X-Forwarded-For", forwardedFor)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}
	const proxy = "172.18.0.2:41000"

	// Client A, through the proxy, uses up its source budget.
	for range 2 {
		if code := try(proxy, "203.0.113.10"); code != http.StatusUnauthorized {
			t.Fatalf("client A attempt: %d", code)
		}
	}
	if code := try(proxy, "203.0.113.10"); code != http.StatusTooManyRequests {
		t.Fatalf("client A over its source limit: %d, want 429", code)
	}
	// Client B, through the same proxy, is not affected.
	if code := try(proxy, "203.0.113.20"); code != http.StatusUnauthorized {
		t.Fatalf("client B behind the same proxy: %d, want 401 (independent counter)", code)
	}
	// Client A cannot get a fresh counter: an appending proxy puts A's
	// real address last, and entries A wrote to its left are never read.
	if code := try(proxy, "198.51.100.99, 203.0.113.10"); code != http.StatusTooManyRequests {
		t.Fatalf("client A spoofing a prefix: %d, want 429", code)
	}
	// Nor by reaching the application directly with a forged header: the
	// peer is not a trusted proxy, so the header is ignored.
	if code := try("203.0.113.10:5000", "198.51.100.98"); code != http.StatusTooManyRequests {
		t.Fatalf("client A direct with a forged header: %d, want 429", code)
	}
	// And IPv6 clients are counted per /64 behind the proxy too.
	for range 2 {
		if code := try(proxy, "2001:db8:1:2::1"); code != http.StatusUnauthorized {
			t.Fatalf("IPv6 client attempt: %d", code)
		}
	}
	if code := try(proxy, "2001:db8:1:2::ffff"); code != http.StatusTooManyRequests {
		t.Fatalf("another address of the same /64: %d, want 429", code)
	}
}
