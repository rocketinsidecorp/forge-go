package authhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func principalOf(t *testing.T, subject uuid.UUID) auth.Principal {
	t.Helper()
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	principal, err := auth.NewPrincipal(subject, tenant)
	if err != nil {
		t.Fatal(err)
	}
	return principal
}

// serveMFA runs h for principal (nil: unauthenticated) with an optional JSON
// body.
func serveMFA(h http.Handler, principal *auth.Principal, body string) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	request := httptest.NewRequest(http.MethodPost, "/auth/mfa", reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.RemoteAddr = "192.0.2.10:4242"
	if principal != nil {
		request = request.WithContext(withPrincipal(request.Context(), *principal))
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	return recorder
}

func statusOf(err error) int { return fault.From(err).Status() }

// armChallenge gives the account a factor and a live sign-in challenge, and
// returns the challenge token.
func (f *faultMFA) armChallenge() auth.MFAChallengeToken {
	f.t.Helper()
	f.enroll()
	token, err := auth.NewMFAChallengeToken()
	if err != nil {
		f.t.Fatal(err)
	}
	f.store.challengeFound = true
	f.store.challenge = auth.MFAChallengeRecord{
		SubjectID: f.subject, MembershipID: f.store.membership, CredentialVersion: 1,
		SecretDigest: digestOf(f.secret), SecretCiphertext: f.store.accounts[f.subject].factor.SecretCiphertext,
		LastCounter: -1, ExpiresAt: f.now.Add(time.Minute), SessionExpiresAt: f.now.Add(time.Hour),
	}
	return token
}

func allowAll() auth.MFAChangeAuthorizer {
	return auth.MFAChangeAuthorizerFunc(func(context.Context, auth.Principal) error { return nil })
}

func denyAll(err error) auth.MFAChangeAuthorizer {
	return auth.MFAChangeAuthorizerFunc(func(context.Context, auth.Principal) error { return err })
}

func hasEvent(auditor *securityAuditorStub, kind auth.SecurityEventKind, outcome auth.SecurityOutcome) bool {
	for _, event := range auditor.events {
		if event.Kind == kind && event.Outcome == outcome {
			return true
		}
	}
	return false
}

func TestMFAHandlerConstructorsRejectMissingDependencies(t *testing.T) {
	t.Parallel()
	f := newFaultMFA(t)
	s, a := f.service, &securityAuditorStub{}
	enrollAuth := auth.MFAEnrollmentAuthorizerFunc(func(context.Context, auth.Principal) error { return nil })
	change := allowAll()
	build := map[string]func() (http.Handler, error){
		"enroll nil service":         func() (http.Handler, error) { return NewTOTPEnrollmentHandler(nil, "Forge", enrollAuth) },
		"enroll bad issuer":          func() (http.Handler, error) { return NewTOTPEnrollmentHandler(s, " ", enrollAuth) },
		"enroll control issuer":      func() (http.Handler, error) { return NewTOTPEnrollmentHandler(s, "a\nb", enrollAuth) },
		"confirm nil":                func() (http.Handler, error) { return NewTOTPConfirmationHandler(nil) },
		"completion nil":             func() (http.Handler, error) { return NewMFACompletionHandler(nil) },
		"rotation nil service":       func() (http.Handler, error) { return NewTOTPRotationHandler(nil, "Forge", change) },
		"rotation bad issuer":        func() (http.Handler, error) { return NewTOTPRotationHandler(s, "", change) },
		"rotation confirm nil":       func() (http.Handler, error) { return NewTOTPRotationConfirmationHandler(nil, change) },
		"rotation confirm nil auth":  func() (http.Handler, error) { return NewTOTPRotationConfirmationHandler(s, nil) },
		"disable nil service":        func() (http.Handler, error) { return NewMFADisableHandler(nil, change) },
		"audited enroll nil auditor": func() (http.Handler, error) { return NewAuditedTOTPEnrollmentHandler(s, "Forge", enrollAuth, nil) },
		"audited enroll nil service": func() (http.Handler, error) { return NewAuditedTOTPEnrollmentHandler(nil, "Forge", enrollAuth, a) },
		"audited enroll nil auth":    func() (http.Handler, error) { return NewAuditedTOTPEnrollmentHandler(s, "Forge", nil, a) },
		"audited enroll bad issuer":  func() (http.Handler, error) { return NewAuditedTOTPEnrollmentHandler(s, "\x00", enrollAuth, a) },
		"audited confirm nil auditor": func() (http.Handler, error) {
			return NewAuditedTOTPConfirmationHandler(s, nil)
		},
		"audited confirm nil service": func() (http.Handler, error) { return NewAuditedTOTPConfirmationHandler(nil, a) },
		"audited completion nil auditor": func() (http.Handler, error) {
			return NewAuditedMFACompletionHandler(s, nil)
		},
		"audited completion nil service": func() (http.Handler, error) { return NewAuditedMFACompletionHandler(nil, a) },
		"audited rotation nil auditor": func() (http.Handler, error) {
			return NewAuditedTOTPRotationHandler(s, "Forge", change, nil)
		},
		"audited rotation nil service": func() (http.Handler, error) {
			return NewAuditedTOTPRotationHandler(nil, "Forge", change, a)
		},
		"audited rotation bad issuer": func() (http.Handler, error) {
			return NewAuditedTOTPRotationHandler(s, "", change, a)
		},
		"audited rotation confirm nil auditor": func() (http.Handler, error) {
			return NewAuditedTOTPRotationConfirmationHandler(s, change, nil)
		},
		"audited rotation confirm nil auth": func() (http.Handler, error) {
			return NewAuditedTOTPRotationConfirmationHandler(s, nil, a)
		},
		"audited disable nil auditor": func() (http.Handler, error) { return NewAuditedMFADisableHandler(s, change, nil) },
		"audited disable nil auth":    func() (http.Handler, error) { return NewAuditedMFADisableHandler(s, nil, a) },
	}
	for name, construct := range build {
		handler, err := construct()
		if err == nil || handler != nil {
			t.Errorf("%s: handler=%v err=%v, want a constructor error", name, handler, err)
		}
	}
}

// handlerRig builds the audited and unaudited form of one JSON MFA handler.
type handlerRig struct {
	name  string
	build func(f *faultMFA, auditor auth.SecurityAuditor, authorizer auth.MFAChangeAuthorizer) (http.Handler, error)
	// body is the request body; "" sends none.
	body func(f *faultMFA) string
	// prepare readies the fixture so the service call would succeed.
	prepare func(f *faultMFA)
	// failStore is the store method whose failure makes the service call fail.
	failStore string
	// started, applied: the audit kinds of this handler.
	kind auth.SecurityEventKind
}

func confirmBody(f *faultMFA) string {
	pending := f.store.accounts[f.subject].pending
	secret, err := f.cipher.Open(pending.SecretCiphertext)
	if err != nil {
		f.t.Fatal(err)
	}
	body, err := json.Marshal(totpConfirmationRequest{Code: totpCode(secret, uint64(f.now.Unix()/totpPeriodSeconds))})
	if err != nil {
		f.t.Fatal(err)
	}
	return string(body)
}

func rigs() []handlerRig {
	enrollAuth := func(a auth.MFAChangeAuthorizer) auth.MFAEnrollmentAuthorizer {
		return auth.MFAEnrollmentAuthorizerFunc(func(ctx context.Context, p auth.Principal) error {
			return a.AuthorizeMFAChange(ctx, p)
		})
	}
	return []handlerRig{
		{
			name: "enroll",
			build: func(f *faultMFA, a auth.SecurityAuditor, auth auth.MFAChangeAuthorizer) (http.Handler, error) {
				if a == nil {
					return NewTOTPEnrollmentHandler(f.service, "Forge", enrollAuth(auth))
				}
				return NewAuditedTOTPEnrollmentHandler(f.service, "Forge", enrollAuth(auth), a)
			},
			body:      func(*faultMFA) string { return "" },
			prepare:   func(*faultMFA) {},
			failStore: "BeginTOTPEnrollment",
			kind:      auth.SecurityMFAEnrollmentStarted,
		},
		{
			name: "confirm",
			build: func(f *faultMFA, a auth.SecurityAuditor, _ auth.MFAChangeAuthorizer) (http.Handler, error) {
				if a == nil {
					return NewTOTPConfirmationHandler(f.service)
				}
				return NewAuditedTOTPConfirmationHandler(f.service, a)
			},
			body: confirmBody,
			prepare: func(f *faultMFA) {
				if _, err := f.service.BeginTOTPEnrollment(context.Background(), f.subject, "Forge", "ada"); err != nil {
					f.t.Fatal(err)
				}
			},
			failStore: "ConfirmTOTPEnrollment",
			kind:      auth.SecurityMFAEnabled,
		},
		{
			name: "rotate",
			build: func(f *faultMFA, a auth.SecurityAuditor, auth auth.MFAChangeAuthorizer) (http.Handler, error) {
				if a == nil {
					return NewTOTPRotationHandler(f.service, "Forge", auth)
				}
				return NewAuditedTOTPRotationHandler(f.service, "Forge", auth, a)
			},
			body:      func(*faultMFA) string { return "" },
			prepare:   func(f *faultMFA) { f.enroll() },
			failStore: "BeginTOTPRotation",
			kind:      auth.SecurityMFARotationStarted,
		},
		{
			name: "rotate confirm",
			build: func(f *faultMFA, a auth.SecurityAuditor, auth auth.MFAChangeAuthorizer) (http.Handler, error) {
				if a == nil {
					return NewTOTPRotationConfirmationHandler(f.service, auth)
				}
				return NewAuditedTOTPRotationConfirmationHandler(f.service, auth, a)
			},
			body: confirmBody,
			prepare: func(f *faultMFA) {
				f.enroll()
				// The rotation to confirm, begun with a current code.
				proof, err := f.service.VerifyMFAStepUp(context.Background(), f.subject, f.code())
				if err != nil {
					f.t.Fatal(err)
				}
				if _, err := f.service.BeginTOTPRotation(context.Background(), f.subject, "Forge", "ada", proof); err != nil {
					f.t.Fatal(err)
				}
			},
			failStore: "ConfirmTOTPRotation",
			kind:      auth.SecurityMFARotated,
		},
		{
			name: "disable",
			build: func(f *faultMFA, a auth.SecurityAuditor, auth auth.MFAChangeAuthorizer) (http.Handler, error) {
				if a == nil {
					return NewMFADisableHandler(f.service, auth)
				}
				return NewAuditedMFADisableHandler(f.service, auth, a)
			},
			body:      func(*faultMFA) string { return "" },
			prepare:   func(f *faultMFA) { f.enroll() },
			failStore: "DisableMFA",
			kind:      auth.SecurityMFADisabled,
		},
	}
}

func (r handlerRig) usesAuthorizer() bool { return r.name != "confirm" }
func (r handlerRig) hasBody() bool        { return r.name == "confirm" || r.name == "rotate confirm" }

func TestMFAJSONHandlersAuditAndFailClosed(t *testing.T) {
	t.Parallel()
	for _, rig := range rigs() {
		for _, audited := range []bool{false, true} {
			name := rig.name + "/unaudited"
			if audited {
				name = rig.name + "/audited"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				newRig := func(authorizer auth.MFAChangeAuthorizer) (*faultMFA, *securityAuditorStub, http.Handler) {
					f := newFaultMFA(t)
					rig.prepare(f)
					var auditor *securityAuditorStub
					var asAuditor auth.SecurityAuditor
					if audited {
						auditor = &securityAuditorStub{}
						asAuditor = auditor
					}
					handler, err := rig.build(f, asAuditor, authorizer)
					if err != nil {
						t.Fatal(err)
					}
					return f, auditor, handler
				}
				principalFor := func(f *faultMFA) *auth.Principal {
					p := principalOf(t, f.subject)
					return &p
				}

				// Success.
				f, auditor, handler := newRig(allowAll())
				body := rig.body(f)
				recorder := serveMFA(handler, principalFor(f), body)
				if recorder.Code != http.StatusOK && recorder.Code != http.StatusNoContent {
					t.Fatalf("success status=%d body=%s", recorder.Code, recorder.Body.String())
				}
				if recorder.Header().Get("Cache-Control") != "no-store" {
					t.Errorf("Cache-Control=%q", recorder.Header().Get("Cache-Control"))
				}
				if audited && !hasEvent(auditor, rig.kind, auth.SecurityOutcomeSucceeded) {
					t.Errorf("no succeeded %s event: %+v", rig.kind, auditor.events)
				}

				// Unauthenticated.
				_, _, handler = newRig(allowAll())
				if recorder := serveMFA(handler, nil, ""); recorder.Code != statusOf(auth.ErrCredentialsRequired) {
					t.Errorf("unauthenticated status=%d", recorder.Code)
				}

				// The step-up authorizer refuses.
				if rig.usesAuthorizer() {
					f, auditor, handler = newRig(denyAll(auth.ErrPermissionDenied))
					recorder = serveMFA(handler, principalFor(f), rig.body(f))
					if recorder.Code != statusOf(auth.ErrPermissionDenied) {
						t.Errorf("refused status=%d", recorder.Code)
					}
					if audited && len(auditor.events) != 0 {
						t.Errorf("a refused step-up was audited as an attempt: %+v", auditor.events)
					}
				}

				// A malformed body.
				if rig.hasBody() {
					f, _, handler = newRig(allowAll())
					if recorder := serveMFA(handler, principalFor(f), "{not json"); recorder.Code != http.StatusBadRequest {
						t.Errorf("malformed body status=%d", recorder.Code)
					}
				}

				// The store fails: 500, and the attempt is audited as failed.
				f, auditor, handler = newRig(allowAll())
				body = rig.body(f)
				f.store.fail[rig.failStore] = errInjected
				recorder = serveMFA(handler, principalFor(f), body)
				if recorder.Code != http.StatusInternalServerError {
					t.Errorf("store failure status=%d body=%s", recorder.Code, recorder.Body.String())
				}
				if strings.Contains(recorder.Body.String(), errInjected.Error()) {
					t.Errorf("the response leaks the internal error: %s", recorder.Body.String())
				}
				if audited && !hasEvent(auditor, rig.kind, auth.SecurityOutcomeFailed) {
					t.Errorf("no failed %s event: %+v", rig.kind, auditor.events)
				}

				// The handler's auditor failing never changes the answer: the
				// operation had already committed.
				if audited {
					f, auditor, handler = newRig(allowAll())
					auditor.err = errors.New("audit down")
					recorder = serveMFA(handler, principalFor(f), rig.body(f))
					if recorder.Code != http.StatusOK && recorder.Code != http.StatusNoContent {
						t.Errorf("audit failure changed the answer: status=%d", recorder.Code)
					}
					f, auditor, handler = newRig(allowAll())
					auditor.err = errors.New("audit down")
					body = rig.body(f)
					f.store.fail[rig.failStore] = errInjected
					if recorder := serveMFA(handler, principalFor(f), body); recorder.Code != http.StatusInternalServerError {
						t.Errorf("failed operation with failing auditor: status=%d", recorder.Code)
					}
				}

				// The service's own audit failing after the commit still
				// answers success on the plain handlers (docs/AUDIT.md): the
				// operation was applied and its one-time codes must reach the
				// client. (The audited handlers audit by themselves and are
				// meant for a service without WithMFAAuditor.)
				if !audited {
					f, _, handler = newRig(allowAll())
					f.auditErr = errors.New("service audit down")
					recorder = serveMFA(handler, principalFor(f), rig.body(f))
					if recorder.Code != http.StatusOK && recorder.Code != http.StatusNoContent {
						t.Errorf("applied operation with failed audit: status=%d body=%s", recorder.Code, recorder.Body.String())
					}
				}
			})
		}
	}
}

