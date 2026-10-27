package authhttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/csrf"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// faultBrowser is the two-factor settings fixture over a faultMFAStore. The
// account ada has MFA enabled when enabled is true.
type faultBrowser struct {
	*mfaSettingsFixture
	store    *faultMFAStore
	auditErr error
	auditMu  sync.Mutex
}

func newFaultBrowser(t *testing.T, options ...auth.MFAOption) *faultBrowser {
	t.Helper()
	hash := cachedPasswordHash(t)
	fb := &faultBrowser{}
	f := &mfaSettingsFixture{
		t:   t,
		ada: uuid.MustNew(), bob: uuid.MustNew(),
		now: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC),
	}
	f.accounts = &memoryMFAAccounts{accounts: map[uuid.UUID]*mfaAccount{
		f.ada: {login: "ada@example.com", hash: hash},
		f.bob: {login: "bob@example.com", hash: hash},
	}}
	fb.store = &faultMFAStore{memoryMFAAccounts: f.accounts, fail: map[string]error{}, deny: map[string]bool{}, before: map[string]func(){}}
	cipher, err := auth.NewAESGCMSecretCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	auditor := auth.SecurityAuditorFunc(func(_ context.Context, event auth.SecurityEvent) error {
		fb.auditMu.Lock()
		defer fb.auditMu.Unlock()
		f.events = append(f.events, event)
		return fb.auditErr
	})
	throttler, err := auth.NewMemoryLoginThrottler(auth.DefaultMFAThrottleConfig())
	if err != nil {
		t.Fatal(err)
	}
	options = append([]auth.MFAOption{auth.WithMFAClock(f.clock), auth.WithMFAAuditor(auditor), auth.WithMFAThrottler(throttler)}, options...)
	mfa, err := auth.NewMFAService(fb.store, &sessionStub{}, cipher, options...)
	if err != nil {
		t.Fatal(err)
	}
	login := browserLogin(t, cachedIdentity(t, false), &sessionStub{})
	browser, err := NewBrowser(BrowserOptions{Login: login, Sessions: &browserRevoker{}, MFA: mfa, Passwords: fb.store})
	if err != nil {
		t.Fatal(err)
	}
	f.app = mfaSettingsApp(t, browser)
	f.mfa = mfa
	fb.mfaSettingsFixture = f
	return fb
}

func (fb *faultBrowser) errorOf(recorder *httptest.ResponseRecorder) string {
	return recorder.Header().Get("X-Error")
}

