package auth

import (
	"errors"
	"net/http"

	"github.com/rocketinsidecorp/forge-go/csrf"
	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/web"
)

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
// factor, a current authenticator or backup code (used up).
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

// MFASettingsAvailable reports whether the two-factor settings of a signed-in
// account work: the Browser has an MFA service and a SubjectPasswordStore.
func (b *Browser) MFASettingsAvailable() bool { return b.mfa != nil && b.passwords != nil }

// MFAStatus returns the second-factor state of the signed-in account (the
// request must have passed Middleware.AuthenticateBrowser or
// AuthenticateCookie).
func (b *Browser) MFAStatus(r *http.Request) (MFAStatus, error) {
	if !b.MFASettingsAvailable() {
		return MFAStatus{}, ErrMFAUnavailable
	}
	principal, ok := FromContext(r.Context())
	if !ok {
		return MFAStatus{}, ErrCredentialsRequired
	}
	return b.mfa.MFAStatus(r.Context(), principal.SubjectID())
}

// BeginMFAEnrollment starts turning two-factor authentication on for the
// signed-in account after proof.Password is right (proof.Code is ignored).
// It returns the new key, pending for ten minutes until
// ConfirmMFAEnrollment; show it on a no-store page and never log it. issuer
// names the application in the authenticator app; the account's login is the
// account label.
//
// A wrong password is ErrMFAStepUpInvalid; attempts are counted per account
// and per network source on the MFA throttler before anything is checked
// (*LoginThrottledError, with Retry-After set). An account that already has a
// factor is ErrMFAAlreadyEnabled, whatever the password: it is refused before
// the password is checked, so the answer does not reveal it. Like every method that changes
// credentials, it requires csrf.Verified and an authenticated principal.
func (b *Browser) BeginMFAEnrollment(w http.ResponseWriter, r *http.Request, issuer string, proof MFAStepUp) (TOTPEnrollment, error) {
	return b.beginMFAChange(w, r, issuer, proof, false)
}

// BeginMFARotation starts replacing the authenticator of the signed-in
// account after the password and a current code (used up). The old factor
// keeps working until ConfirmMFARotation. Errors are BeginMFAEnrollment's,
// with ErrMFANotEnabled for an account without a factor.
func (b *Browser) BeginMFARotation(w http.ResponseWriter, r *http.Request, issuer string, proof MFAStepUp) (TOTPEnrollment, error) {
	return b.beginMFAChange(w, r, issuer, proof, true)
}

func (b *Browser) beginMFAChange(w http.ResponseWriter, r *http.Request, issuer string, proof MFAStepUp, rotation bool) (TOTPEnrollment, error) {
	action := SecurityMFAEnrollmentStarted
	if rotation {
		action = SecurityMFARotationStarted
	}
	step, err := b.stepUp(w, r, action, proof, rotation)
	if err != nil {
		return TOTPEnrollment{}, err
	}
	var enrollment TOTPEnrollment
	if rotation {
		enrollment, err = b.mfa.BeginTOTPRotation(r.Context(), step.subject, issuer, step.login)
	} else {
		enrollment, err = b.mfa.BeginTOTPEnrollment(r.Context(), step.subject, issuer, step.login)
	}
	if err != nil {
		if _, applied := logSecurityAuditFailure(r, err); !applied {
			return TOTPEnrollment{}, err
		}
	}
	b.stepUpSucceeded(r, step)
	return enrollment, nil
}

// ConfirmMFAEnrollment turns two-factor authentication on with code, a code
// of the pending key, after re-checking proof.Password through the same
// throttled step-up as BeginMFAEnrollment: a stolen session alone can neither
// guess the code nor see the key again. It returns the backup codes; show
// them once. The store revoked every session of the account, so this
// browser's session and CSRF cookies are cleared: render the codes in this
// response (with no forms) and send the user to sign in again.
//
// A wrong password is ErrMFAStepUpInvalid; a wrong code is
// *MFAPendingCodeError carrying the key to show again, and counts on the
// step-up throttle; an expired or missing setup is ErrMFAEnrollmentInvalid.
// An account that already has a factor is ErrMFAAlreadyEnabled, also while a
// rotation is pending: that needs ConfirmMFARotation and a current code.
func (b *Browser) ConfirmMFAEnrollment(w http.ResponseWriter, r *http.Request, issuer string, proof MFAStepUp, code string) ([]MFABackupCode, error) {
	return b.confirmMFAChange(w, r, issuer, proof, code, false)
}

