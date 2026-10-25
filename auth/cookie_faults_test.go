package auth

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func secureJSON(h http.Handler, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "https://app.example.com/auth/login", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://app.example.com")
	request.RemoteAddr = "192.0.2.5:999"
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	return recorder
}

func TestCookieHandlersRejectMissingDependencies(t *testing.T) {
	t.Parallel()
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if _, err := NewCookieLoginHandler(nil); err == nil {
		t.Error("cookie login accepted a nil service")
	}
	if _, err := NewCookieMFACompletionHandler(nil); err == nil {
		t.Error("cookie MFA completion accepted a nil service")
	}
	if _, err := RequireCSRF(nil); err == nil {
		t.Error("RequireCSRF accepted a nil handler")
	}
	if _, err := NewCookieLogoutHandler(nil); err == nil {
		t.Error("cookie logout accepted a nil revoker")
	}
	if _, err := NewAuditedCookieLogoutHandler(&revokingSessionStub{}, nil); err == nil {
		t.Error("audited cookie logout accepted a nil auditor")
	}
	middleware, err := NewMiddleware(ResolverFunc(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := middleware.AuthenticateBrowser("/session/new", nil); err == nil {
		t.Error("AuthenticateBrowser accepted a nil handler")
	}
	for _, path := range []string{"", "session/new", "//evil.example", "/a?b=c", "/a#frag"} {
		if _, err := middleware.AuthenticateBrowser(path, next); err == nil {
			t.Errorf("AuthenticateBrowser accepted the sign-in path %q", path)
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("AuthenticateCookie(nil) did not panic")
			}
		}()
		middleware.AuthenticateCookie(nil)
	}()
}

func TestCookieLoginHandlerFailsClosed(t *testing.T) {
	t.Parallel()
	good := `{"email":"alice@example.com","password":"` + mfaTestPassword + `","tenant":"acme"}`
	build := func(store PasswordStore, options ...LoginOption) http.Handler {
		service, err := NewLoginService(store, &sessionStub{}, options...)
		if err != nil {
			t.Fatal(err)
		}
		handler, err := NewCookieLoginHandler(service)
		if err != nil {
			t.Fatal(err)
		}
		return handler
	}
	handler := build(&loginStoreStub{})
	if recorder := secureJSON(handler, "{nope"); recorder.Code != http.StatusBadRequest {
		t.Errorf("malformed body: status=%d", recorder.Code)
	}
	// A refusal is the uniform 401 and sets no cookies.
	recorder := secureJSON(handler, good)
	if recorder.Code != http.StatusUnauthorized || len(recorder.Result().Cookies()) != 0 {
		t.Errorf("refusal: status=%d cookies=%v", recorder.Code, recorder.Result().Cookies())
	}
	// Throttled: 429 with Retry-After, and the audit failure is only logged.
	handler = build(&loginStoreStub{}, WithLoginThrottler(failingThrottler{}), WithSecurityAuditor(&securityAuditorStub{err: errors.New("audit down")}))
	recorder = secureJSON(handler, good)
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "7" {
		t.Errorf("throttled: status=%d Retry-After=%q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
	// An infrastructure failure is a 500 without detail.
	handler = build(&loginStoreStub{lookupErr: errInjected})
	recorder = secureJSON(handler, good)
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), errInjected.Error()) {
		t.Errorf("store failure: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestCookieMFACompletionHandlerFailsClosed(t *testing.T) {
	t.Parallel()
	f := newFaultMFA(t)
	handler, err := NewCookieMFACompletionHandler(f.service)
	if err != nil {
		t.Fatal(err)
	}
	if recorder := secureJSON(handler, "{nope"); recorder.Code != http.StatusBadRequest {
		t.Errorf("malformed body: status=%d", recorder.Code)
	}
	token := f.armChallenge()
	body := `{"challenge_token":"` + token.Reveal() + `","code":"000000"}`
	recorder := secureJSON(handler, body)
	if recorder.Code != http.StatusUnauthorized || len(recorder.Result().Cookies()) != 0 {
		t.Errorf("wrong code: status=%d cookies=%v", recorder.Code, recorder.Result().Cookies())
	}
	throttled := newFaultMFA(t, WithMFAThrottler(failingThrottler{}))
	handler, err = NewCookieMFACompletionHandler(throttled.service)
	if err != nil {
		t.Fatal(err)
	}
	throttledToken := throttled.armChallenge()
	recorder = secureJSON(handler, `{"challenge_token":"`+throttledToken.Reveal()+`","code":"000000"}`)
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "7" {
		t.Errorf("throttled: status=%d Retry-After=%q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
}

func TestCookieLogoutFailsWhenRevocationFails(t *testing.T) {
	t.Parallel()
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	csrfToken, err := newCSRFToken()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewCookieLogoutHandler(&revokingSessionStub{revokeErr: errInjected})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "https://app.example.com/auth/logout", nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token.Reveal()})
	request.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: csrfToken})
	request.Header.Set(CSRFHeaderName, csrfToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("status=%d", recorder.Code)
	}
	// The cookies stay in place so the user can retry.
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.MaxAge < 0 {
			t.Errorf("cookie %s was cleared although revocation failed", cookie.Name)
		}
	}
}

func TestStartCookieSessionRefusesAnMFAChallenge(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	if _, err := startCookieSession(recorder, LoginResult{MFARequired: true}); err == nil {
		t.Fatal("an MFA challenge was turned into a session")
	}
	if len(recorder.Result().Cookies()) != 0 {
		t.Error("cookies were set for an MFA challenge")
	}
}

func TestValidateCSRFRejectsMalformedTokens(t *testing.T) {
	t.Parallel()
	valid, err := newCSRFToken()
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ cookie, header string }{
		"bad cookie token": {"not-a-token", valid},
		"bad header token": {valid, "not-a-token"},
		"comma header":     {valid, valid + "," + valid},
		"empty header":     {valid, ""},
	} {
		request := httptest.NewRequest(http.MethodPost, "https://app.example.com/x", nil)
		request.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tc.cookie})
		if tc.header != "" {
			request.Header.Set(CSRFHeaderName, tc.header)
		}
		if err := validateCSRF(request); !errors.Is(err, ErrCSRFInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