func TestBrowserMFAStepUpFailsClosed(t *testing.T) {
	t.Parallel()
	enrollForm := url.Values{"password": {mfaTestPassword}}

	t.Run("infrastructure failures are not masked", func(t *testing.T) {
		t.Parallel()
		fb := newFaultBrowser(t)
		fb.store.fail["LoadMFAStatus"] = errInjected
		if got := fb.errorOf(fb.post(fb.ada, "/mfa", enrollForm, true)); got != errInjected.Error() {
			t.Errorf("status failure: %q", got)
		}
		delete(fb.store.fail, "LoadMFAStatus")
		fb.store.fail["LookupSubjectPassword"] = errInjected
		if got := fb.errorOf(fb.post(fb.ada, "/mfa", enrollForm, true)); got != errInjected.Error() {
			t.Errorf("password lookup failure: %q", got)
		}
		delete(fb.store.fail, "LookupSubjectPassword")
		fb.store.fail["BeginTOTPEnrollment"] = errInjected
		if got := fb.errorOf(fb.post(fb.ada, "/mfa", enrollForm, true)); got != errInjected.Error() {
			t.Errorf("begin failure: %q", got)
		}
		if fb.store.accounts[fb.ada].pending != nil {
			t.Error("a failed begin left a pending key")
		}
	})

	t.Run("an unauthenticated request and a failing throttler", func(t *testing.T) {
		t.Parallel()
		fb := newFaultBrowser(t)
		if recorder := fb.post(uuid.UUID{}, "/mfa", enrollForm, true); recorder.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated: status=%d", recorder.Code)
		}
		failing := newFaultBrowser(t, auth.WithMFAThrottler(failingThrottler{attemptErr: errInjected}))
		if got := failing.errorOf(failing.post(failing.ada, "/mfa", enrollForm, true)); got != errInjected.Error() {
			t.Errorf("throttler failure: %q", got)
		}
		// A throttler that denies refuses with Retry-After before any check,
		// and the refusal is audited as denied.
		denying := newFaultBrowser(t, auth.WithMFAThrottler(failingThrottler{}))
		recorder := denying.post(denying.ada, "/mfa", enrollForm, true)
		if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "7" {
			t.Errorf("denied: status=%d Retry-After=%q", recorder.Code, recorder.Header().Get("Retry-After"))
		}
		if denying.deniedEvents(auth.SecurityMFAEnrollmentStarted) != 1 {
			t.Errorf("events=%+v", denying.events)
		}
	})

	t.Run("a failing auditor does not turn a refusal into success", func(t *testing.T) {
		t.Parallel()
		fb := newFaultBrowser(t)
		fb.auditErr = errors.New("audit down")
		recorder := fb.post(fb.ada, "/mfa", url.Values{"password": {"wrong password"}}, true)
		if fb.errorOf(recorder) != auth.ErrMFAStepUpInvalid.Error() || fb.accounts.account(fb.ada).pending != nil {
			t.Errorf("status=%d error=%q", recorder.Code, fb.errorOf(recorder))
		}
		if fb.deniedEvents(auth.SecurityMFAEnrollmentStarted) != 1 {
			t.Errorf("the refusal was not attempted to be audited: %+v", fb.events)
		}
	})

	t.Run("a failing throttle reset keeps the completed change", func(t *testing.T) {
		t.Parallel()
		fb := newFaultBrowser(t, auth.WithMFAThrottler(failingThrottler{allowed: true, successErr: errInjected}))
		recorder := fb.post(fb.ada, "/mfa", enrollForm, true)
		if recorder.Code != http.StatusOK || recorder.Header().Get("X-Key") == "" {
			t.Errorf("status=%d error=%q", recorder.Code, fb.errorOf(recorder))
		}
	})

	t.Run("a step-up with no MFA support", func(t *testing.T) {
		t.Parallel()
		login := browserLogin(t, cachedIdentity(t, false), &sessionStub{})
		browser, err := NewBrowser(BrowserOptions{Login: login, Sessions: &browserRevoker{}})
		if err != nil {
			t.Fatal(err)
		}
		if browser.MFASettingsAvailable() {
			t.Fatal("settings must be unavailable without an MFA service")
		}
		app := mfaSettingsApp(t, browser)
		fb := &mfaSettingsFixture{t: t, app: app, ada: uuid.MustNew()}
		recorder := fb.post(fb.ada, "/mfa", enrollForm, true)
		if fb.errorOf(recorder) != auth.ErrMFAUnavailable.Error() {
			t.Errorf("status=%d error=%q", recorder.Code, fb.errorOf(recorder))
		}
		request := httptest.NewRequest(http.MethodGet, browserSite+"/", nil)
		if _, err := browser.MFAStatus(request); !errors.Is(err, auth.ErrMFAUnavailable) {
			t.Errorf("MFAStatus: %v", err)
		}
	})
}

func (f *mfaSettingsFixture) errorOf(recorder *httptest.ResponseRecorder) string {
	return recorder.Header().Get("X-Error")
}

func TestBrowserMFAStatusNeedsAPrincipal(t *testing.T) {
	t.Parallel()
	fb := newFaultBrowser(t)
	browser := fb.browserForStatus(t)
	request := httptest.NewRequest(http.MethodGet, browserSite+"/", nil)
	if _, err := browser.MFAStatus(request); !errors.Is(err, auth.ErrCredentialsRequired) {
		t.Errorf("no principal: %v", err)
	}
	principal := principalOf(t, fb.ada)
	request = request.WithContext(withPrincipal(request.Context(), principal))
	status, err := browser.MFAStatus(request)
	if err != nil || status.Enabled {
		t.Errorf("status=%+v err=%v", status, err)
	}
}

