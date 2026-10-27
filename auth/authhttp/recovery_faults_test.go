package authhttp

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rocketinsidecorp/forge-go/auth"
)

func serveJSON(h http.Handler, method, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "/auth/recovery", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "192.0.2.77:1234"
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	return recorder
}

func TestRecoveryHandlersRejectMissingDependencies(t *testing.T) {
	t.Parallel()
	service, err := auth.NewRecoveryService(&recoveryStoreStub{}, &recoverySenderStub{}, auth.WithRecoveryResponseTime(0))
	if err != nil {
		t.Fatal(err)
	}
	a := &securityAuditorStub{}
	for name, build := range map[string]func() (http.Handler, error){
		"request nil service":       func() (http.Handler, error) { return NewPasswordRecoveryRequestHandler(nil) },
		"reset nil service":         func() (http.Handler, error) { return NewPasswordResetHandler(nil) },
		"audited request nil audit": func() (http.Handler, error) { return NewAuditedPasswordRecoveryRequestHandler(service, nil) },
		"audited request nil svc":   func() (http.Handler, error) { return NewAuditedPasswordRecoveryRequestHandler(nil, a) },
		"audited reset nil audit":   func() (http.Handler, error) { return NewAuditedPasswordResetHandler(service, nil) },
		"audited reset nil svc":     func() (http.Handler, error) { return NewAuditedPasswordResetHandler(nil, a) },
	} {
		if handler, err := build(); err == nil || handler != nil {
			t.Errorf("%s: handler=%v err=%v", name, handler, err)
		}
	}
}