// ConfirmMFARotation replaces the authenticator with the pending key after
// the password and a current code of the old factor (proof.Code, used up),
// then code of the new key. Results are ConfirmMFAEnrollment's.
func (b *Browser) ConfirmMFARotation(w http.ResponseWriter, r *http.Request, issuer string, proof MFAStepUp, code string) ([]MFABackupCode, error) {
	return b.confirmMFAChange(w, r, issuer, proof, code, true)
}

func (b *Browser) confirmMFAChange(w http.ResponseWriter, r *http.Request, issuer string, proof MFAStepUp, code string, rotation bool) ([]MFABackupCode, error) {
	action := SecurityMFAEnabled
	if rotation {
		action = SecurityMFARotated
	}
	step, err := b.stepUp(w, r, action, proof, rotation)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	var confirmation TOTPConfirmation
	if rotation {
		confirmation, err = b.mfa.ConfirmTOTPRotation(ctx, step.subject, code)
	} else {
		confirmation, err = b.mfa.ConfirmTOTPEnrollment(ctx, step.subject, code)
	}
	if err != nil {
		if _, applied := logSecurityAuditFailure(r, err); !applied {
			if !errors.Is(err, ErrMFAEnrollmentInvalid) {
				return nil, err
			}
			// The step-up counter is not reset: wrong codes of the new key
			// count against it.
			b.recordStepUpDenied(r, action, step.subject, step.key)
			enrollment, found, pendingErr := b.mfa.PendingTOTP(ctx, step.subject, issuer, step.login)
			if pendingErr != nil {
				return nil, pendingErr
			}
			if !found {
				return nil, ErrMFAEnrollmentInvalid
			}
			return nil, &MFAPendingCodeError{Enrollment: enrollment}
		}
	}
	clearSessionCookies(w)
	b.stepUpSucceeded(r, step)
	return confirmation.BackupCodes, nil
}

// RegenerateMFABackupCodes replaces the backup codes of the signed-in
// account after the password and a current code (used up), and returns the
// new ones; show them once. The old codes stop working; sessions stay. A
// wrong password or code is ErrMFAStepUpInvalid, throttling
// *LoginThrottledError, an account without a factor ErrMFAStepUpInvalid too
// (it has no code to give).
func (b *Browser) RegenerateMFABackupCodes(w http.ResponseWriter, r *http.Request, proof MFAStepUp) ([]MFABackupCode, error) {
	step, err := b.stepUp(w, r, SecurityMFABackupCodesRegenerated, proof, true)
	if err != nil {
		return nil, err
	}
	codes, err := b.mfa.RegenerateMFABackupCodes(r.Context(), step.subject)
	if err != nil {
		if _, applied := logSecurityAuditFailure(r, err); !applied {
			return nil, err
		}
	}
	b.stepUpSucceeded(r, step)
	return codes, nil
}

// DisableMFA turns two-factor authentication off for the signed-in account
// after the password and a current code (used up). The store revokes every
// session of the account, so this browser's session and CSRF cookies are
// cleared: redirect to the sign-in page. Errors are
// RegenerateMFABackupCodes'.
func (b *Browser) DisableMFA(w http.ResponseWriter, r *http.Request, proof MFAStepUp) error {
	step, err := b.stepUp(w, r, SecurityMFADisabled, proof, true)
	if err != nil {
		return err
	}
	if err := b.mfa.DisableMFA(r.Context(), step.subject); err != nil {
		if _, applied := logSecurityAuditFailure(r, err); !applied {
			return err
		}
	}
	clearSessionCookies(w)
	b.stepUpSucceeded(r, step)
	return nil
}

// verifiedStepUp is a passed re-authentication. The step-up may clear the
// throttle counters once the change succeeds: a code was verified, or the
// account had no factor (an account with one never passes with a password
// alone, or interleaving password-only requests would give a session holding
// the password unlimited code guesses).
type verifiedStepUp struct {
	subject uuid.UUID
	login   string
	key     LoginThrottleKey
}

