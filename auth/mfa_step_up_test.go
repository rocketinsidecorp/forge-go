package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/principalctx"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// stepUpGateOver is the gate over f's service and store, with a login
// service on a cheap hasher.
func stepUpGateOver(t *testing.T, f *faultMFA) *MFAStepUpGate {
	t.Helper()
	login, err := NewLoginService(newAccountsStore(t, nil), &sessionStub{}, WithLoginPasswordHasher(cheapPasswordHasher(t)))
	if err != nil {
		t.Fatal(err)
	}
	gate, err := NewMFAStepUpGate(login, f.service, f.store)
	if err != nil {
		t.Fatal(err)
	}
	return gate
}

// deniedAudits counts the denied events of kind f recorded.
func deniedAudits(f *faultMFA, kind SecurityEventKind) int {
	count := 0
	for _, event := range f.audits {
		if event.Kind == kind && event.Outcome == SecurityOutcomeDenied {
			count++
		}
	}
	return count
}

func TestNewMFAStepUpGateNeedsEveryService(t *testing.T) {
	t.Parallel()
	f := newFaultMFA(t)
	login, err := NewLoginService(newAccountsStore(t, nil), &sessionStub{})
	if err != nil {
		t.Fatal(err)
	}
	for name, build := range map[string]func() (*MFAStepUpGate, error){
		"no login":     func() (*MFAStepUpGate, error) { return NewMFAStepUpGate(nil, f.service, f.store) },
		"no MFA":       func() (*MFAStepUpGate, error) { return NewMFAStepUpGate(login, nil, f.store) },
		"no passwords": func() (*MFAStepUpGate, error) { return NewMFAStepUpGate(login, f.service, nil) },
	} {
		if gate, err := build(); err == nil || gate != nil {
			t.Errorf("%s: gate=%v err=%v, want an error", name, gate, err)
		}
	}
}

// The step-up passes with the password (and, when asked, a current code) and
// refuses everything else, auditing each refusal after the throttle attempt.
func TestMFAStepUpGateChecksPasswordAndCode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const action = SecurityMFAEnrollmentStarted

	f := newFaultMFA(t)
	gate := stepUpGateOver(t, f)
	pass, err := gate.Check(ctx, f.subject, "192.0.2.1", action, MFAStepUp{Password: mfaTestPassword}, false, nil)
	if err != nil || pass.Subject() != f.subject || pass.Login() != "ada@example.com" || pass.Proof().Subject() != (uuid.UUID{}) {
		t.Fatalf("password step-up: pass=%+v err=%v", pass, err)
	}
	if err := gate.Succeeded(ctx, pass); err != nil {
		t.Fatalf("succeeded: %v", err)
	}
	var reported []error
	report := func(err error) { reported = append(reported, err) }
	if _, err := gate.Check(ctx, f.subject, "192.0.2.1", action, MFAStepUp{Password: "wrong"}, false, report); !errors.Is(err, ErrMFAStepUpInvalid) {
		t.Fatalf("wrong password: %v", err)
	}
	if deniedAudits(f, action) != 1 || len(reported) != 0 {
		t.Fatalf("wrong password: %d denied audits, reported %v", deniedAudits(f, action), reported)
	}
	if event := f.audits[len(f.audits)-1]; event.ActorID != f.subject || event.SubjectID != f.subject || event.SourceDigest == (Digest{}) {
		t.Fatalf("denied audit %+v, want the account and the source digest", event)
	}

	// An account with a factor: the password-only step-up is refused whatever
	// the password; with a code, a current one passes and binds the proof.
	f.enroll()
	if _, err := gate.Check(ctx, f.subject, "192.0.2.1", action, MFAStepUp{Password: mfaTestPassword}, false, nil); !errors.Is(err, ErrMFAAlreadyEnabled) {
		t.Fatalf("password-only step-up of an enrolled account: %v", err)
	}
	if _, err := gate.Check(ctx, f.subject, "192.0.2.1", SecurityMFADisabled, MFAStepUp{Password: mfaTestPassword, Code: "000000"}, true, nil); !errors.Is(err, ErrMFAStepUpInvalid) {
		t.Fatalf("wrong code: %v", err)
	}
	pass, err = gate.Check(ctx, f.subject, "192.0.2.1", SecurityMFADisabled, MFAStepUp{Password: mfaTestPassword, Code: f.code()}, true, nil)
	if err != nil || pass.Proof().Subject() != f.subject {
		t.Fatalf("code step-up: pass=%+v err=%v", pass, err)
	}
	if deniedAudits(f, action) != 2 || deniedAudits(f, SecurityMFADisabled) != 1 {
		t.Fatalf("audits %+v", f.audits)
	}

	// Refused: a used-up step-up code becomes ErrMFAStepUpInvalid, audited;
	// any other refusal is returned as is. Denied audits.
	if err := gate.Refused(ctx, SecurityMFADisabled, pass, ErrMFAStepUpInvalid, nil); !errors.Is(err, ErrMFAStepUpInvalid) || deniedAudits(f, SecurityMFADisabled) != 2 {
		t.Fatalf("refused, code spent: %v, %d audits", err, deniedAudits(f, SecurityMFADisabled))
	}
	if err := gate.Refused(ctx, SecurityMFADisabled, pass, errInjected, nil); !errors.Is(err, errInjected) || deniedAudits(f, SecurityMFADisabled) != 2 {
		t.Fatalf("refused, other error: %v, %d audits", err, deniedAudits(f, SecurityMFADisabled))
	}
	gate.Denied(ctx, SecurityMFADisabled, pass, nil)
	if deniedAudits(f, SecurityMFADisabled) != 3 {
		t.Fatalf("denied: %d audits", deniedAudits(f, SecurityMFADisabled))
	}

	// An audit failure of a refusal goes to the reporter; the refusal stands.
	f.auditErr = errInjected
	if _, err := gate.Check(ctx, f.subject, "192.0.2.1", action, MFAStepUp{Password: "wrong"}, false, report); !errors.Is(err, ErrMFAAlreadyEnabled) || len(reported) != 1 {
		t.Fatalf("audit failure: err=%v reported=%v", err, reported)
	}
}

