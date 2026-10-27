package auth

import (
	"context"
	"errors"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// MFAStepUpGate is the transport-independent half of the two-factor settings
// of a signed-in account: the throttled, audited re-authentication (step-up)
// that every change of the second factor passes first. authhttp.Browser drives
// it from the settings forms; it adds the CSRF check, the network source and
// the response headers (ADR 0052).
type MFAStepUpGate struct {
	mfa       *MFAService
	hasher    *PasswordHasher
	passwords SubjectPasswordStore
}

// NewMFAStepUpGate returns the gate over mfa (its throttler, factors and
// audit), the password hasher of login (and its legacy timing floor) and
// passwords, the stored credential of each account. All three are required.
func NewMFAStepUpGate(login *LoginService, mfa *MFAService, passwords SubjectPasswordStore) (*MFAStepUpGate, error) {
	if login == nil || mfa == nil || passwords == nil {
		return nil, errors.New("auth: the MFA step-up needs a login service, an MFA service and a subject password store")
	}
	return &MFAStepUpGate{mfa: mfa, hasher: login.hasher, passwords: passwords}, nil
}

// MFAStepUpPass is a passed re-authentication. The gate may clear the
// throttle counters once the change succeeds (Succeeded): a code was
// verified, or the account had no factor (an account with one never passes
// with a password alone, or interleaving password-only requests would give a
// session holding the password unlimited code guesses).
type MFAStepUpPass struct {
	subject uuid.UUID
	login   string
	key     LoginThrottleKey
	// proof is the current code's: a backup code is used up by the change.
	proof MFAStepUpProof
}

// Subject is the re-authenticated account.
func (p MFAStepUpPass) Subject() uuid.UUID { return p.subject }

// Login is the account's login, the account label of a new authenticator key.
func (p MFAStepUpPass) Login() string { return p.login }

// Proof is the current code's proof, for the change the step-up guards.
func (p MFAStepUpPass) Proof() MFAStepUpProof { return p.proof }

// Check re-authenticates subject for action: one attempt on the MFA throttler
// (per account and per source) before anything is checked, then, without
// needCode, a refusal of an account that already has a factor
// (ErrMFAAlreadyEnabled, so the password is not revealed by the answer; a
// pending rotation is confirmed with a current code, never through the
// password-only path), then the password and, with needCode, a current code:
// an authenticator code is used up, a backup code only checked (the change
// uses it up, so a change that fails keeps it). Every refusal after the
// throttle attempt is audited as action denied; auditFailed receives an audit
// failure, which does not change the refusal (nil drops it).
//
// A throttled attempt is *LoginThrottledError, whose RetryAfter the caller
// sends; saturated password hashing is ErrPasswordHashingUnavailable. The
// throttle counter is reset only when the whole change succeeds (Succeeded).
func (g *MFAStepUpGate) Check(ctx context.Context, subject uuid.UUID, source string, action SecurityEventKind, proof MFAStepUp, needCode bool, auditFailed func(error)) (MFAStepUpPass, error) {
	key := deriveMFAStepUpThrottleKey(subject, source)
	decision, err := g.mfa.throttler.Attempt(ctx, key)
	if err != nil {
		return MFAStepUpPass{}, err
	}
	if !decision.Allowed {
		g.denied(ctx, action, subject, key, auditFailed)
		return MFAStepUpPass{}, &LoginThrottledError{RetryAfter: decision.RetryAfter}
	}
	if !needCode {
		// With no code to give, the answer to a password must not depend on
		// whether it is right: an account that already has a factor is
		// refused before the password is checked (ErrMFAAlreadyEnabled), or
		// the refusal (422) and the success (303) of the next step would
		// tell a session holder the password.
		status, err := g.mfa.MFAStatus(ctx, subject)
		if err != nil {
			return MFAStepUpPass{}, err
		}
		if status.Enabled {
			// Audited whatever the password, so the audit trail keeps the
			// attempt without depending on it.
			g.denied(ctx, action, subject, key, auditFailed)
			return MFAStepUpPass{}, ErrMFAAlreadyEnabled
		}
	}
	started := g.hasher.checkStarted()
	login, ok, err := verifySubjectPassword(ctx, g.hasher, g.passwords, subject, proof.Password)
	if err != nil {
		return MFAStepUpPass{}, err
	}
	if !ok {
		// Audited before the legacy timing floor's padding, so hanging up
		// during it cannot skip the record.
		g.denied(ctx, action, subject, key, auditFailed)
		if err := g.hasher.padFailedCheck(ctx, started); err != nil {
			return MFAStepUpPass{}, err
		}
		return MFAStepUpPass{}, ErrMFAStepUpInvalid
	}
	var codeProof MFAStepUpProof
	if needCode {
		codeProof, err = g.mfa.VerifyMFAStepUp(ctx, subject, proof.Code)
		if err != nil {
			if errors.Is(err, ErrMFAInvalid) {
				g.denied(ctx, action, subject, key, auditFailed)
				return MFAStepUpPass{}, ErrMFAStepUpInvalid
			}
			return MFAStepUpPass{}, err
		}
	}
	return MFAStepUpPass{subject: subject, login: login, key: key, proof: codeProof}, nil
}

// Refused turns err, the refusal of a change that passed the step-up, into
// ErrMFAStepUpInvalid (audited as action denied) when the step-up backup code
// was used up meanwhile, and returns it unchanged otherwise.
func (g *MFAStepUpGate) Refused(ctx context.Context, action SecurityEventKind, pass MFAStepUpPass, err error, auditFailed func(error)) error {
	if !errors.Is(err, ErrMFAStepUpInvalid) {
		return err
	}
	g.denied(ctx, action, pass.subject, pass.key, auditFailed)
	return ErrMFAStepUpInvalid
}

// Denied audits a change that passed the step-up and was then refused, such
// as a wrong code of a new authenticator key, as action denied. The step-up
// counter is not reset: such refusals count against it.
func (g *MFAStepUpGate) Denied(ctx context.Context, action SecurityEventKind, pass MFAStepUpPass, auditFailed func(error)) {
	g.denied(ctx, action, pass.subject, pass.key, auditFailed)
}

// Succeeded resets the step-up counter after a completed change. A failed
// reset only leaves the counter higher, so the caller logs it: the change (and
// any one-time backup codes) already happened.
func (g *MFAStepUpGate) Succeeded(ctx context.Context, pass MFAStepUpPass) error {
	return g.mfa.throttler.Success(ctx, pass.key)
}

// denied audits a refused two-factor change as action denied. An audit
// failure goes to auditFailed; the refusal stands.
func (g *MFAStepUpGate) denied(ctx context.Context, action SecurityEventKind, subject uuid.UUID, key LoginThrottleKey, auditFailed func(error)) {
	event := SecurityEvent{
		Kind:      action,
		Outcome:   SecurityOutcomeDenied,
		ActorID:   subject,
		SubjectID: subject,
	}
	if key.HasSource {
		event.SourceDigest = Digest(key.Source)
	}
	if err := g.mfa.recordMFAEvent(ctx, event, false); err != nil && auditFailed != nil {
		auditFailed(err)
	}
}

var (
	// ErrMFAStepUpInvalid means the password, or the current code, given to
	// re-authenticate before a two-factor change was wrong. On the routes
	// that also need a code it does not say which, so show one message. The
	// routes that need no code refuse an account that already has a factor
	// with ErrMFAAlreadyEnabled before the password is checked, so a wrong
	// password there is ErrMFAStepUpInvalid only for an account without one.
	ErrMFAStepUpInvalid = fault.New("mfa_step_up_invalid", "password or code is invalid", fault.CategoryUnauthorized, 0)
	// ErrMFAUnavailable means the two-factor settings cannot work: the
	// Browser has no MFA service (FORGE_AUTH_MFA_KEY is not set) or no
	// SubjectPasswordStore to re-check the password.
	ErrMFAUnavailable = fault.New("mfa_unavailable", "two-factor authentication is not available", fault.CategoryUnavailable, 0)
)

// MFAStepUp is the proof a signed-in account gives again before it changes
// its second factor: the password and, when the account already has a
// factor, a current authenticator code (used up) or backup code (used up by
// the change only if it succeeds).
type MFAStepUp struct {
	Password string
	Code     string
}

// MFAPendingCodeError means re-authentication succeeded but the code of the
// new authenticator key was wrong. The setup stays pending; Enrollment is its
// key, to show again (the user proved the password, and for a rotation a
// current code, in this request). Its text never contains the key.
type MFAPendingCodeError struct {
	Enrollment TOTPEnrollment
}

func (e *MFAPendingCodeError) Error() string {
	return "auth: the code of the new authenticator key is wrong"
}

// MFAEnrollmentAuthorizer is the application's step-up check before TOTP enrollment starts, such as
// asking for the password again. A non-nil error refuses the request.
type MFAEnrollmentAuthorizer interface {
	AuthorizeMFAEnrollment(ctx context.Context, principal Principal) error
}

// MFAEnrollmentAuthorizerFunc adapts a function to MFAEnrollmentAuthorizer.
type MFAEnrollmentAuthorizerFunc func(context.Context, Principal) error

// AuthorizeMFAEnrollment calls fn.
func (fn MFAEnrollmentAuthorizerFunc) AuthorizeMFAEnrollment(ctx context.Context, principal Principal) error {
	return fn(ctx, principal)
}

// MFAChangeAuthorizer is the application's step-up check before an active factor is rotated or
// disabled. A non-nil error refuses the request.
type MFAChangeAuthorizer interface {
	AuthorizeMFAChange(ctx context.Context, principal Principal) error
}

// MFAChangeAuthorizerFunc adapts a function to MFAChangeAuthorizer.
type MFAChangeAuthorizerFunc func(context.Context, Principal) error

// AuthorizeMFAChange calls fn.
func (fn MFAChangeAuthorizerFunc) AuthorizeMFAChange(ctx context.Context, principal Principal) error {
	return fn(ctx, principal)
}

// AuthorizeMFAChange runs authorizer, the application's step-up check of the
// JSON two-factor routes, for principal, and returns the proof of the change
// it allowed: it carries no backup code. A non-nil error refuses the change.
func AuthorizeMFAChange(ctx context.Context, authorizer MFAChangeAuthorizer, principal Principal) (MFAStepUpProof, error) {
	if authorizer == nil {
		return MFAStepUpProof{}, errors.New("auth: MFA change authorizer is required")
	}
	if err := authorizer.AuthorizeMFAChange(ctx, principal); err != nil {
		return MFAStepUpProof{}, err
	}
	return changeAuthorizedBy(principal.SubjectID()), nil
}

// ValidTOTPIssuer reports whether issuer can name the application in an
// authenticator app: 1 to 128 bytes with no control characters, the rule
// BeginTOTPEnrollment applies.
func ValidTOTPIssuer(issuer string) bool { return validTOTPLabel(issuer, 128) }