func TestRecoveryHandlersFailClosedWithAndWithoutAudit(t *testing.T) {
	t.Parallel()
	validToken := mustRecoveryToken(t)
	for _, audited := range []bool{false, true} {
		name := "unaudited"
		if audited {
			name = "audited"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			build := func(store auth.RecoveryStore, sender auth.RecoverySender, options ...auth.RecoveryOption) (request, reset http.Handler, auditor *securityAuditorStub) {
				options = append([]auth.RecoveryOption{auth.WithRecoveryResponseTime(0)}, options...)
				service, err := auth.NewRecoveryService(store, sender, options...)
				if err != nil {
					t.Fatal(err)
				}
				if !audited {
					request, err = NewPasswordRecoveryRequestHandler(service)
					if err != nil {
						t.Fatal(err)
					}
					reset, err = NewPasswordResetHandler(service)
					if err != nil {
						t.Fatal(err)
					}
					return request, reset, nil
				}
				auditor = &securityAuditorStub{}
				request, err = NewAuditedPasswordRecoveryRequestHandler(service, auditor)
				if err != nil {
					t.Fatal(err)
				}
				reset, err = NewAuditedPasswordResetHandler(service, auditor)
				if err != nil {
					t.Fatal(err)
				}
				return request, reset, auditor
			}
			good := `{"email":"alice@example.com","tenant":"acme"}`

			// Malformed bodies.
			request, reset, _ := build(knownRecoveryStore(), &recoverySenderStub{})
			if recorder := serveJSON(request, http.MethodPost, "{nope"); recorder.Code != http.StatusBadRequest {
				t.Errorf("request, malformed body: status=%d", recorder.Code)
			}
			if recorder := serveJSON(reset, http.MethodPost, "{nope"); recorder.Code != http.StatusBadRequest {
				t.Errorf("reset, malformed body: status=%d", recorder.Code)
			}

			// A good request is accepted and audited as succeeded.
			request, _, auditor := build(knownRecoveryStore(), &recoverySenderStub{})
			if recorder := serveJSON(request, http.MethodPost, good); recorder.Code != http.StatusAccepted {
				t.Fatalf("request: status=%d", recorder.Code)
			}
			if audited && !hasEvent(auditor, auth.SecurityRecoveryRequested, auth.SecurityOutcomeSucceeded) {
				t.Errorf("no succeeded event: %+v", auditor.events)
			}
			if audited {
				// An auditor that fails is logged and the answer stays.
				auditor.err = errors.New("audit down")
				if recorder := serveJSON(request, http.MethodPost, good); recorder.Code != http.StatusAccepted {
					t.Errorf("failing auditor changed the answer: status=%d", recorder.Code)
				}
			}

			// A delivery failure stays masked as accepted, audited as failed.
			request, _, auditor = build(knownRecoveryStore(), &recoverySenderStub{err: errInjected})
			if recorder := serveJSON(request, http.MethodPost, good); recorder.Code != http.StatusAccepted {
				t.Errorf("delivery failure: status=%d", recorder.Code)
			}
			if audited && !hasEvent(auditor, auth.SecurityRecoveryRequested, auth.SecurityOutcomeFailed) {
				t.Errorf("no failed event: %+v", auditor.events)
			}
			if audited {
				auditor.err = errors.New("audit down")
				if recorder := serveJSON(request, http.MethodPost, good); recorder.Code != http.StatusAccepted {
					t.Errorf("failing auditor on delivery failure: status=%d", recorder.Code)
				}
			}

			// An infrastructure failure is a 500, never masked.
			request, _, _ = build(knownRecoveryStore(), &recoverySenderStub{}, auth.WithRecoveryThrottler(failingThrottler{attemptErr: errInjected}))
			if recorder := serveJSON(request, http.MethodPost, good); recorder.Code != http.StatusInternalServerError {
				t.Errorf("throttler failure: status=%d", recorder.Code)
			}

			// A reset with a good token succeeds with 204 and is audited.
			store := &recoveryStoreStub{consumeResult: true}
			_, reset, auditor = build(store, &recoverySenderStub{})
			resetBody := `{"token":"` + validToken.Reveal() + `","new_password":"a long enough password"}`
			if recorder := serveJSON(reset, http.MethodPost, resetBody); recorder.Code != http.StatusNoContent {
				t.Fatalf("reset: status=%d", recorder.Code)
			}
			if audited && !hasEvent(auditor, auth.SecurityPasswordReset, auth.SecurityOutcomeSucceeded) {
				t.Errorf("no succeeded reset event: %+v", auditor.events)
			}
			if audited {
				auditor.err = errors.New("audit down")
				if recorder := serveJSON(reset, http.MethodPost, resetBody); recorder.Code != http.StatusNoContent {
					t.Errorf("failing auditor changed the reset answer: status=%d", recorder.Code)
				}
			}

			// An invalid token is a 4xx and an audited failure.
			store = &recoveryStoreStub{consumeResult: false}
			_, reset, auditor = build(store, &recoverySenderStub{})
			if recorder := serveJSON(reset, http.MethodPost, resetBody); recorder.Code != http.StatusBadRequest {
				t.Errorf("invalid token: status=%d", recorder.Code)
			}
			if audited && !hasEvent(auditor, auth.SecurityPasswordReset, auth.SecurityOutcomeFailed) {
				t.Errorf("no failed reset event: %+v", auditor.events)
			}
			if audited {
				auditor.err = errors.New("audit down")
				if recorder := serveJSON(reset, http.MethodPost, resetBody); recorder.Code != http.StatusBadRequest {
					t.Errorf("failing auditor on a failed reset: status=%d", recorder.Code)
				}
			}

			// A throttled reset carries Retry-After.
			_, reset, _ = build(&recoveryStoreStub{}, &recoverySenderStub{}, auth.WithRecoveryThrottler(failingThrottler{allowed: false}))
			recorder := serveJSON(reset, http.MethodPost, resetBody)
			if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "7" {
				t.Errorf("throttled reset: status=%d Retry-After=%q", recorder.Code, recorder.Header().Get("Retry-After"))
			}

			// A reset whose audit failed after it committed still answers 204.
			if !audited {
				store = &recoveryStoreStub{consumeResult: true}
				_, reset, _ = build(store, &recoverySenderStub{}, auth.WithRecoveryAuditor(&securityAuditorStub{err: errors.New("audit down")}))
				if recorder := serveJSON(reset, http.MethodPost, resetBody); recorder.Code != http.StatusNoContent {
					t.Errorf("committed reset with failed audit: status=%d", recorder.Code)
				}
			}
		})
	}
}