// Every store and throttle failure fails the step-up closed.
func TestMFAStepUpGateFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const action = SecurityMFAEnrollmentStarted
	check := func(f *faultMFA, needCode bool, code string) error {
		_, err := stepUpGateOver(t, f).Check(ctx, f.subject, "192.0.2.1", action, MFAStepUp{Password: mfaTestPassword, Code: code}, needCode, nil)
		return err
	}

	broken := newFaultMFA(t, WithMFAThrottler(failingThrottler{attemptErr: errInjected}))
	if err := check(broken, false, ""); !errors.Is(err, errInjected) || len(broken.audits) != 0 {
		t.Errorf("throttle failure: %v, audits %+v", err, broken.audits)
	}
	throttled := newFaultMFA(t, WithMFAThrottler(failingThrottler{}))
	var limited *LoginThrottledError
	if err := check(throttled, false, ""); !errors.As(err, &limited) || limited.RetryAfter != 7*time.Second || deniedAudits(throttled, action) != 1 {
		t.Errorf("throttled: %v, audits %+v", err, throttled.audits)
	}
	resetFails := newFaultMFA(t, WithMFAThrottler(failingThrottler{allowed: true, successErr: errInjected}))
	pass, err := stepUpGateOver(t, resetFails).Check(ctx, resetFails.subject, "192.0.2.1", action, MFAStepUp{Password: mfaTestPassword}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := stepUpGateOver(t, resetFails).Succeeded(ctx, pass); !errors.Is(err, errInjected) {
		t.Errorf("throttle reset failure: %v", err)
	}
	for method, needCode := range map[string]bool{"LoadMFAStatus": false, "LookupSubjectPassword": false, "LoadActiveTOTP": true} {
		f := newFaultMFA(t)
		f.enroll()
		if !needCode {
			f.store.accounts[f.subject].factor = nil
		}
		f.store.fail[method] = errInjected
		if err := check(f, needCode, f.code()); !errors.Is(err, errInjected) {
			t.Errorf("%s failing: %v", method, err)
		}
	}
}

