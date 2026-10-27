package authhttp

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/csrf"
)

// The cookie login, the browser reset form and the browser step-up answer a
// saturated limiter 503 with Retry-After too.
func TestSaturatedHashingAnswers503OnCookieLoginResetAndStepUp(t *testing.T) {
	t.Parallel()
	limiter, err := auth.NewPasswordWorkLimiter(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	saturated := cheapPasswordHasher(t, auth.WithPasswordWorkLimiter(limiter))
	login := browserLogin(t, knownIdentity(t, mfaTestPassword, false), &sessionStub{}, auth.WithLoginPasswordHasher(saturated))
	cookieLogin, err := NewCookieLoginHandler(login)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := auth.NewRecoveryService(&recoveryStoreStub{consumeResult: true}, &recoverySenderStub{}, auth.WithRecoveryPasswordHasher(saturated))
	if err != nil {
		t.Fatal(err)
	}
	resetBrowser, err := NewBrowser(BrowserOptions{Login: login, Sessions: &revokeStub{}, Recovery: recovery})
	if err != nil {
		t.Fatal(err)
	}
	resetApp := browserApp(t, resetBrowser, csrf.Options{})
	settings := newMFASettingsFixture(t, auth.DefaultMFAThrottleConfig())
	stepUpBrowser, err := NewBrowser(BrowserOptions{Login: login, Sessions: &browserRevoker{}, MFA: settings.mfa, Passwords: settings.accounts})
	if err != nil {
		t.Fatal(err)
	}
	settings.app = mfaSettingsApp(t, stepUpBrowser)
	token, err := auth.NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}

	release := holdSlot(t, limiter)
	defer release()
	request := httptest.NewRequest(http.MethodPost, "https://app.example.com/auth/login",
		strings.NewReader(`{"email":"ada@example.com","password":"`+mfaTestPassword+`","tenant":"acme"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://app.example.com")
	cookie := httptest.NewRecorder()
	cookieLogin.ServeHTTP(cookie, request)
	reset := submit(resetApp, "/passwords", url.Values{"_method": {"PATCH"}, "token": {token.Reveal()}, "password": {"replacement password"}}, true)
	stepUp := settings.post(settings.ada, "/mfa", url.Values{"password": {mfaTestPassword}}, true)
	for name, recorder := range map[string]*httptest.ResponseRecorder{"cookie login": cookie, "browser reset": reset, "browser step-up": stepUp} {
		if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "5" {
			t.Errorf("%s: status=%d retry-after=%q body=%s", name, recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
		}
	}
}