// stepUp re-authenticates the signed-in account for action: the CSRF token,
// then one attempt on the MFA throttler (per account and per source) before
// anything is checked, then, without needCode, a refusal of an account that
// already has a factor (ErrMFAAlreadyEnabled, so the password is not
// revealed by the answer; a pending rotation is confirmed with
// ConfirmMFARotation and a current code, never through the password-only
// path), then the password and, with needCode, a current code that is used
// up. A refusal other than ErrMFAAlreadyEnabled is audited as action denied. The throttle
// counter is reset only when the whole change succeeds (stepUpSucceeded).
func (b *Browser) stepUp(w http.ResponseWriter, r *http.Request, action SecurityEventKind, proof MFAStepUp, needCode bool) (verifiedStepUp, error) {
	noStore(w)
	if !csrf.Verified(r) {
		return verifiedStepUp{}, ErrCSRFInvalid
	}
	if !b.MFASettingsAvailable() {
		return verifiedStepUp{}, ErrMFAUnavailable
	}
	principal, ok := FromContext(r.Context())
	if !ok {
		return verifiedStepUp{}, ErrCredentialsRequired
	}
	ctx := r.Context()
	subject := principal.SubjectID()
	key := deriveMFAStepUpThrottleKey(subject, requestSource(r))
	decision, err := b.mfa.throttler.Attempt(ctx, key)
	if err != nil {
		return verifiedStepUp{}, err
	}
	if !decision.Allowed {
		b.recordStepUpDenied(r, action, subject, key)
		w.Header().Set("Retry-After", retryAfterHeader(decision.RetryAfter))
		return verifiedStepUp{}, &LoginThrottledError{RetryAfter: decision.RetryAfter}
	}
	if !needCode {
		// With no code to give, the answer to a password must not depend on
		// whether it is right: an account that already has a factor is
		// refused before the password is checked (ErrMFAAlreadyEnabled), or
		// the refusal (422) and the success (303) of the next step would
		// tell a session holder the password.
		status, err := b.mfa.MFAStatus(ctx, subject)
		if err != nil {
			return verifiedStepUp{}, err
		}
		if status.Enabled {
			return verifiedStepUp{}, ErrMFAAlreadyEnabled
		}
	}
	login, ok, err := verifySubjectPassword(ctx, b.passwords, subject, proof.Password)
	if err != nil {
		return verifiedStepUp{}, err
	}
	if !ok {
		b.recordStepUpDenied(r, action, subject, key)
		return verifiedStepUp{}, ErrMFAStepUpInvalid
	}
	if needCode {
		if err := b.mfa.VerifyMFACode(ctx, subject, proof.Code); err != nil {
			if errors.Is(err, ErrMFAInvalid) {
				b.recordStepUpDenied(r, action, subject, key)
				return verifiedStepUp{}, ErrMFAStepUpInvalid
			}
			// A backup code whose use was not audited is still used up.
			if _, applied := logSecurityAuditFailure(r, err); !applied {
				return verifiedStepUp{}, err
			}
		}
	}
	return verifiedStepUp{subject: subject, login: login, key: key}, nil
}

// stepUpSucceeded resets the step-up counter after a completed change. A
// failed reset only leaves the counter higher, so it is logged, not returned:
// the change (and any one-time backup codes) already happened.
func (b *Browser) stepUpSucceeded(r *http.Request, step verifiedStepUp) {
	if err := b.mfa.throttler.Success(r.Context(), step.key); err != nil {
		web.Logger(r.Context()).Error("MFA step-up throttle reset failed", "path", r.URL.Path, "error", err)
	}
}

// recordStepUpDenied audits a refused two-factor change as action denied.
// An audit failure is logged; the refusal stands.
func (b *Browser) recordStepUpDenied(r *http.Request, action SecurityEventKind, subject uuid.UUID, key LoginThrottleKey) {
	event := SecurityEvent{
		Kind:      action,
		Outcome:   SecurityOutcomeDenied,
		ActorID:   subject,
		SubjectID: subject,
	}
	if key.HasSource {
		event.SourceDigest = Digest(key.Source)
	}
	if err := b.mfa.recordMFAEvent(r.Context(), event, false); err != nil {
		_, _ = logSecurityAuditFailure(r, err)
	}
}
