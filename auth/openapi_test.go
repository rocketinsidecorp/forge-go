package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/csrf"
	"github.com/rocketinsidecorp/forge-go/openapi"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// describedAuthRoutes registers every self-describing auth handler on the
// patterns the generated application uses, with in-memory stores.
func describedAuthRoutes(t *testing.T) (*http.ServeMux, []openapi.Route) {
	t.Helper()
	hash, err := HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	sessionToken, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	passwords := &loginStoreStub{found: true, identity: PasswordIdentity{
		SubjectID: uuid.MustNew(), MembershipID: uuid.MustNew(), PasswordHash: hash, CredentialVersion: 1,
	}}
	sessions := &sessionStub{token: sessionToken}
	throttler, err := newMemoryLoginThrottler(LoginThrottleConfig{Window: time.Minute, AccountLimit: 2, SourceLimit: 100, MaxEntries: 100}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	login, err := NewLoginService(passwords, sessions, WithLoginThrottler(throttler))
	if err != nil {
		t.Fatal(err)
	}

	cipher, err := NewAESGCMSecretCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("12345678901234567890")
	sealed, err := cipher.Seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	membership := uuid.MustNew()
	mfaStore := &mfaStoreStub{
		challengeFound: true,
		challenge: MFAChallengeRecord{
			SubjectID: uuid.MustNew(), MembershipID: membership, CredentialVersion: 1,
			SecretDigest: sha256.Sum256(secret), SecretCiphertext: sealed, LastCounter: -1,
			ExpiresAt: time.Now().Add(5 * time.Minute), SessionExpiresAt: time.Now().Add(time.Hour),
		},
		consumeResult: true, consumeMembership: membership,
	}
	mfa, err := NewMFAService(mfaStore, sessions, cipher)
	if err != nil {
		t.Fatal(err)
	}

	recoveryThrottler, err := newMemoryLoginThrottler(LoginThrottleConfig{Window: time.Minute, AccountLimit: 1, SourceLimit: 100, MaxEntries: 100}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	recoveryStore := &recoveryStoreStub{consumeResult: true}
	recovery, err := NewRecoveryService(recoveryStore, &recoverySenderStub{}, WithRecoveryThrottler(recoveryThrottler))
	if err != nil {
		t.Fatal(err)
	}

	// An account that needs MFA answers 202 with a challenge.
	challenge, err := NewMFAChallengeToken()
	if err != nil {
		t.Fatal(err)
	}
	mfaLogin, err := NewLoginService(
		&loginStoreStub{found: true, identity: PasswordIdentity{
			SubjectID: uuid.MustNew(), MembershipID: uuid.MustNew(), PasswordHash: hash, CredentialVersion: 1, MFARequired: true,
		}},
		sessions,
		WithMFAChallengeIssuer(&mfaIssuerStub{token: challenge, expiresAt: time.Now().Add(5 * time.Minute)}),
	)
	if err != nil {
		t.Fatal(err)
	}

	auditor := &securityAuditorStub{}
	handlers := map[string]func() (http.Handler, error){
		"POST /auth/login":                func() (http.Handler, error) { return NewLoginHandler(login) },
		"POST /auth/logout":               func() (http.Handler, error) { return NewAuditedLogoutHandler(&revokeStub{}, auditor) },
		"POST /auth/mfa/complete":         func() (http.Handler, error) { return NewAuditedMFACompletionHandler(mfa, auditor) },
		"POST /auth/recovery/request":     func() (http.Handler, error) { return NewAuditedPasswordRecoveryRequestHandler(recovery, auditor) },
		"POST /auth/recovery/reset":       func() (http.Handler, error) { return NewAuditedPasswordResetHandler(recovery, auditor) },
		"POST /auth/cookie/login":         func() (http.Handler, error) { return NewCookieLoginHandler(login) },
		"POST /auth/cookie/mfa/complete":  func() (http.Handler, error) { return NewCookieMFACompletionHandler(mfa) },
		"POST /auth/cookie/logout":        func() (http.Handler, error) { return NewAuditedCookieLogoutHandler(&revokeStub{}, auditor) },
		"POST /auth/unaudited/logout":     func() (http.Handler, error) { return NewLogoutHandler(&revokeStub{}) },
		"POST /auth/unaudited/mfa":        func() (http.Handler, error) { return NewMFACompletionHandler(mfa) },
		"POST /auth/unaudited/recover":    func() (http.Handler, error) { return NewPasswordRecoveryRequestHandler(recovery) },
		"POST /auth/unaudited/reset":      func() (http.Handler, error) { return NewPasswordResetHandler(recovery) },
		"POST /auth/mfa/login":            func() (http.Handler, error) { return NewLoginHandler(mfaLogin) },
		"POST /auth/mfa/cookie/login":     func() (http.Handler, error) { return NewCookieLoginHandler(mfaLogin) },
		"POST /auth/unaudited/cookie/out": func() (http.Handler, error) { return NewCookieLogoutHandler(&revokeStub{}) },
	}
	mux := http.NewServeMux()
	var routes []openapi.Route
	for pattern, build := range handlers {
		handler, err := build()
		if err != nil {
			t.Fatalf("%s: %v", pattern, err)
		}
		method, path, _ := strings.Cut(pattern, " ")
		mux.Handle(pattern, handler)
		routes = append(routes, openapi.Route{Method: method, Pattern: path, Handler: handler})
	}
	return mux, routes
}

func buildAuthDocument(t *testing.T) (*openapi.Document, *http.ServeMux) {
	t.Helper()
	mux, routes := describedAuthRoutes(t)
	document, err := openapi.Build(openapi.Info{Title: "auth", Version: "1"}, routes)
	if err != nil {
		t.Fatal(err)
	}
	if undocumented := document.Undocumented(); len(undocumented) != 0 {
		t.Fatalf("auth handlers without a description: %v", undocumented)
	}
	return document, mux
}

func TestAuthHandlersDescribeThemselves(t *testing.T) {
	document, _ := buildAuthDocument(t)
	want := []string{
		"/auth/cookie/login", "/auth/cookie/logout", "/auth/cookie/mfa/complete",
		"/auth/login", "/auth/logout", "/auth/mfa/complete", "/auth/mfa/cookie/login", "/auth/mfa/login",
		"/auth/recovery/request", "/auth/recovery/reset",
		"/auth/unaudited/cookie/out", "/auth/unaudited/logout", "/auth/unaudited/mfa",
		"/auth/unaudited/recover", "/auth/unaudited/reset",
	}
	if got := document.Paths(); !slices.Equal(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
}

// Parameter tags are literals, so they are pinned to the names the handlers
// enforce.
func TestAuthHeaderParametersNameTheHeadersTheHandlersRead(t *testing.T) {
	for _, test := range []struct {
		typ    reflect.Type
		header string
	}{
		{reflect.TypeFor[csrfHeader](), CSRFHeaderName},
		{reflect.TypeFor[csrfHeader](), csrf.HeaderName},
		{reflect.TypeFor[originHeader](), "Origin"},
	} {
		if got := test.typ.Field(0).Tag.Get("header"); got != test.header {
			t.Errorf("%s declares header %q, the handler reads %q", test.typ, got, test.header)
		}
	}
}

// No secret-bearing member may carry an example, however the type is tagged.
func TestAuthDocumentHasNoExamplesForSecrets(t *testing.T) {
	document, _ := buildAuthDocument(t)
	var tree struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(document.JSON(), &tree); err != nil {
		t.Fatal(err)
	}
	secrets := []string{"password", "new_password", "token", "access_token", "challenge_token", "csrf_token", "code"}
	checked := map[string]bool{}
	examples := map[string]bool{}
	for name, schema := range tree.Components.Schemas {
		for property, definition := range schema.Properties {
			if _, has := definition["examples"]; has {
				examples[name+"."+property] = true
			}
			if slices.Contains(secrets, property) {
				checked[property] = true
				if _, has := definition["examples"]; has {
					t.Errorf("%s.%s carries an example", name, property)
				}
				if _, has := definition["default"]; has {
					t.Errorf("%s.%s carries a default", name, property)
				}
			}
		}
	}
	for _, property := range secrets {
		if !checked[property] {
			t.Errorf("no schema member %q was checked; update the list", property)
		}
	}
	if len(examples) == 0 {
		t.Error("expected examples for the login and the tenant slug")
	}
	for member := range examples {
		if !strings.HasSuffix(member, ".email") && !strings.HasSuffix(member, ".tenant") {
			t.Errorf("unexpected example on %s", member)
		}
	}
}

type authScenario struct {
	name    string
	path    string
	headers map[string]string
	cookies []*http.Cookie
	body    string
	// bodyFn, when set, builds the body as the request is sent: a TOTP code
	// computed when the table is built expires before the request under the
	// race detector, where the password hashes before it take a minute.
	bodyFn func() string
	status int
}

// Every answer the handlers give below is checked against the document, and
// its status must be declared by number: an undeclared one would only match
// the generic "default" response.
func TestAuthDescriptionsMatchWhatTheHandlersAnswer(t *testing.T) {
	document, mux := buildAuthDocument(t)
	var tree struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Headers map[string]any `json:"headers"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(document.JSON(), &tree); err != nil {
		t.Fatal(err)
	}
	checked := document.CheckResponses(mux, func(r *http.Request, err error) {
		t.Errorf("%s %s: %v", r.Method, r.URL.Path, err)
	})

	mfaChallenge, err := NewMFAChallengeToken()
	if err != nil {
		t.Fatal(err)
	}
	csrfToken, err := newCSRFToken()
	if err != nil {
		t.Fatal(err)
	}
	sessionToken, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	const jsonType = "application/json"
	login := `{"email":"alice@example.com","password":"secret","tenant":"acme"}`
	wrong := `{"email":"nobody@example.com","password":"wrong","tenant":"acme"}`
	// The stub store accepts any challenge the cipher can open; the code is
	// the current TOTP of its fixed secret.
	validMFA := func() string {
		return `{"challenge_token":"` + mfaChallenge.Reveal() + `","code":"` + totpCode([]byte("12345678901234567890"), uint64(time.Now().Unix()/totpPeriodSeconds)) + `"}`
	}
	origin := map[string]string{"Origin": "https://app.example.com", "Content-Type": jsonType}
	scenarios := []authScenario{
		{name: "login", path: "/auth/login", body: login, headers: map[string]string{"Content-Type": jsonType}, status: http.StatusOK},
		{name: "login bad JSON", path: "/auth/login", body: "{", headers: map[string]string{"Content-Type": jsonType}, status: http.StatusBadRequest},
		{name: "login media type", path: "/auth/login", body: login, headers: map[string]string{"Content-Type": "text/plain"}, status: http.StatusUnsupportedMediaType},
		{name: "login wrong", path: "/auth/login", body: wrong, headers: map[string]string{"Content-Type": jsonType}, status: http.StatusUnauthorized},
		{name: "login wrong again", path: "/auth/login", body: wrong, headers: map[string]string{"Content-Type": jsonType}, status: http.StatusUnauthorized},
		{name: "login throttled", path: "/auth/login", body: wrong, headers: map[string]string{"Content-Type": jsonType}, status: http.StatusTooManyRequests},
		{name: "logout", path: "/auth/logout", headers: map[string]string{"Authorization": "Bearer " + sessionToken.Reveal()}, status: http.StatusNoContent},
		{name: "logout without token", path: "/auth/logout", status: http.StatusUnauthorized},
		{name: "logout malformed", path: "/auth/logout", headers: map[string]string{"Authorization": "Bearer malformed"}, status: http.StatusUnauthorized},
		{name: "login needs MFA", path: "/auth/mfa/login", body: login, headers: map[string]string{"Content-Type": jsonType}, status: http.StatusAccepted},
		{name: "cookie login needs MFA", path: "/auth/mfa/cookie/login", body: login, headers: origin, status: http.StatusAccepted},
		{name: "mfa complete", path: "/auth/unaudited/mfa", bodyFn: validMFA, headers: map[string]string{"Content-Type": jsonType}, status: http.StatusOK},
		{name: "cookie mfa complete", path: "/auth/cookie/mfa/complete", bodyFn: validMFA, headers: origin, status: http.StatusOK},
		{name: "mfa wrong challenge", path: "/auth/unaudited/mfa", body: `{"challenge_token":"x","code":"000000"}`, headers: map[string]string{"Content-Type": jsonType}, status: http.StatusUnauthorized},
		{name: "recovery request", path: "/auth/recovery/request", body: `{"email":"nobody@example.com","tenant":"acme"}`, headers: map[string]string{"Content-Type": jsonType}, status: http.StatusAccepted},
		{name: "recovery throttled", path: "/auth/recovery/request", body: `{"email":"nobody@example.com","tenant":"acme"}`, headers: map[string]string{"Content-Type": jsonType}, status: http.StatusTooManyRequests},
		{name: "reset", path: "/auth/unaudited/reset", body: `{"token":"bad","new_password":"replacement password"}`, headers: map[string]string{"Content-Type": jsonType}, status: http.StatusBadRequest},
		{name: "cookie login", path: "/auth/cookie/login", body: login, headers: origin, status: http.StatusOK},
		{name: "cookie login without origin", path: "/auth/cookie/login", body: login, headers: map[string]string{"Content-Type": jsonType}, status: http.StatusForbidden},
		{name: "cookie login wrong", path: "/auth/cookie/login", body: strings.Replace(wrong, "nobody@", "other@", 1), headers: origin, status: http.StatusUnauthorized},
		{name: "cookie mfa without origin", path: "/auth/cookie/mfa/complete", body: `{"challenge_token":"x","code":"000000"}`, headers: map[string]string{"Content-Type": jsonType}, status: http.StatusForbidden},
		{name: "cookie mfa wrong challenge", path: "/auth/cookie/mfa/complete", body: `{"challenge_token":"x","code":"000000"}`, headers: origin, status: http.StatusUnauthorized},
		{
			name: "cookie logout", path: "/auth/cookie/logout", headers: map[string]string{csrf.HeaderName: csrfToken},
			cookies: []*http.Cookie{{Name: SessionCookieName, Value: sessionToken.Reveal()}, {Name: CSRFCookieName, Value: csrfToken}},
			status:  http.StatusNoContent,
		},
		{name: "cookie logout without CSRF", path: "/auth/cookie/logout", status: http.StatusForbidden},
	}
	for _, scenario := range scenarios {
		body := scenario.body
		if scenario.bodyFn != nil {
			body = scenario.bodyFn()
		}
		request := httptest.NewRequest(http.MethodPost, "https://app.example.com"+scenario.path, bytes.NewReader([]byte(body)))
		request.RemoteAddr = "192.0.2.10:4242"
		for name, value := range scenario.headers {
			request.Header.Set(name, value)
		}
		for _, cookie := range scenario.cookies {
			request.AddCookie(cookie)
		}
		recorder := httptest.NewRecorder()
		checked.ServeHTTP(recorder, request)
		if recorder.Code != scenario.status {
			t.Errorf("%s: status=%d, want %d; body=%s", scenario.name, recorder.Code, scenario.status, recorder.Body.String())
			continue
		}
		operation, ok := tree.Paths[scenario.path]["post"]
		if !ok {
			t.Errorf("%s: %s is not documented", scenario.name, scenario.path)
			continue
		}
		declared, ok := operation.Responses[strconv.Itoa(scenario.status)]
		if !ok {
			t.Errorf("%s: %s does not declare %d (only the default response would match)", scenario.name, scenario.path, scenario.status)
			continue
		}
		// A header the document names for the answer is really sent.
		for header := range declared.Headers {
			if recorder.Header().Get(header) == "" {
				t.Errorf("%s: documented header %s is missing from the %d answer", scenario.name, header, scenario.status)
			}
		}
	}
}
