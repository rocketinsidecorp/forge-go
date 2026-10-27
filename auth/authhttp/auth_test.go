package authhttp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/auth/authhttp"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestMiddlewareAuthenticatesCurrentSessionAndTenant(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	token, principal := fixture(t)
	resolver := auth.ResolverFunc(func(_ context.Context, digest auth.Digest) (auth.Session, bool, error) {
		if digest != token.Digest() {
			return auth.Session{}, false, nil
		}
		return auth.Session{Principal: principal, ExpiresAt: now.Add(time.Hour)}, true, nil
	})
	middleware, err := authhttp.NewMiddleware(resolver, authhttp.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	handler := middleware.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := auth.FromContext(r.Context())
		tenant, tenantErr := tenancy.Require(r.Context())
		if !ok || tenantErr != nil || got.SubjectID() != principal.SubjectID() || tenant != principal.Tenant() {
			t.Errorf("principal=%v ok=%v tenant=%v err=%v", got, ok, tenant, tenantErr)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/private", nil)
	request.Header.Set("Authorization", "Bearer "+token.Reveal())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestMiddlewareRejectsCredentialsFailClosed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	token, principal := fixture(t)
	resolver := auth.ResolverFunc(func(_ context.Context, _ auth.Digest) (auth.Session, bool, error) {
		return auth.Session{Principal: principal, ExpiresAt: now}, true, nil
	})
	middleware, err := authhttp.NewMiddleware(resolver, authhttp.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		headers []string
	}{
		{name: "missing"},
		{name: "wrong scheme", headers: []string{"Basic " + token.Reveal()}},
		{name: "duplicate", headers: []string{"Bearer " + token.Reveal(), "Bearer " + token.Reveal()}},
		{name: "combined", headers: []string{"Bearer " + token.Reveal() + ", Bearer " + token.Reveal()}},
		{name: "expired", headers: []string{"Bearer " + token.Reveal()}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodGet, "/private", nil)
			for _, value := range test.headers {
				request.Header.Add("Authorization", value)
			}
			recorder := httptest.NewRecorder()
			middleware.Authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler called") })).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("WWW-Authenticate") == "" {
				t.Fatalf("status=%d authenticate=%q body=%s", recorder.Code, recorder.Header().Get("WWW-Authenticate"), recorder.Body.String())
			}
		})
	}
}

func TestMiddlewarePreservesResolverFailure(t *testing.T) {
	t.Parallel()
	token, _ := fixture(t)
	resolver := auth.ResolverFunc(func(context.Context, auth.Digest) (auth.Session, bool, error) {
		return auth.Session{}, false, errors.New("database unavailable")
	})
	middleware, err := authhttp.NewMiddleware(resolver)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/private", nil)
	request.Header.Set("Authorization", "Bearer "+token.Reveal())
	recorder := httptest.NewRecorder()
	middleware.Authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler called") })).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRequirePermission(t *testing.T) {
	t.Parallel()
	token, principal := fixture(t)
	resolver := auth.ResolverFunc(func(context.Context, auth.Digest) (auth.Session, bool, error) {
		return auth.Session{Principal: principal, ExpiresAt: time.Now().Add(time.Hour)}, true, nil
	})
	middleware, err := authhttp.NewMiddleware(resolver)
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := authhttp.Require("tasks:read", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	if err != nil {
		t.Fatal(err)
	}
	denied, err := authhttp.Require("tasks:write", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	if err != nil {
		t.Fatal(err)
	}
	for name, handler := range map[string]http.Handler{"allowed": allowed, "denied": denied} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/tasks", nil)
			request.Header.Set("Authorization", "Bearer "+token.Reveal())
			recorder := httptest.NewRecorder()
			middleware.Authenticate(handler).ServeHTTP(recorder, request)
			want := http.StatusNoContent
			if name == "denied" {
				want = http.StatusForbidden
			}
			if recorder.Code != want {
				t.Fatalf("status=%d want=%d body=%s", recorder.Code, want, recorder.Body.String())
			}
		})
	}
}