func TestMFACompletionHandlersFailClosed(t *testing.T) {
	t.Parallel()
	for _, audited := range []bool{false, true} {
		name := "unaudited"
		if audited {
			name = "audited"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			build := func(f *faultMFA) (http.Handler, *securityAuditorStub) {
				if !audited {
					handler, err := NewMFACompletionHandler(f.service)
					if err != nil {
						t.Fatal(err)
					}
					return handler, nil
				}
				auditor := &securityAuditorStub{}
				handler, err := NewAuditedMFACompletionHandler(f.service, auditor)
				if err != nil {
					t.Fatal(err)
				}
				return handler, auditor
			}
			bodyOf := func(token auth.MFAChallengeToken, code string) string {
				body, err := json.Marshal(mfaCompletionRequest{ChallengeToken: token.Reveal(), Code: code})
				if err != nil {
					t.Fatal(err)
				}
				return string(body)
			}

			// A malformed body.
			f := newFaultMFA(t)
			handler, _ := build(f)
			if recorder := serveMFA(handler, nil, "{not json"); recorder.Code != http.StatusBadRequest {
				t.Errorf("malformed body status=%d", recorder.Code)
			}

			// A wrong code is refused as unauthorized and audited as failed.
			f = newFaultMFA(t)
			token := f.armChallenge()
			handler, auditor := build(f)
			recorder := serveMFA(handler, nil, bodyOf(token, "000000"))
			if recorder.Code != http.StatusUnauthorized {
				t.Errorf("wrong code status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if audited && !hasEvent(auditor, auth.SecurityMFAChallengeFailed, auth.SecurityOutcomeFailed) {
				t.Errorf("no failed challenge event: %+v", auditor.events)
			}

			// The auditor failing does not change the refusal.
			if audited {
				auditor.err = errors.New("audit down")
				if recorder := serveMFA(handler, nil, bodyOf(token, "000000")); recorder.Code != http.StatusUnauthorized {
					t.Errorf("failing auditor changed the refusal: status=%d", recorder.Code)
				}
			}

			// A throttled challenge answers 429 with Retry-After.
			f = newFaultMFA(t, auth.WithMFAThrottler(failingThrottler{allowed: false}))
			token = f.armChallenge()
			handler, _ = build(f)
			recorder = serveMFA(handler, nil, bodyOf(token, f.code()))
			if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "7" {
				t.Errorf("throttled status=%d Retry-After=%q", recorder.Code, recorder.Header().Get("Retry-After"))
			}

			// A good code signs in; its success is audited once by the handler.
			f = newFaultMFA(t)
			token = f.armChallenge()
			handler, auditor = build(f)
			recorder = serveMFA(handler, nil, bodyOf(token, f.code()))
			if recorder.Code != http.StatusOK {
				t.Fatalf("sign-in status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var response loginResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.TokenType != "Bearer" || response.AccessToken == "" {
				t.Errorf("response=%+v err=%v", response, err)
			}
			if audited && !hasEvent(auditor, auth.SecurityMFAChallengeSucceeded, auth.SecurityOutcomeSucceeded) {
				t.Errorf("no succeeded challenge event: %+v", auditor.events)
			}
			if audited {
				// A failing auditor on success is logged, never an error.
				f = newFaultMFA(t)
				token = f.armChallenge()
				handler, auditor = build(f)
				auditor.err = errors.New("audit down")
				if recorder := serveMFA(handler, nil, bodyOf(token, f.code())); recorder.Code != http.StatusOK {
					t.Errorf("failing auditor on success: status=%d", recorder.Code)
				}
			}
		})
	}
}
