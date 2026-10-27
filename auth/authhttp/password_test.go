package authhttp

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/csrf"
	"github.com/rocketinsidecorp/forge-go/web"
)

// A saturated limiter answers the JSON and browser sign-ins 503 with
// Retry-After, for a known and an unknown account alike, never 401/422, so a
// full limiter tells nothing about the credentials.
func TestSaturatedPasswordHashingAnswers503WithRetryAfter(t *testing.T) {
	t.Parallel()
	limiter, err := auth.NewPasswordWorkLimiter(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	hasher := cheapPasswordHasher(t, auth.WithPasswordWorkLimiter(limiter))
	known, err := cheapPasswordHasher(t).Hash(context.Background(), "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	service, err := auth.NewLoginService(newRehashStore(t, known), &sessionStub{}, auth.WithLoginPasswordHasher(hasher))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLoginHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	browser, err := NewBrowser(BrowserOptions{Login: service, Sessions: &revokeStub{}})
	if err != nil {
		t.Fatal(err)
	}
	app := browserApp(t, browser, csrf.Options{})
	release := holdSlot(t, limiter)
	defer release()

	for _, email := range []string{"ada@example.com", "nobody@example.com"} {
		body := `{"email":"` + email + `","password":"correct horse","tenant":"acme"}`
		request := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "5" ||
			!strings.Contains(recorder.Body.String(), "authentication_unavailable") {
			t.Errorf("JSON sign-in of %s: status=%d retry-after=%q body=%s", email, recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
		}

		form := url.Values{"email": {email}, "password": {"correct horse"}, "tenant": {"acme"}}
		recorder = submit(app, "/session", form, true)
		if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "5" {
			t.Errorf("browser sign-in of %s: status=%d retry-after=%q", email, recorder.Code, recorder.Header().Get("Retry-After"))
		}
	}
}

// A reset writes the new password with the recovery service's hasher, so it
// is Argon2id, and a saturated limiter answers 503 with Retry-After.
func TestPasswordResetHashesThroughTheRecoveryHasher(t *testing.T) {
	t.Parallel()
	token, err := auth.NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := auth.NewPasswordWorkLimiter(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	store := &recoveryStoreStub{consumeResult: true}
	service, err := auth.NewRecoveryService(store, &recoverySenderStub{}, auth.WithRecoveryPasswordHasher(cheapPasswordHasher(t, auth.WithPasswordWorkLimiter(limiter))))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewPasswordResetHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	reset := func() *httptest.ResponseRecorder {
		body := `{"token":"` + token.Reveal() + `","new_password":"replacement password"}`
		request := httptest.NewRequest(http.MethodPost, "/auth/reset-password", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	release := holdSlot(t, limiter)
	if recorder := reset(); recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "5" || store.consumedHash != "" {
		t.Fatalf("saturated reset: status=%d retry-after=%q consumed=%q", recorder.Code, recorder.Header().Get("Retry-After"), store.consumedHash)
	}
	release()
	if recorder := reset(); recorder.Code != http.StatusNoContent || !strings.HasPrefix(store.consumedHash, "$argon2id$v=19$") {
		t.Fatalf("reset: status=%d hash=%q", recorder.Code, store.consumedHash)
	}
	if _, err := auth.NewRecoveryService(store, &recoverySenderStub{}, auth.WithRecoveryPasswordHasher(nil)); err == nil {
		t.Fatal("nil hasher accepted")
	}
}

// A stored Argon2id hash in a binary with no Argon2id implementation
// registered (auth.ErrPasswordAlgorithmUnavailable, which auth's tests
// produce from a real hasher) is a deployment error: the JSON and browser
// sign-ins answer 503 without Retry-After, never 401/422, and the request
// log names the fix.
func TestUnavailablePasswordAlgorithmAnswers503AndLogsTheFix(t *testing.T) {
	t.Parallel()
	store := &loginStoreStub{lookupErr: auth.ErrPasswordAlgorithmUnavailable}
	service, err := auth.NewLoginService(store, &sessionStub{}, auth.WithLoginPasswordHasher(cheapPasswordHasher(t)))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLoginHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	body := `{"email":"ada@example.com","password":"correct horse","tenant":"acme"}`
	request := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	request = request.WithContext(web.WithLogger(request.Context(), logger))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "" ||
		!strings.Contains(recorder.Body.String(), "password_algorithm_unavailable") || strings.Contains(recorder.Body.String(), "argon2") {
		t.Fatalf("JSON sign-in: status=%d retry-after=%q body=%s", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
	if !strings.Contains(logs.String(), "auth.RegisterArgon2id(argon2.IDKey)") || !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("the request log does not name the fix: %s", logs.String())
	}

	browser, err := NewBrowser(BrowserOptions{Login: service, Sessions: &revokeStub{}})
	if err != nil {
		t.Fatal(err)
	}
	app := browserApp(t, browser, csrf.Options{})
	form := url.Values{"email": {"ada@example.com"}, "password": {"correct horse"}, "tenant": {"acme"}}
	recorder = submit(app, "/session", form, true)
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "" {
		t.Fatalf("browser sign-in: status=%d retry-after=%q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
}