func TestMiddlewareAuditsRejectedSessionsWithoutChangingUnauthorizedResponse(t *testing.T) {
	t.Parallel()
	token, _ := fixture(t)
	auditor := &recordingAuditor{}
	middleware, err := authhttp.NewMiddleware(
		auth.ResolverFunc(func(context.Context, auth.Digest) (auth.Session, bool, error) {
			return auth.Session{}, false, nil
		}),
		authhttp.WithAuthenticationAuditor(auditor),
	)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/private", nil)
	request.Header.Set("Authorization", "Bearer "+token.Reveal())
	recorder := httptest.NewRecorder()
	middleware.Authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler called for rejected session")
	})).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("status=%d authenticate=%q body=%s", recorder.Code, recorder.Header().Get("WWW-Authenticate"), recorder.Body.String())
	}
	if len(auditor.events) != 1 {
		t.Fatalf("events=%+v", auditor.events)
	}
	event := auditor.events[0]
	if event.Kind != auth.SecuritySessionRejected || event.Outcome != auth.SecurityOutcomeDenied ||
		event.CredentialDigest != token.Digest() || event.ActorID != (uuid.UUID{}) || event.SubjectID != (uuid.UUID{}) ||
		event.MembershipID != (uuid.UUID{}) || event.AccountDigest != (auth.Digest{}) || event.SourceDigest != (auth.Digest{}) {
		t.Fatalf("event=%+v", event)
	}
}

func TestMiddlewareAuditFailureDoesNotChangeUnauthorizedResponse(t *testing.T) {
	t.Parallel()
	token, _ := fixture(t)
	auditor := &recordingAuditor{err: errors.New("audit unavailable")}
	middleware, err := authhttp.NewMiddleware(
		auth.ResolverFunc(func(context.Context, auth.Digest) (auth.Session, bool, error) {
			return auth.Session{}, false, nil
		}),
		authhttp.WithAuthenticationAuditor(auditor),
	)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/private", nil)
	request.Header.Set("Authorization", "Bearer "+token.Reveal())
	recorder := httptest.NewRecorder()
	middleware.Authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler called for rejected session")
	})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || len(auditor.events) != 1 {
		t.Fatalf("status=%d events=%+v body=%s", recorder.Code, auditor.events, recorder.Body.String())
	}
}

func TestRequireAuditsAuthorizationDenialAndPreservesForbiddenResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		auditErr error
	}{
		{name: "audit succeeds"},
		{name: "audit fails", auditErr: errors.New("audit unavailable")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			token, principal := fixture(t)
			auditor := &recordingAuditor{err: test.auditErr}
			middleware, err := authhttp.NewMiddleware(
				auth.ResolverFunc(func(context.Context, auth.Digest) (auth.Session, bool, error) {
					return auth.Session{Principal: principal, ExpiresAt: time.Now().Add(time.Hour)}, true, nil
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			denied, err := authhttp.RequireAudited("tasks:write", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("handler called without permission")
			}), auditor)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/tasks", nil)
			request.Header.Set("Authorization", "Bearer "+token.Reveal())
			recorder := httptest.NewRecorder()
			middleware.Authenticate(denied).ServeHTTP(recorder, request)

			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if len(auditor.events) != 1 {
				t.Fatalf("events=%+v", auditor.events)
			}
			event := auditor.events[0]
			if event.Kind != auth.SecurityAuthorizationDenied || event.Outcome != auth.SecurityOutcomeDenied ||
				event.ActorID != principal.SubjectID() || event.SubjectID != (uuid.UUID{}) ||
				event.MembershipID != (uuid.UUID{}) || event.AccountDigest != (auth.Digest{}) ||
				event.SourceDigest != (auth.Digest{}) || event.CredentialDigest != (auth.Digest{}) {
				t.Fatalf("event=%+v", event)
			}
		})
	}
}

func TestMiddlewareDoesNotAuditMalformedCredential(t *testing.T) {
	t.Parallel()
	auditor := &recordingAuditor{}
	middleware, err := authhttp.NewMiddleware(
		auth.ResolverFunc(func(context.Context, auth.Digest) (auth.Session, bool, error) {
			t.Fatal("resolver called for malformed token")
			return auth.Session{}, false, nil
		}),
		authhttp.WithAuthenticationAuditor(auditor),
	)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/private", nil)
	request.Header.Set("Authorization", "Bearer malformed")
	recorder := httptest.NewRecorder()
	middleware.Authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler called for malformed token")
	})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || len(auditor.events) != 0 {
		t.Fatalf("status=%d events=%+v body=%s", recorder.Code, auditor.events, recorder.Body.String())
	}
}

type recordingAuditor struct {
	events []auth.SecurityEvent
	err    error
}

func (a *recordingAuditor) RecordSecurityEvent(_ context.Context, event auth.SecurityEvent) error {
	a.events = append(a.events, event)
	return a.err
}

func fixture(t *testing.T) (auth.Token, auth.Principal) {
	t.Helper()
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	permission, err := auth.NewPermission("tasks:read")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := auth.NewPrincipal(uuid.MustNew(), tenant, permission)
	if err != nil {
		t.Fatal(err)
	}
	return token, principal
}