// AuthorizeMFAChange gives a proof only for the signed-in principal of the
// context, and only when the application's authorizer allows the change.
func TestAuthorizeMFAChangeBindsTheProofToTheSignedInPrincipal(t *testing.T) {
	t.Parallel()
	f := newMFADomainFixture(t)
	f.enable(f.ada)
	f.enable(f.bob)
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	ada, err := NewPrincipal(f.ada, tenant)
	if err != nil {
		t.Fatal(err)
	}
	signedIn := principalctx.With(context.Background(), ada)
	var asked []uuid.UUID
	allow := MFAChangeAuthorizerFunc(func(_ context.Context, principal Principal) error {
		asked = append(asked, principal.SubjectID())
		return nil
	})
	refusal := errors.New("step-up required")
	deny := MFAChangeAuthorizerFunc(func(context.Context, Principal) error { return refusal })

	if proof, err := AuthorizeMFAChange(signedIn, nil); err == nil || proof != (MFAStepUpProof{}) {
		t.Errorf("nil authorizer: proof=%+v err=%v", proof, err)
	}
	if proof, err := AuthorizeMFAChange(context.Background(), allow); !errors.Is(err, ErrCredentialsRequired) || proof != (MFAStepUpProof{}) || len(asked) != 0 {
		t.Errorf("no principal: proof=%+v err=%v asked=%v", proof, err, asked)
	}
	if proof, err := AuthorizeMFAChange(signedIn, deny); !errors.Is(err, refusal) || proof != (MFAStepUpProof{}) {
		t.Errorf("refused: proof=%+v err=%v", proof, err)
	}
	proof, err := AuthorizeMFAChange(signedIn, allow)
	if err != nil || proof.Subject() != f.ada || proof.BackupCode() || len(asked) != 1 || asked[0] != f.ada {
		t.Fatalf("allowed: proof=%+v err=%v asked=%v", proof, err, asked)
	}
	// The proof is ada's: it changes nothing of bob's, and works for ada.
	if err := f.mfa.DisableMFA(context.Background(), f.bob, proof); !errors.Is(err, ErrMFAStepUpInvalid) {
		t.Errorf("another account: %v", err)
	}
	if err := f.mfa.DisableMFA(context.Background(), f.ada, proof); err != nil {
		t.Errorf("ada's own change: %v", err)
	}
	if !f.status(f.bob).Enabled || f.status(f.ada).Enabled {
		t.Error("the proof changed the wrong account")
	}
}

func TestValidTOTPIssuer(t *testing.T) {
	t.Parallel()
	for issuer, want := range map[string]bool{"Forge": true, "": false, "a\nb": false, string(make([]byte, 129)): false} {
		if got := ValidTOTPIssuer(issuer); got != want {
			t.Errorf("ValidTOTPIssuer(%q) = %v, want %v", issuer, got, want)
		}
	}
}

// The small adapters and error types the HTTP side used to exercise.
func TestAuthAdaptersAndErrorTexts(t *testing.T) {
	t.Parallel()
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	principal, err := NewPrincipal(uuid.MustNew(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	if principal.Tenant() != tenant {
		t.Error("Tenant() is not the principal's tenant")
	}
	resolver := ResolverFunc(func(context.Context, Digest) (Session, bool, error) {
		return Session{Principal: principal}, true, nil
	})
	if session, found, err := resolver.ResolveSession(context.Background(), Digest{}); err != nil || !found || session.Principal.SubjectID() != principal.SubjectID() {
		t.Errorf("ResolverFunc: %+v %v %v", session, found, err)
	}
	called := false
	enroll := MFAEnrollmentAuthorizerFunc(func(context.Context, Principal) error { called = true; return errInjected })
	if err := enroll.AuthorizeMFAEnrollment(context.Background(), principal); !errors.Is(err, errInjected) || !called {
		t.Errorf("MFAEnrollmentAuthorizerFunc: %v", err)
	}
	if _, err := NewRecoveryService(&recoveryStoreStub{}, &recoverySenderStub{}, WithRecoveryPasswordHasher(nil)); err == nil {
		t.Error("a nil recovery hasher was accepted")
	}
	for err, want := range map[error]string{
		&SecurityAuditError{Cause: errInjected}:    "security audit failed",
		&LoginThrottledError{RetryAfter: 1}:        ErrLoginThrottled.Error(),
		&RecoveryDeliveryError{Cause: errInjected}: ErrRecoveryDelivery.Error(),
		&MFAPendingCodeError{}:                     "auth: the code of the new authenticator key is wrong",
	} {
		if err.Error() != want {
			t.Errorf("%T.Error() = %q, want %q", err, err.Error(), want)
		}
	}
	var none *SecurityAuditError
	if none.Unwrap() != nil || !errors.Is(&SecurityAuditError{Cause: errInjected}, errInjected) {
		t.Error("SecurityAuditError does not unwrap to its cause")
	}
}