func (fb *faultBrowser) browserForStatus(t *testing.T) *Browser {
	t.Helper()
	login := browserLogin(t, cachedIdentity(t, false), &sessionStub{})
	browser, err := NewBrowser(BrowserOptions{Login: login, Sessions: &browserRevoker{}, MFA: fb.mfa, Passwords: fb.store})
	if err != nil {
		t.Fatal(err)
	}
	return browser
}

func TestBrowserMFAChangesFailClosedAfterTheStepUp(t *testing.T) {
	t.Parallel()

	t.Run("a backup code used up meanwhile refuses regeneration and disable", func(t *testing.T) {
		t.Parallel()
		for _, path := range []string{"POST /mfa/backup_codes", "DELETE /mfa"} {
			fb := newFaultBrowser(t)
			_, backup := fb.enable(fb.ada)
			digest := mustBackupDigest(t, backup[0])
			method := "ReplaceMFABackupCodes"
			if path == "DELETE /mfa" {
				method = "DisableMFA"
			}
			fb.store.before[method] = func() { fb.accounts.account(fb.ada).backupCodes[digest] = true }
			form := url.Values{"password": {mfaTestPassword}, "code": {backup[0]}}
			target := "/mfa/backup_codes"
			if path == "DELETE /mfa" {
				form.Set("_method", "DELETE")
				target = "/mfa"
			}
			recorder := fb.post(fb.ada, target, form, true)
			if fb.errorOf(recorder) != auth.ErrMFAStepUpInvalid.Error() {
				t.Errorf("%s: status=%d error=%q", path, recorder.Code, fb.errorOf(recorder))
			}
			if fb.deniedEvents(map[string]auth.SecurityEventKind{"ReplaceMFABackupCodes": auth.SecurityMFABackupCodesRegenerated, "DisableMFA": auth.SecurityMFADisabled}[method]) != 1 {
				t.Errorf("%s: the refusal was not audited as denied: %+v", path, fb.events)
			}
			if !fb.status(fb.ada).Enabled {
				t.Errorf("%s: MFA was turned off", path)
			}
		}
	})

	t.Run("store failures after the step-up are returned as they are", func(t *testing.T) {
		t.Parallel()
		fb := newFaultBrowser(t)
		key, backup := fb.enable(fb.ada)
		fb.store.fail["ReplaceMFABackupCodes"] = errInjected
		if got := fb.errorOf(fb.post(fb.ada, "/mfa/backup_codes", url.Values{"password": {mfaTestPassword}, "code": {backup[0]}}, true)); got != errInjected.Error() {
			t.Errorf("regenerate failure: %q", got)
		}
		delete(fb.store.fail, "ReplaceMFABackupCodes")
		fb.store.fail["DisableMFA"] = errInjected
		if got := fb.errorOf(fb.post(fb.ada, "/mfa", url.Values{"_method": {"DELETE"}, "password": {mfaTestPassword}, "code": {backup[1]}}, true)); got != errInjected.Error() {
			t.Errorf("disable failure: %q", got)
		}
		delete(fb.store.fail, "DisableMFA")
		// The step-up verifier failing is an error too, not a wrong code.
		fb.store.fail["LoadActiveTOTP"] = errInjected
		if got := fb.errorOf(fb.post(fb.ada, "/mfa/backup_codes", url.Values{"password": {mfaTestPassword}, "code": {fb.code(key)}}, true)); got != errInjected.Error() {
			t.Errorf("verifier failure: %q", got)
		}
		if !fb.status(fb.ada).Enabled || fb.status(fb.ada).BackupCodesRemaining != auth.DefaultMFABackupCodeCount {
			t.Errorf("a failed change changed the account: %+v", fb.status(fb.ada))
		}
	})

	t.Run("confirmation failures", func(t *testing.T) {
		t.Parallel()
		fb := newFaultBrowser(t)
		begun := fb.post(fb.ada, "/mfa", url.Values{"password": {mfaTestPassword}}, true)
		key := begun.Header().Get("X-Key")
		if key == "" {
			t.Fatalf("begin: %q", fb.errorOf(begun))
		}
		confirm := func(code string) *httptest.ResponseRecorder {
			return fb.post(fb.ada, "/mfa/confirm", url.Values{"password": {mfaTestPassword}, "code": {code}}, true)
		}
		// The store failing to confirm is returned as it is.
		fb.store.fail["ConfirmTOTPEnrollment"] = errInjected
		if got := fb.errorOf(confirm(fb.code(key))); got != errInjected.Error() {
			t.Errorf("confirm failure: %q", got)
		}
		delete(fb.store.fail, "ConfirmTOTPEnrollment")

		// A wrong code whose pending key cannot be read again is an error.
		calls := 0
		fb.store.before["LoadPendingTOTP"] = func() {
			calls++
			if calls == 2 {
				fb.store.fail["LoadPendingTOTP"] = errInjected
			}
		}
		if got := fb.errorOf(confirm("000000")); got != errInjected.Error() {
			t.Errorf("pending failure: %q", got)
		}
		delete(fb.store.fail, "LoadPendingTOTP")
		delete(fb.store.before, "LoadPendingTOTP")

		// An expired setup is ErrMFAEnrollmentInvalid, not a retry form.
		fb.advance(11 * time.Minute)
		recorder := confirm("000000")
		if fb.errorOf(recorder) != auth.ErrMFAEnrollmentInvalid.Error() || recorder.Header().Get("X-Key") != "" {
			t.Errorf("expired: status=%d error=%q key=%q", recorder.Code, fb.errorOf(recorder), recorder.Header().Get("X-Key"))
		}
	})
}

