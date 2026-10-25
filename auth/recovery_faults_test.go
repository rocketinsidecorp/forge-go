package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errInjected }

func TestRecoveryTokenEntropyFailuresAndRedaction(t *testing.T) {
	t.Parallel()
	if _, err := newRecoveryTokenWithEntropy(nil); err == nil {
		t.Error("a nil entropy source was accepted")
	}
	if _, err := newRecoveryTokenWithEntropy(failingReader{}); !errors.Is(err, errInjected) {
		t.Errorf("failing entropy: %v", err)
	}
	token, err := NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	if got := token.LogValue().String(); got != "[REDACTED]" {
		t.Errorf("LogValue=%q", got)
	}
}

func TestRecoveryServiceConstructionValidatesItsOptions(t *testing.T) {
	t.Parallel()
	store, sender := &recoveryStoreStub{}, &recoverySenderStub{}
	if _, err := NewRecoveryService(nil, sender); err == nil {
		t.Error("a nil store was accepted")
	}
	if _, err := NewRecoveryService(store, nil); err == nil {
		t.Error("a nil sender was accepted")
	}
	for name, option := range map[string]RecoveryOption{
		"nil option":    nil,
		"short ttl":     WithRecoveryTTL(time.Second),
		"long ttl":      WithRecoveryTTL(48 * time.Hour),
		"nil clock":     WithRecoveryClock(nil),
		"nil throttler": WithRecoveryThrottler(nil),
		"nil auditor":   WithRecoveryAuditor(nil),
		"nil dispatch":  WithRecoveryDispatcher(nil),
		"nil logger":    WithRecoveryLogger(nil),
		"long response": WithRecoveryResponseTime(time.Minute),
		"neg response":  WithRecoveryResponseTime(-time.Second),
	} {
		if _, err := NewRecoveryService(store, sender, option); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	var sent PasswordRecoveryMessage
	fn := RecoverySenderFunc(func(_ context.Context, message PasswordRecoveryMessage) error {
		sent = message
		return errInjected
	})
	if err := fn.SendPasswordRecovery(context.Background(), PasswordRecoveryMessage{Email: "a@example.com"}); !errors.Is(err, errInjected) || sent.Email != "a@example.com" {
		t.Errorf("RecoverySenderFunc: sent=%+v err=%v", sent, err)
	}
	if got := (&RecoveryThrottledError{}).Error(); got != ErrRecoveryThrottled.Error() {
		t.Errorf("throttled message=%q", got)
	}
	if err := (&RecoveryDeliveryError{}).Unwrap(); !errors.Is(err, ErrRecoveryDelivery) {
		t.Errorf("a delivery error without a cause must still be ErrRecoveryDelivery: %v", err)
	}
}

func TestRecoveryServiceFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	newService := func(store RecoveryStore, options ...RecoveryOption) *RecoveryService {
		options = append([]RecoveryOption{WithRecoveryResponseTime(0)}, options...)
		service, err := NewRecoveryService(store, &recoverySenderStub{}, options...)
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	request := RecoveryRequest{Email: "alice@example.com", TenantSlug: "acme", Source: "192.0.2.1"}

	// The throttler failing is an error, whoever asks.
	service := newService(knownRecoveryStore(), WithRecoveryThrottler(failingThrottler{attemptErr: errInjected}))
	if err := service.Request(ctx, request); !errors.Is(err, errInjected) {
		t.Errorf("request throttler failure: %v", err)
	}
	if err := service.Reset(ctx, PasswordReset{Token: mustRecoveryToken(t).Reveal(), NewPassword: "a long enough password"}); !errors.Is(err, errInjected) {
		t.Errorf("reset throttler failure: %v", err)
	}

	// Process: a job that names no account is a no-op; store failures and an
	// invalid identity are errors that issue nothing.
	store := knownRecoveryStore()
	service = newService(store)
	if err := service.Process(ctx, RecoveryJob{Email: "not an email", TenantSlug: "acme"}); err != nil {
		t.Errorf("malformed job: %v", err)
	}
	if store.lookupEmail != "" {
		t.Error("a malformed job reached the store")
	}
	store.lookupErr = errInjected
	if err := service.Process(ctx, RecoveryJob{Email: "alice@example.com", TenantSlug: "acme"}); !errors.Is(err, errInjected) {
		t.Errorf("lookup failure: %v", err)
	}
	store.lookupErr = nil
	for name, identity := range map[string]RecoveryIdentity{
		"nil subject": {Email: "alice@example.com"},
		"no email":    {SubjectID: uuid.MustNew()},
	} {
		store.identity = identity
		if err := service.Process(ctx, RecoveryJob{Email: "alice@example.com", TenantSlug: "acme"}); err == nil || !strings.Contains(err.Error(), "invalid identity") {
			t.Errorf("%s: %v", name, err)
		}
		if store.createdDigest != (Digest{}) {
			t.Errorf("%s: a token was issued", name)
		}
	}
	store = knownRecoveryStore()
	store.createErr = errInjected
	service = newService(store)
	if err := service.Process(ctx, RecoveryJob{Email: "alice@example.com", TenantSlug: "acme"}); !errors.Is(err, errInjected) {
		t.Errorf("create failure: %v", err)
	}

	// A dispatcher failing for another reason than a full queue is returned.
	service = newService(knownRecoveryStore(), WithRecoveryDispatcher(&dispatcherStub{err: errInjected}))
	if err := service.Request(ctx, request); !errors.Is(err, errInjected) {
		t.Errorf("dispatch failure: %v", err)
	}
}

func mustRecoveryToken(t *testing.T) RecoveryToken {
	t.Helper()
	token, err := NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestRecoveryResetFailsClosedOnStoreAndPasswordErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token := mustRecoveryToken(t)
	auditor := &securityAuditorStub{}
	newService := func(store RecoveryStore) *RecoveryService {
		service, err := NewRecoveryService(store, &recoverySenderStub{}, WithRecoveryResponseTime(0), WithRecoveryAuditor(auditor))
		if err != nil {
			t.Fatal(err)
		}
		return service
	}

	// The usability check failing is an error, and an unusable token is
	// invalid, audited as denied, without hashing the password.
	checker := &tokenCheckerStore{err: errInjected}
	if err := newService(checker).Reset(ctx, PasswordReset{Token: token.Reveal(), NewPassword: "a long enough password"}); !errors.Is(err, errInjected) {
		t.Errorf("checker failure: %v", err)
	}

	// An empty or oversized password is a typed refusal, audited as denied.
	store := &recoveryStoreStub{consumeResult: true}
	for name, password := range map[string]string{"empty": "", "too long": strings.Repeat("x", maxPasswordBytes+1)} {
		auditor.events = nil
		err := newService(store).Reset(ctx, PasswordReset{Token: token.Reveal(), NewPassword: password})
		if !errors.Is(err, ErrPasswordInvalid) {
			t.Errorf("%s password: %v", name, err)
		}
		if !hasEvent(auditor, SecurityPasswordReset, SecurityOutcomeDenied) {
			t.Errorf("%s password: no denied event: %+v", name, auditor.events)
		}
		if store.consumedDigest != (Digest{}) {
			t.Errorf("%s password: the token was consumed", name)
		}
	}

	// The store failing to consume is an error.
	store = &recoveryStoreStub{consumeErr: errInjected}
	if err := newService(store).Reset(ctx, PasswordReset{Token: token.Reveal(), NewPassword: "a long enough password"}); !errors.Is(err, errInjected) {
		t.Errorf("consume failure: %v", err)
	}
	// A malformed token is invalid before any throttling or store work.
	if err := newService(store).Reset(ctx, PasswordReset{Token: "nope", NewPassword: "a long enough password"}); !errors.Is(err, ErrRecoveryInvalid) {
		t.Errorf("malformed token: %v", err)
	}
}

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
	service, err := NewRecoveryService(&recoveryStoreStub{}, &recoverySenderStub{}, WithRecoveryResponseTime(0))
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
			build := func(store RecoveryStore, sender RecoverySender, options ...RecoveryOption) (request, reset http.Handler, auditor *securityAuditorStub) {
				options = append([]RecoveryOption{WithRecoveryResponseTime(0)}, options...)
				service, err := NewRecoveryService(store, sender, options...)
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
			if audited && !hasEvent(auditor, SecurityRecoveryRequested, SecurityOutcomeSucceeded) {
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
			if audited && !hasEvent(auditor, SecurityRecoveryRequested, SecurityOutcomeFailed) {
				t.Errorf("no failed event: %+v", auditor.events)
			}
			if audited {
				auditor.err = errors.New("audit down")
				if recorder := serveJSON(request, http.MethodPost, good); recorder.Code != http.StatusAccepted {
					t.Errorf("failing auditor on delivery failure: status=%d", recorder.Code)
				}
			}

			// An infrastructure failure is a 500, never masked.
			request, _, _ = build(knownRecoveryStore(), &recoverySenderStub{}, WithRecoveryThrottler(failingThrottler{attemptErr: errInjected}))
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
			if audited && !hasEvent(auditor, SecurityPasswordReset, SecurityOutcomeSucceeded) {
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
			if audited && !hasEvent(auditor, SecurityPasswordReset, SecurityOutcomeFailed) {
				t.Errorf("no failed reset event: %+v", auditor.events)
			}
			if audited {
				auditor.err = errors.New("audit down")
				if recorder := serveJSON(reset, http.MethodPost, resetBody); recorder.Code != http.StatusBadRequest {
					t.Errorf("failing auditor on a failed reset: status=%d", recorder.Code)
				}
			}

			// A throttled reset carries Retry-After.
			_, reset, _ = build(&recoveryStoreStub{}, &recoverySenderStub{}, WithRecoveryThrottler(failingThrottler{allowed: false}))
			recorder := serveJSON(reset, http.MethodPost, resetBody)
			if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "7" {
				t.Errorf("throttled reset: status=%d Retry-After=%q", recorder.Code, recorder.Header().Get("Retry-After"))
			}

			// A reset whose audit failed after it committed still answers 204.
			if !audited {
				store = &recoveryStoreStub{consumeResult: true}
				_, reset, _ = build(store, &recoverySenderStub{}, WithRecoveryAuditor(&securityAuditorStub{err: errors.New("audit down")}))
				if recorder := serveJSON(reset, http.MethodPost, resetBody); recorder.Code != http.StatusNoContent {
					t.Errorf("committed reset with failed audit: status=%d", recorder.Code)
				}
			}
		})
	}
}
