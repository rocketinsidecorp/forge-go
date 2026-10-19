package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/csrf"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/web"
)

const browserSite = "https://app.example.com"

// browserApp mounts Browser the way a generated HTML adapter does: behind
// web.MethodOverride and csrf.Protection, reading the form fields. Outcomes
// are written as headers so tests can assert them without templates.
func browserApp(t *testing.T, browser *Browser, opts csrf.Options) http.Handler {
	t.Helper()
	protection, err := csrf.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	fail := func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("X-Error", err.Error())
		web.Error(w, r, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		step, err := browser.SignIn(w, r, SignInCredentials{
			Email: r.PostFormValue("email"), Password: r.PostFormValue("password"), Tenant: r.PostFormValue("tenant"),
		})
		if err != nil {
			fail(w, r, err)
			return
		}
		w.Header().Set("X-Step", strconv.Itoa(int(step)))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /session/mfa", func(w http.ResponseWriter, r *http.Request) {
		if err := browser.CompleteMFA(w, r, r.PostFormValue("code")); err != nil {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /session", func(w http.ResponseWriter, r *http.Request) {
		if err := browser.SignOut(w, r); err != nil {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /passwords", func(w http.ResponseWriter, r *http.Request) {
		if err := browser.RequestPasswordReset(w, r, r.PostFormValue("email"), r.PostFormValue("tenant")); err != nil {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PATCH /passwords", func(w http.ResponseWriter, r *http.Request) {
		if err := browser.ResetPassword(w, r, r.PostFormValue("token"), r.PostFormValue("password")); err != nil {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return web.MethodOverride(protection.Handler(mux))
}

// submit posts a form with the CSRF cookie and, when withToken, the
// matching authenticity_token field.
func submit(handler http.Handler, path string, values url.Values, withToken bool, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	token, err := csrf.NewToken()
	if err != nil {
		panic(err)
	}
	form := url.Values{}
	for key, value := range values {
		form[key] = value
	}
	if withToken {
		form.Set(csrf.FormField, token)
	}
	request := httptest.NewRequest(http.MethodPost, browserSite+path, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "text/html")
	request.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: token})
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func responseCookie(recorder *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func browserLogin(t *testing.T, store *loginStoreStub, sessions *sessionStub, options ...LoginOption) *LoginService {
	t.Helper()
	service, err := NewLoginService(store, sessions, options...)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func knownIdentity(t *testing.T, password string, mfa bool) *loginStoreStub {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	return &loginStoreStub{found: true, identity: PasswordIdentity{
		SubjectID: uuid.MustNew(), MembershipID: uuid.MustNew(), PasswordHash: hash, CredentialVersion: 1, MFARequired: mfa,
	}}
}

func TestBrowserSignInSetsTheCookieLoginCookiesAndRotatesCSRF(t *testing.T) {
	t.Parallel()
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	sessions := &sessionStub{token: token}
	login := browserLogin(t, knownIdentity(t, "correct horse", false), sessions, WithLoginClock(func() time.Time { return now }))
	browser, err := NewBrowser(BrowserOptions{Login: login, Sessions: &browserRevoker{}})
	if err != nil {
		t.Fatal(err)
	}
	app := browserApp(t, browser, csrf.Options{})

	formToken, err := csrf.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, browserSite+"/session", strings.NewReader(url.Values{
		"email": {" Alice@Example.com "}, "password": {"correct horse"}, "tenant": {"ACME"}, csrf.FormField: {formToken},
	}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: formToken})
	request.RemoteAddr = "203.0.113.7:4000"
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent || recorder.Header().Get("X-Step") != strconv.Itoa(int(SignInComplete)) {
		t.Fatalf("status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control=%q", recorder.Header().Get("Cache-Control"))
	}
	session := responseCookie(recorder, SessionCookieName)
	if session == nil || session.Value != token.Reveal() || !session.Secure || !session.HttpOnly || session.Path != "/" ||
		session.SameSite != http.SameSiteLaxMode || !session.Expires.Equal(now.Add(defaultSessionTTL)) {
		t.Fatalf("session cookie=%+v", session)
	}
	rotated := responseCookie(recorder, CSRFCookieName)
	if rotated == nil || rotated.Value == formToken || rotated.HttpOnly || !rotated.Secure || rotated.SameSite != http.SameSiteLaxMode {
		t.Fatalf("CSRF cookie must be a fresh token: %+v", rotated)
	}
	if _, err := csrf.ParseToken(rotated.Value); err != nil {
		t.Fatalf("rotated CSRF token is not canonical: %v", err)
	}
}

func TestBrowserSignInFailuresAreUniform(t *testing.T) {
	t.Parallel()
	known := knownIdentity(t, "correct horse", false)
	cases := map[string]struct {
		store  *loginStoreStub
		values url.Values
	}{
		"wrong password":   {known, url.Values{"email": {"a@example.com"}, "password": {"wrong"}, "tenant": {"acme"}}},
		"unknown account":  {&loginStoreStub{}, url.Values{"email": {"b@example.com"}, "password": {"correct horse"}, "tenant": {"acme"}}},
		"malformed tenant": {known, url.Values{"email": {"a@example.com"}, "password": {"correct horse"}, "tenant": {"x"}}},
		"empty password":   {known, url.Values{"email": {"a@example.com"}, "password": {""}, "tenant": {"acme"}}},
	}
	var bodies []string
	for name, current := range cases {
		browser, err := NewBrowser(BrowserOptions{Login: browserLogin(t, current.store, &sessionStub{}), Sessions: &browserRevoker{}})
		if err != nil {
			t.Fatal(err)
		}
		recorder := submit(browserApp(t, browser, csrf.Options{}), "/session", current.values, true)
		if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("X-Error") != ErrCredentialsInvalid.Error() {
			t.Fatalf("%s: status=%d error=%q", name, recorder.Code, recorder.Header().Get("X-Error"))
		}
		if responseCookie(recorder, SessionCookieName) != nil || responseCookie(recorder, MFAChallengeCookieName) != nil {
			t.Fatalf("%s: a failed sign-in set credentials", name)
		}
		bodies = append(bodies, recorder.Body.String())
	}
	for _, body := range bodies[1:] {
		if body != bodies[0] {
			t.Fatalf("failure bodies differ:\n%s\n%s", bodies[0], body)
		}
	}
}

func TestBrowserSignInThrottlingSetsRetryAfter(t *testing.T) {
	t.Parallel()
	throttler, err := NewMemoryLoginThrottler(LoginThrottleConfig{Window: time.Minute, AccountLimit: 1, SourceLimit: 100, MaxEntries: 10})
	if err != nil {
		t.Fatal(err)
	}
	browser, err := NewBrowser(BrowserOptions{Login: browserLogin(t, &loginStoreStub{}, &sessionStub{}, WithLoginThrottler(throttler)), Sessions: &browserRevoker{}})
	if err != nil {
		t.Fatal(err)
	}
	app := browserApp(t, browser, csrf.Options{})
	values := url.Values{"email": {"a@example.com"}, "password": {"guess"}, "tenant": {"acme"}}
	if first := submit(app, "/session", values, true); first.Code != http.StatusUnauthorized {
		t.Fatalf("first attempt status=%d", first.Code)
	}
	throttled := submit(app, "/session", values, true)
	retryAfter, err := strconv.Atoi(throttled.Header().Get("Retry-After"))
	if throttled.Code != http.StatusTooManyRequests || err != nil || retryAfter < 1 || retryAfter > 60 {
		t.Fatalf("status=%d Retry-After=%q", throttled.Code, throttled.Header().Get("Retry-After"))
	}
}

// TestBrowserFailsClosedWithoutVerifiedCSRF covers both layers: the
// middleware rejects a form without a token, and Browser itself refuses a
// request the middleware let through without checking a token (an exempted
// route or a JSON body), so a misconfigured route cannot sign anyone in or
// out.
func TestBrowserFailsClosedWithoutVerifiedCSRF(t *testing.T) {
	t.Parallel()
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	revoker := &browserRevoker{}
	recovery, err := NewRecoveryService(&recoveryStoreStub{}, &recoverySenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	browser, err := NewBrowser(BrowserOptions{
		Login:    browserLogin(t, knownIdentity(t, "correct horse", false), &sessionStub{token: token}),
		Sessions: revoker, Recovery: recovery,
	})
	if err != nil {
		t.Fatal(err)
	}
	session := &http.Cookie{Name: SessionCookieName, Value: token.Reveal()}
	signIn := url.Values{"email": {"a@example.com"}, "password": {"correct horse"}, "tenant": {"acme"}}
	requests := []struct {
		path   string
		values url.Values
	}{
		{"/session", signIn},
		{"/session", url.Values{"_method": {"DELETE"}}},
		{"/session/mfa", url.Values{"code": {"123456"}}},
		{"/passwords", url.Values{"email": {"a@example.com"}, "tenant": {"acme"}}},
		{"/passwords", url.Values{"_method": {"PATCH"}, "token": {"x"}, "password": {"new password"}}},
	}
	protected := browserApp(t, browser, csrf.Options{})
	exempted := browserApp(t, browser, csrf.Options{Exempt: []string{"POST /session", "POST /session/mfa", "POST /passwords"}})
	for _, current := range requests {
		if recorder := submit(protected, current.path, current.values, false, session); recorder.Code != http.StatusForbidden {
			t.Errorf("POST %s %v without a token: status=%d", current.path, current.values, recorder.Code)
		}
		// Exempt patterns never cover a form carrying _method, so only the
		// plain POSTs reach Browser here; it must still refuse them.
		if current.values.Has("_method") {
			continue
		}
		recorder := submit(exempted, current.path, current.values, false, session)
		if recorder.Code != http.StatusForbidden || recorder.Header().Get("X-Error") != ErrCSRFInvalid.Error() {
			t.Errorf("exempted POST %s: status=%d error=%q", current.path, recorder.Code, recorder.Header().Get("X-Error"))
		}
		if responseCookie(recorder, SessionCookieName) != nil {
			t.Errorf("exempted POST %s set a session cookie", current.path)
		}
	}
	if revoker.calls != 0 {
		t.Fatalf("a sign-out without a verified token revoked the session %d times", revoker.calls)
	}
}

func TestBrowserSignOutRevokesAndClearsCredentials(t *testing.T) {
	t.Parallel()
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	revoker := &browserRevoker{}
	auditor := &securityAuditorStub{}
	browser, err := NewBrowser(BrowserOptions{Login: browserLogin(t, &loginStoreStub{}, &sessionStub{}), Sessions: revoker, Auditor: auditor})
	if err != nil {
		t.Fatal(err)
	}
	recorder := submit(browserApp(t, browser, csrf.Options{}), "/session", url.Values{"_method": {"DELETE"}}, true,
		&http.Cookie{Name: SessionCookieName, Value: token.Reveal()},
		&http.Cookie{Name: MFAChallengeCookieName, Value: "stale"})
	if recorder.Code != http.StatusNoContent || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v", recorder.Code, recorder.Header())
	}
	if revoker.calls != 1 || revoker.token.Digest() != token.Digest() {
		t.Fatalf("revocations=%d", revoker.calls)
	}
	if len(auditor.events) != 1 || auditor.events[0].Kind != SecuritySessionRevoked || auditor.events[0].CredentialDigest != token.Digest() {
		t.Fatalf("audit events=%+v", auditor.events)
	}
	for _, name := range []string{SessionCookieName, CSRFCookieName, MFAChallengeCookieName} {
		if cookie := responseCookie(recorder, name); cookie == nil || cookie.MaxAge >= 0 || cookie.Value != "" {
			t.Errorf("%s was not cleared: %+v", name, cookie)
		}
	}

	// Without a session it is still a successful, idempotent sign-out.
	if again := submit(browserApp(t, browser, csrf.Options{}), "/session", url.Values{"_method": {"DELETE"}}, true); again.Code != http.StatusNoContent || revoker.calls != 1 {
		t.Fatalf("sign-out without a session: status=%d revocations=%d", again.Code, revoker.calls)
	}

	failing := &browserRevoker{err: errors.New("database down")}
	broken, err := NewBrowser(BrowserOptions{Login: browserLogin(t, &loginStoreStub{}, &sessionStub{}), Sessions: failing})
	if err != nil {
		t.Fatal(err)
	}
	failed := submit(browserApp(t, broken, csrf.Options{}), "/session", url.Values{"_method": {"DELETE"}}, true,
		&http.Cookie{Name: SessionCookieName, Value: token.Reveal()})
	if failed.Code != http.StatusInternalServerError || responseCookie(failed, SessionCookieName) != nil {
		t.Fatalf("failed revocation must keep the cookie and report 500: status=%d", failed.Code)
	}
}

func TestBrowserMFASignInUsesAChallengeCookie(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	challenge, err := NewMFAChallengeToken()
	if err != nil {
		t.Fatal(err)
	}
	sessionToken, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	backupCodes, _, err := newMFABackupCodes(1)
	if err != nil {
		t.Fatal(err)
	}
	store := knownIdentity(t, "correct horse", true)
	issuer := &mfaIssuerStub{token: challenge, expiresAt: now.Add(5 * time.Minute)}
	loginSessions := &sessionStub{}
	login := browserLogin(t, store, loginSessions, WithLoginClock(func() time.Time { return now }), WithMFAChallengeIssuer(issuer))
	mfaStore := &mfaStoreStub{
		challengeFound: true,
		challenge: MFAChallengeRecord{
			MembershipID: store.identity.MembershipID, CredentialVersion: 1,
			ExpiresAt: now.Add(5 * time.Minute), SessionExpiresAt: now.Add(time.Hour),
		},
		consumeResult: true, consumeMembership: store.identity.MembershipID,
	}
	cipher, err := NewAESGCMSecretCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	mfa, err := NewMFAService(mfaStore, &sessionStub{token: sessionToken}, cipher, WithMFAClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	browser, err := NewBrowser(BrowserOptions{Login: login, Sessions: &browserRevoker{}, MFA: mfa})
	if err != nil {
		t.Fatal(err)
	}
	app := browserApp(t, browser, csrf.Options{})

	password := submit(app, "/session", url.Values{"email": {"a@example.com"}, "password": {"correct horse"}, "tenant": {"acme"}}, true)
	if password.Code != http.StatusNoContent || password.Header().Get("X-Step") != strconv.Itoa(int(SignInNeedsMFA)) {
		t.Fatalf("password step status=%d headers=%v", password.Code, password.Header())
	}
	if responseCookie(password, SessionCookieName) != nil || loginSessions.membership != (uuid.UUID{}) {
		t.Fatal("the password step must not create a session")
	}
	pending := responseCookie(password, MFAChallengeCookieName)
	if pending == nil || pending.Value != challenge.Reveal() || !pending.HttpOnly || !pending.Secure ||
		pending.SameSite != http.SameSiteStrictMode || pending.Path != "/" || !pending.Expires.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("challenge cookie=%+v", pending)
	}
	if strings.Contains(password.Body.String(), challenge.Reveal()) {
		t.Fatal("the challenge leaked into the response body")
	}
	pendingRequest := httptest.NewRequest(http.MethodGet, browserSite+"/session/mfa", nil)
	pendingRequest.AddCookie(pending)
	if !browser.MFAPending(pendingRequest) || browser.MFAPending(httptest.NewRequest(http.MethodGet, browserSite+"/session/mfa", nil)) {
		t.Fatal("MFAPending must follow the challenge cookie")
	}

	if missing := submit(app, "/session/mfa", url.Values{"code": {backupCodes[0].Reveal()}}, true); missing.Code != http.StatusUnauthorized ||
		missing.Header().Get("X-Error") != ErrMFAChallengeMissing.Error() {
		t.Fatalf("no challenge: status=%d error=%q", missing.Code, missing.Header().Get("X-Error"))
	}
	completed := submit(app, "/session/mfa", url.Values{"code": {backupCodes[0].Reveal()}}, true, pending)
	if completed.Code != http.StatusNoContent {
		t.Fatalf("completion status=%d body=%s", completed.Code, completed.Body)
	}
	if session := responseCookie(completed, SessionCookieName); session == nil || session.Value != sessionToken.Reveal() || !session.HttpOnly {
		t.Fatalf("session cookie=%+v", session)
	}
	if cleared := responseCookie(completed, MFAChallengeCookieName); cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("challenge cookie not cleared: %+v", cleared)
	}
	if responseCookie(completed, CSRFCookieName) == nil {
		t.Fatal("MFA completion must rotate the CSRF token")
	}

	mfaStore.consumeResult = false
	wrong := submit(app, "/session/mfa", url.Values{"code": {backupCodes[0].Reveal()}}, true, pending)
	if wrong.Code != http.StatusUnauthorized || wrong.Header().Get("X-Error") != ErrMFAInvalid.Error() || responseCookie(wrong, SessionCookieName) != nil {
		t.Fatalf("rejected code: status=%d error=%q", wrong.Code, wrong.Header().Get("X-Error"))
	}
}

// TestBrowserPasswordResetRequestDoesNotEnumerate compares the complete
// responses for a known account, an unknown account, a malformed identity
// and a failed delivery: they must be indistinguishable.
func TestBrowserPasswordResetRequestDoesNotEnumerate(t *testing.T) {
	t.Parallel()
	known := RecoveryIdentity{SubjectID: uuid.MustNew(), Email: "a@example.com"}
	cases := []struct {
		name   string
		store  *recoveryStoreStub
		sender *recoverySenderStub
		values url.Values
	}{
		{"known", &recoveryStoreStub{found: true, identity: known}, &recoverySenderStub{}, url.Values{"email": {"a@example.com"}, "tenant": {"acme"}}},
		{"unknown", &recoveryStoreStub{}, &recoverySenderStub{}, url.Values{"email": {"b@example.com"}, "tenant": {"acme"}}},
		{"malformed", &recoveryStoreStub{}, &recoverySenderStub{}, url.Values{"email": {"nope"}, "tenant": {""}}},
		{"delivery failure", &recoveryStoreStub{found: true, identity: known}, &recoverySenderStub{err: errors.New("smtp down")}, url.Values{"email": {"a@example.com"}, "tenant": {"acme"}}},
	}
	type observed struct {
		status  int
		body    string
		headers string
	}
	var results []observed
	for _, current := range cases {
		recovery, err := NewRecoveryService(current.store, current.sender)
		if err != nil {
			t.Fatal(err)
		}
		browser, err := NewBrowser(BrowserOptions{Login: browserLogin(t, &loginStoreStub{}, &sessionStub{}), Sessions: &browserRevoker{}, Recovery: recovery})
		if err != nil {
			t.Fatal(err)
		}
		recorder := submit(browserApp(t, browser, csrf.Options{}), "/passwords", current.values, true)
		header := recorder.Header().Clone()
		header.Del("Set-Cookie")
		results = append(results, observed{recorder.Code, recorder.Body.String(), strings.Join([]string{header.Get("Cache-Control"), header.Get("Content-Type"), header.Get("Retry-After")}, "|")})
		if current.name == "known" && current.sender.calls != 1 {
			t.Fatalf("the known account received %d messages", current.sender.calls)
		}
	}
	for i, result := range results {
		if result != results[0] || result.status != http.StatusNoContent {
			t.Fatalf("%s answered %+v, %s answered %+v", cases[i].name, result, cases[0].name, results[0])
		}
	}
}

func TestBrowserPasswordReset(t *testing.T) {
	t.Parallel()
	store := &recoveryStoreStub{consumeResult: true}
	recovery, err := NewRecoveryService(store, &recoverySenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	browser, err := NewBrowser(BrowserOptions{Login: browserLogin(t, &loginStoreStub{}, &sessionStub{}), Sessions: &browserRevoker{}, Recovery: recovery})
	if err != nil {
		t.Fatal(err)
	}
	app := browserApp(t, browser, csrf.Options{})
	token, err := NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	reset := submit(app, "/passwords", url.Values{"_method": {"PATCH"}, "token": {token.Reveal()}, "password": {"a new long password"}}, true)
	if reset.Code != http.StatusNoContent || store.consumedDigest != token.Digest() {
		t.Fatalf("status=%d consumed=%x", reset.Code, store.consumedDigest)
	}
	if cleared := responseCookie(reset, SessionCookieName); cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("a reset must clear this browser's session cookie: %+v", cleared)
	}
	if invalid := submit(app, "/passwords", url.Values{"_method": {"PATCH"}, "token": {"forged"}, "password": {"x"}}, true); invalid.Code != http.StatusBadRequest ||
		invalid.Header().Get("X-Error") != ErrRecoveryInvalid.Error() {
		t.Fatalf("forged token: status=%d error=%q", invalid.Code, invalid.Header().Get("X-Error"))
	}
	store.consumeResult = false
	if used := submit(app, "/passwords", url.Values{"_method": {"PATCH"}, "token": {token.Reveal()}, "password": {"another password"}}, true); used.Header().Get("X-Error") != ErrRecoveryInvalid.Error() {
		t.Fatalf("used token error=%q", used.Header().Get("X-Error"))
	}
}

func TestReturnPathAcceptsOnlyLocalPaths(t *testing.T) {
	t.Parallel()
	for candidate, want := range map[string]string{
		"/products?page=2":              "/products?page=2",
		"/":                             "/",
		"":                              "/home",
		"//evil.example/":               "/home",
		"/\\evil.example":               "/home",
		"https://evil.example/":         "/home",
		"javascript:alert(1)":           "/home",
		"products":                      "/home",
		"/\tevil":                       "/home",
		"/café":                         "/home",
		"/" + strings.Repeat("a", 2048): "/home",
	} {
		if got := ReturnPath(candidate, "/home"); got != want {
			t.Errorf("ReturnPath(%q) = %q, want %q", candidate, got, want)
		}
	}
}

func TestAuthenticateBrowserRedirectsOnlyHTMLRequests(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	principal, err := NewPrincipal(uuid.MustNew(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	broken := false
	middleware, err := NewMiddleware(ResolverFunc(func(_ context.Context, digest Digest) (Session, bool, error) {
		if broken {
			return Session{}, false, errors.New("database down")
		}
		return Session{Principal: principal, ExpiresAt: now.Add(time.Hour)}, digest == token.Digest(), nil
	}), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := middleware.AuthenticateBrowser("https://evil.example/login", http.NotFoundHandler()); err == nil {
		t.Fatal("an absolute sign-in URL must be rejected")
	}
	if _, err := middleware.AuthenticateBrowser("/session/new?x=1", http.NotFoundHandler()); err == nil {
		t.Fatal("a sign-in path with a query must be rejected")
	}
	handler, err := middleware.AuthenticateBrowser("/session/new", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := FromContext(r.Context()); !ok {
			t.Error("no principal in context")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	if err != nil {
		t.Fatal(err)
	}
	serve := func(method, target, accept string, cookie *http.Cookie) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, browserSite+target, nil)
		if accept != "" {
			request.Header.Set("Accept", accept)
		}
		if cookie != nil {
			request.AddCookie(cookie)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	const browserAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

	redirected := serve(http.MethodGet, "/products?page=2", browserAccept, nil)
	if redirected.Code != http.StatusSeeOther || redirected.Header().Get("Location") != "/session/new?return_to=%2Fproducts%3Fpage%3D2" {
		t.Fatalf("HTML GET: status=%d Location=%q", redirected.Code, redirected.Header().Get("Location"))
	}
	revoked := serve(http.MethodGet, "/products", browserAccept, &http.Cookie{Name: SessionCookieName, Value: strings.Repeat("A", 43)})
	if revoked.Code != http.StatusSeeOther || !strings.HasPrefix(revoked.Header().Get("Location"), "/session/new?") {
		t.Fatalf("HTML GET with an unknown session: status=%d", revoked.Code)
	}
	if form := serve(http.MethodPost, "/products", browserAccept, nil); form.Code != http.StatusSeeOther || form.Header().Get("Location") != "/session/new" {
		t.Fatalf("HTML POST: status=%d Location=%q", form.Code, form.Header().Get("Location"))
	}
	for _, accept := range []string{"", "application/json", "*/*"} {
		if api := serve(http.MethodGet, "/products", accept, nil); api.Code != http.StatusUnauthorized || !strings.Contains(api.Body.String(), "credentials_required") {
			t.Fatalf("Accept %q: status=%d body=%s", accept, api.Code, api.Body)
		}
	}
	if ok := serve(http.MethodGet, "/products", browserAccept, &http.Cookie{Name: SessionCookieName, Value: token.Reveal()}); ok.Code != http.StatusNoContent {
		t.Fatalf("valid session: status=%d", ok.Code)
	}
	broken = true
	if failed := serve(http.MethodGet, "/products", browserAccept, &http.Cookie{Name: SessionCookieName, Value: token.Reveal()}); failed.Code != http.StatusInternalServerError {
		t.Fatalf("resolver failure must stay 500, got %d", failed.Code)
	}
}

func TestNewBrowserRequiresLoginAndSessions(t *testing.T) {
	t.Parallel()
	login := browserLogin(t, &loginStoreStub{}, &sessionStub{})
	if _, err := NewBrowser(BrowserOptions{Sessions: &browserRevoker{}}); err == nil {
		t.Fatal("missing login service accepted")
	}
	if _, err := NewBrowser(BrowserOptions{Login: login}); err == nil {
		t.Fatal("missing session revoker accepted")
	}
	browser, err := NewBrowser(BrowserOptions{Login: login, Sessions: &browserRevoker{}})
	if err != nil || browser.MFAEnabled() || browser.RecoveryEnabled() {
		t.Fatalf("browser=%+v err=%v", browser, err)
	}
}

type browserRevoker struct {
	token Token
	calls int
	err   error
}

func (s *browserRevoker) RevokeSession(_ context.Context, token Token) error {
	s.token = token
	s.calls++
	return s.err
}