// newBrowserWithServices is a Browser over a known account with an MFA
// service and a recovery service, for the sign-in and recovery forms.
func newBrowserWithServices(t *testing.T, store *loginStoreStub, mfa *auth.MFAService, recovery *auth.RecoveryService) (*Browser, http.Handler) {
	t.Helper()
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	login := browserLogin(t, store, &sessionStub{token: token})
	browser, err := NewBrowser(BrowserOptions{Login: login, Sessions: &browserRevoker{}, MFA: mfa, Recovery: recovery})
	if err != nil {
		t.Fatal(err)
	}
	return browser, browserApp(t, browser, csrf.Options{})
}

func TestBrowserFormsFailClosed(t *testing.T) {
	t.Parallel()
	if _, err := NewBrowser(BrowserOptions{Sessions: &browserRevoker{}}); err == nil {
		t.Error("a browser without a login service was accepted")
	}
	login := browserLogin(t, cachedIdentity(t, false), &sessionStub{})
	if _, err := NewBrowser(BrowserOptions{Login: login}); err == nil {
		t.Error("a browser without a session revoker was accepted")
	}

	t.Run("sign-in", func(t *testing.T) {
		t.Parallel()
		store := cachedIdentity(t, false)
		store.lookupErr = errInjected
		_, app := newBrowserWithServices(t, store, nil, nil)
		recorder := submit(app, "/session", url.Values{"email": {"a@example.com"}, "password": {mfaTestPassword}, "tenant": {"acme"}}, true)
		if recorder.Header().Get("X-Error") != errInjected.Error() {
			t.Errorf("store failure: status=%d error=%q", recorder.Code, recorder.Header().Get("X-Error"))
		}
		// A stale MFA challenge cookie is cleared by a complete sign-in.
		_, app = newBrowserWithServices(t, cachedIdentity(t, false), nil, nil)
		stale := &http.Cookie{Name: MFAChallengeCookieName, Value: "stale"}
		recorder = submit(app, "/session", url.Values{"email": {"a@example.com"}, "password": {mfaTestPassword}, "tenant": {"acme"}}, true, stale)
		if cleared := responseCookie(recorder, MFAChallengeCookieName); cleared == nil || cleared.MaxAge >= 0 {
			t.Errorf("stale challenge cookie=%+v", cleared)
		}
	})

	t.Run("sign-out without a verified CSRF token", func(t *testing.T) {
		t.Parallel()
		_, app := newBrowserWithServices(t, cachedIdentity(t, false), nil, nil)
		recorder := submit(app, "/session", url.Values{"_method": {"DELETE"}}, false)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("status=%d", recorder.Code)
		}
	})

	t.Run("MFA completion", func(t *testing.T) {
		t.Parallel()
		// Without an MFA service the form is refused.
		_, app := newBrowserWithServices(t, cachedIdentity(t, false), nil, nil)
		recorder := submit(app, "/session/mfa", url.Values{"code": {"123456"}}, true)
		if recorder.Code != http.StatusInternalServerError {
			t.Errorf("no MFA service: status=%d", recorder.Code)
		}
		if recorder.Header().Get("X-Error") != errBrowserMFANotConfigured.Error() {
			t.Errorf("no MFA service: error=%q", recorder.Header().Get("X-Error"))
		}

		f := newFaultMFA(t)
		token := f.armChallenge()
		_, app = newBrowserWithServices(t, cachedIdentity(t, false), f.service, nil)
		// A malformed challenge cookie is cleared and the user starts over.
		recorder = submit(app, "/session/mfa", url.Values{"code": {"123456"}}, true, &http.Cookie{Name: MFAChallengeCookieName, Value: "malformed"})
		if recorder.Header().Get("X-Error") != ErrMFAChallengeMissing.Error() {
			t.Errorf("malformed cookie: error=%q", recorder.Header().Get("X-Error"))
		}
		if cleared := responseCookie(recorder, MFAChallengeCookieName); cleared == nil || cleared.MaxAge >= 0 {
			t.Errorf("malformed challenge cookie was not cleared: %+v", cleared)
		}
		// An infrastructure failure is not masked as a wrong code.
		f.store.fail["LoadMFAChallenge"] = errInjected
		recorder = submit(app, "/session/mfa", url.Values{"code": {f.code()}}, true, &http.Cookie{Name: MFAChallengeCookieName, Value: token.Reveal()})
		if recorder.Header().Get("X-Error") != errInjected.Error() {
			t.Errorf("store failure: error=%q", recorder.Header().Get("X-Error"))
		}
		delete(f.store.fail, "LoadMFAChallenge")

		// A throttled completion carries Retry-After.
		throttled := newFaultMFA(t, auth.WithMFAThrottler(failingThrottler{}))
		throttledToken := throttled.armChallenge()
		_, app = newBrowserWithServices(t, cachedIdentity(t, false), throttled.service, nil)
		recorder = submit(app, "/session/mfa", url.Values{"code": {throttled.code()}}, true, &http.Cookie{Name: MFAChallengeCookieName, Value: throttledToken.Reveal()})
		if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "7" {
			t.Errorf("throttled: status=%d Retry-After=%q", recorder.Code, recorder.Header().Get("Retry-After"))
		}
	})

	t.Run("password recovery", func(t *testing.T) {
		t.Parallel()
		// Without a recovery service both forms are refused.
		_, app := newBrowserWithServices(t, cachedIdentity(t, false), nil, nil)
		for _, form := range []struct {
			method string
			values url.Values
		}{
			{"POST", url.Values{"email": {"a@example.com"}, "tenant": {"acme"}}},
			{"PATCH", url.Values{"token": {"x"}, "password": {"y"}, "_method": {"PATCH"}}},
		} {
			recorder := submit(app, "/passwords", form.values, true)
			if recorder.Header().Get("X-Error") != errBrowserRecoveryNotConfigured.Error() {
				t.Errorf("%s without recovery: error=%q", form.method, recorder.Header().Get("X-Error"))
			}
		}
		// Without a verified token both fail closed.
		recorder := submit(app, "/passwords", url.Values{"email": {"a@example.com"}, "tenant": {"acme"}}, false)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("request without CSRF: status=%d", recorder.Code)
		}
		recorder = submit(app, "/passwords", url.Values{"_method": {"PATCH"}, "token": {"x"}, "password": {"y"}}, false)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("reset without CSRF: status=%d", recorder.Code)
		}

		build := func(store auth.RecoveryStore, options ...auth.RecoveryOption) http.Handler {
			options = append([]auth.RecoveryOption{auth.WithRecoveryResponseTime(0)}, options...)
			recovery, err := auth.NewRecoveryService(store, &recoverySenderStub{}, options...)
			if err != nil {
				t.Fatal(err)
			}
			_, handler := newBrowserWithServices(t, cachedIdentity(t, false), nil, recovery)
			return handler
		}
		token := mustRecoveryToken(t).Reveal()
		// Throttled and failing requests are errors; Retry-After is set.
		app = build(&recoveryStoreStub{}, auth.WithRecoveryThrottler(failingThrottler{}))
		recorder = submit(app, "/passwords", url.Values{"email": {"a@example.com"}, "tenant": {"acme"}}, true)
		if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "7" {
			t.Errorf("throttled request: status=%d Retry-After=%q", recorder.Code, recorder.Header().Get("Retry-After"))
		}
		recorder = submit(app, "/passwords", url.Values{"_method": {"PATCH"}, "token": {token}, "password": {"a long enough password"}}, true)
		if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "7" {
			t.Errorf("throttled reset: status=%d Retry-After=%q", recorder.Code, recorder.Header().Get("Retry-After"))
		}
		app = build(&recoveryStoreStub{}, auth.WithRecoveryThrottler(failingThrottler{attemptErr: errInjected}))
		recorder = submit(app, "/passwords", url.Values{"email": {"a@example.com"}, "tenant": {"acme"}}, true)
		if recorder.Header().Get("X-Error") != errInjected.Error() {
			t.Errorf("request failure: %q", recorder.Header().Get("X-Error"))
		}
		recorder = submit(app, "/passwords", url.Values{"_method": {"PATCH"}, "token": {token}, "password": {"a long enough password"}}, true)
		if recorder.Header().Get("X-Error") != errInjected.Error() {
			t.Errorf("reset failure: %q", recorder.Header().Get("X-Error"))
		}
		// An empty password is ErrPasswordInvalid, an unusable token is
		// ErrRecoveryInvalid.
		app = build(&recoveryStoreStub{})
		recorder = submit(app, "/passwords", url.Values{"_method": {"PATCH"}, "token": {token}, "password": {""}}, true)
		if recorder.Header().Get("X-Error") != auth.ErrPasswordInvalid.Error() {
			t.Errorf("empty password: %q", recorder.Header().Get("X-Error"))
		}
		recorder = submit(app, "/passwords", url.Values{"_method": {"PATCH"}, "token": {token}, "password": {"a long enough password"}}, true)
		if recorder.Header().Get("X-Error") != auth.ErrRecoveryInvalid.Error() {
			t.Errorf("unusable token: %q", recorder.Header().Get("X-Error"))
		}
		// A committed reset whose audit failed still clears the cookies.
		app = build(&recoveryStoreStub{consumeResult: true}, auth.WithRecoveryAuditor(&securityAuditorStub{err: errors.New("audit down")}))
		recorder = submit(app, "/passwords", url.Values{"_method": {"PATCH"}, "token": {token}, "password": {"a long enough password"}}, true)
		if recorder.Code != http.StatusNoContent || responseCookie(recorder, SessionCookieName) == nil {
			t.Errorf("committed reset: status=%d error=%q", recorder.Code, recorder.Header().Get("X-Error"))
		}
	})
}

func TestMFAChallengeCookieMustBeWellFormed(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, browserSite+"/", nil)
	request.AddCookie(&http.Cookie{Name: MFAChallengeCookieName, Value: "malformed"})
	if _, ok := mfaChallengeCookie(request); ok {
		t.Error("a malformed challenge cookie was accepted")
	}
	token, err := auth.NewMFAChallengeToken()
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, browserSite+"/", nil)
	request.AddCookie(&http.Cookie{Name: MFAChallengeCookieName, Value: token.Reveal()})
	request.AddCookie(&http.Cookie{Name: MFAChallengeCookieName, Value: token.Reveal()})
	if _, ok := mfaChallengeCookie(request); ok {
		t.Error("two challenge cookies were accepted")
	}
	if strings.TrimSpace(token.Reveal()) == "" {
		t.Fatal("empty token")
	}
}
