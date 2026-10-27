package authhttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestLoginHandlerFailsClosed(t *testing.T) {
	t.Parallel()
	if _, err := NewLoginHandler(nil); err == nil {
		t.Error("a nil service was accepted")
	}
	// A login that fails and whose audit append also fails is still the
	// uniform 401, not a 500.
	service, err := auth.NewLoginService(&loginStoreStub{}, &sessionStub{}, auth.WithSecurityAuditor(&securityAuditorStub{err: errors.New("audit down")}))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLoginHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"email":"alice@example.com","password":"` + mfaTestPassword + `","tenant":"acme"}`
	if recorder := serveJSON(handler, http.MethodPost, body); recorder.Code != http.StatusUnauthorized {
		t.Errorf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := serveJSON(handler, http.MethodPost, "{nope"); recorder.Code != http.StatusBadRequest {
		t.Errorf("malformed body: status=%d", recorder.Code)
	}
	// An infrastructure failure is a 500 with no detail.
	service, _ = auth.NewLoginService(&loginStoreStub{lookupErr: errInjected}, &sessionStub{})
	handler, _ = NewLoginHandler(service)
	recorder := serveJSON(handler, http.MethodPost, body)
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), errInjected.Error()) {
		t.Errorf("store failure: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestLogoutHandlerConstructionAndRevocationFailure(t *testing.T) {
	t.Parallel()
	if _, err := NewLogoutHandler(nil); err == nil {
		t.Error("a nil revoker was accepted")
	}
	if _, err := NewAuditedLogoutHandler(&revokingSessionStub{}, nil); err == nil {
		t.Error("a nil auditor was accepted")
	}
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLogoutHandler(&revokingSessionStub{revokeErr: errInjected})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	request.Header.Set("Authorization", "Bearer "+token.Reveal())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("revocation failure: status=%d", recorder.Code)
	}
}

func TestRetryAfterHeaderRoundsUpToWholeSeconds(t *testing.T) {
	t.Parallel()
	for in, want := range map[time.Duration]string{
		0: "1", time.Nanosecond: "1", time.Second: "1", 1500 * time.Millisecond: "2", 90 * time.Second: "90", -time.Second: "1",
	} {
		if got := retryAfterHeader(in); got != want {
			t.Errorf("retryAfterHeader(%v)=%q, want %q", in, got, want)
		}
	}
}

func TestMiddlewareConstructionAndFailureModes(t *testing.T) {
	t.Parallel()
	resolver := auth.ResolverFunc(func(context.Context, auth.Digest) (auth.Session, bool, error) { return auth.Session{}, false, nil })
	if _, err := NewMiddleware(nil); err == nil {
		t.Error("a nil resolver was accepted")
	}
	for name, option := range map[string]Option{
		"nil option": nil, "nil clock": WithClock(nil), "nil auditor": WithAuthenticationAuditor(nil),
	} {
		if _, err := NewMiddleware(resolver, option); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	middleware, err := NewMiddleware(resolver)
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("Authenticate(nil) did not panic")
			}
		}()
		middleware.Authenticate(nil)
	}()

	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	valid, err := auth.NewPrincipal(uuid.MustNew(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	var withoutTenant bool
	call := func(session auth.Session, found bool, resolveErr error) int {
		middleware, err := NewMiddleware(auth.ResolverFunc(func(context.Context, auth.Digest) (auth.Session, bool, error) {
			return session, found, resolveErr
		}), WithClock(func() time.Time { return now }))
		if err != nil {
			t.Fatal(err)
		}
		if withoutTenant {
			// A principal whose tenant is missing, which only auth could build.
			middleware.tenantOf = func(auth.Principal) tenancy.Tenant { return tenancy.Tenant{} }
		}
		handler := middleware.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := auth.FromContext(r.Context()); !ok {
				t.Error("the principal is missing from the context")
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("Authorization", "Bearer "+token.Reveal())
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}
	if code := call(auth.Session{Principal: valid, ExpiresAt: now.Add(time.Hour)}, true, nil); code != http.StatusNoContent {
		t.Errorf("valid session: status=%d", code)
	}
	if code := call(auth.Session{}, false, errInjected); code != http.StatusInternalServerError {
		t.Errorf("resolver failure: status=%d", code)
	}
	if code := call(auth.Session{Principal: valid, ExpiresAt: now}, true, nil); code != http.StatusUnauthorized {
		t.Errorf("expired session: status=%d", code)
	}
	// A resolver returning a principal without a valid subject, or one
	// without a tenant, is a server fault, never an authenticated request.
	if code := call(auth.Session{Principal: auth.Principal{}, ExpiresAt: now.Add(time.Hour)}, true, nil); code != http.StatusInternalServerError {
		t.Errorf("invalid principal: status=%d", code)
	}
	withoutTenant = true
	if code := call(auth.Session{Principal: valid, ExpiresAt: now.Add(time.Hour)}, true, nil); code != http.StatusInternalServerError {
		t.Errorf("principal without tenant: status=%d", code)
	}
}

func TestRequireValidatesAndRefusesUnauthenticatedRequests(t *testing.T) {
	t.Parallel()
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if _, err := Require("BAD", next); err == nil {
		t.Error("an invalid permission was accepted")
	}
	if _, err := Require("reports:read", nil); err == nil {
		t.Error("a nil handler was accepted")
	}
	if _, err := RequireAudited("reports:read", next, nil); err == nil {
		t.Error("a nil auditor was accepted")
	}
	handler, err := Require("reports:read", next)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: status=%d", recorder.Code)
	}
	if recorder.Header().Get("WWW-Authenticate") == "" {
		t.Error("a 401 must carry WWW-Authenticate")
	}
}
