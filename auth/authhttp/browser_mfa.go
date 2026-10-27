package authhttp

import (
	"errors"
	"net/http"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/csrf"
	"github.com/rocketinsidecorp/forge-go/web"
)

// MFASettingsAvailable reports whether the two-factor settings of a signed-in
// account work: the Browser has an MFA service and a SubjectPasswordStore.
func (b *Browser) MFASettingsAvailable() bool { return b.stepUpGate != nil }

// MFAStatus returns the second-factor state of the signed-in account (the
// request must have passed Middleware.AuthenticateBrowser or
// AuthenticateCookie).
func (b *Browser) MFAStatus(r *http.Request) (auth.MFAStatus, error) {
	if !b.MFASettingsAvailable() {
		return auth.MFAStatus{}, auth.ErrMFAUnavailable
	}
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		return auth.MFAStatus{}, auth.ErrCredentialsRequired
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
func (b *Browser) BeginMFAEnrollment(w http.ResponseWriter, r *http.Request, issuer string, proof auth.MFAStepUp) (auth.TOTPEnrollment, error) {
	return b.beginMFAChange(w, r, issuer, proof, false)
}

// BeginMFARotation starts replacing the authenticator of the signed-in
// account after the password and a current code (used up). The old factor
// keeps working until ConfirmMFARotation. Errors are BeginMFAEnrollment's,
// with ErrMFANotEnabled for an account without a factor.
func (b *Browser) BeginMFARotation(w http.ResponseWriter, r *http.Request, issuer string, proof auth.MFAStepUp) (auth.TOTPEnrollment, error) {
	return b.beginMFAChange(w, r, issuer, proof, true)
}

func (b *Browser) beginMFAChange(w http.ResponseWriter, r *http.Request, issuer string, proof auth.MFAStepUp, rotation bool) (auth.TOTPEnrollment, error) {
	action := auth.SecurityMFAEnrollmentStarted
	if rotation {
		action = auth.SecurityMFARotationStarted
	}
	step, err := b.stepUp(w, r, action, proof, rotation)
	if err != nil {
		return auth.TOTPEnrollment{}, err
	}
	var enrollment auth.TOTPEnrollment
	if rotation {
		enrollment, err = b.mfa.BeginTOTPRotation(r.Context(), step.Subject(), issuer, step.Login(), step.Proof())
	} else {
		enrollment, err = b.mfa.BeginTOTPEnrollment(r.Context(), step.Subject(), issuer, step.Login())
	}
	if err != nil {
		if _, applied := logSecurityAuditFailure(r, err); !applied {
			return auth.TOTPEnrollment{}, b.stepUpCodeSpent(r, action, step, err)
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
func (b *Browser) ConfirmMFAEnrollment(w http.ResponseWriter, r *http.Request, issuer string, proof auth.MFAStepUp, code string) ([]auth.MFABackupCode, error) {
	return b.confirmMFAChange(w, r, issuer, proof, code, false)
}

// ConfirmMFARotation replaces the authenticator with the pending key after
// the password and a current code of the old factor (proof.Code, used up),
// then code of the new key. Results are ConfirmMFAEnrollment's.
func (b *Browser) ConfirmMFARotation(w http.ResponseWriter, r *http.Request, issuer string, proof auth.MFAStepUp, code string) ([]auth.MFABackupCode, error) {
	return b.confirmMFAChange(w, r, issuer, proof, code, true)
}

func (b *Browser) confirmMFAChange(w http.ResponseWriter, r *http.Request, issuer string, proof auth.MFAStepUp, code string, rotation bool) ([]auth.MFABackupCode, error) {
	action := auth.SecurityMFAEnabled
	if rotation {
		action = auth.SecurityMFARotated
	}
	step, err := b.stepUp(w, r, action, proof, rotation)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	var confirmation auth.TOTPConfirmation
	if rotation {
		// A backup code given as the current code is used up only if the
		// new key is installed: a wrong code of it keeps the backup code.
		confirmation, err = b.mfa.ConfirmTOTPRotation(ctx, step.Subject(), code, step.Proof())
	} else {
		confirmation, err = b.mfa.ConfirmTOTPEnrollment(ctx, step.Subject(), code)
	}
	if err != nil {
		if _, applied := logSecurityAuditFailure(r, err); !applied {
			if !errors.Is(err, auth.ErrMFAEnrollmentInvalid) {
				return nil, b.stepUpCodeSpent(r, action, step, err)
			}
			// The step-up counter is not reset: wrong codes of the new key
			// count against it.
			b.stepUpGate.Denied(r.Context(), action, step, auditFailureLogger(r))
			// Only a pending key of this kind: a rotation's while the
			// account has a factor, an enrollment's otherwise.
			enrollment, found, pendingErr := b.mfa.PendingTOTP(ctx, step.Subject(), issuer, step.Login(), rotation)
			if pendingErr != nil {
				return nil, pendingErr
			}
			if !found {
				return nil, auth.ErrMFAEnrollmentInvalid
			}
			return nil, &auth.MFAPendingCodeError{Enrollment: enrollment}
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
func (b *Browser) RegenerateMFABackupCodes(w http.ResponseWriter, r *http.Request, proof auth.MFAStepUp) ([]auth.MFABackupCode, error) {
	step, err := b.stepUp(w, r, auth.SecurityMFABackupCodesRegenerated, proof, true)
	if err != nil {
		return nil, err
	}
	codes, err := b.mfa.RegenerateMFABackupCodes(r.Context(), step.Subject(), step.Proof())
	if err != nil {
		if _, applied := logSecurityAuditFailure(r, err); !applied {
			return nil, b.stepUpCodeSpent(r, auth.SecurityMFABackupCodesRegenerated, step, err)
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
func (b *Browser) DisableMFA(w http.ResponseWriter, r *http.Request, proof auth.MFAStepUp) error {
	step, err := b.stepUp(w, r, auth.SecurityMFADisabled, proof, true)
	if err != nil {
		return err
	}
	if err := b.mfa.DisableMFA(r.Context(), step.Subject(), step.Proof()); err != nil {
		if _, applied := logSecurityAuditFailure(r, err); !applied {
			return b.stepUpCodeSpent(r, auth.SecurityMFADisabled, step, err)
		}
	}
	clearSessionCookies(w)
	b.stepUpSucceeded(r, step)
	return nil
}

// stepUp re-authenticates the signed-in account for action through the
// MFAStepUpGate after the CSRF token (see MFAStepUpGate.Check), with the
// request's network source, and sets Retry-After on a throttled or
// saturated refusal.
func (b *Browser) stepUp(w http.ResponseWriter, r *http.Request, action auth.SecurityEventKind, proof auth.MFAStepUp, needCode bool) (auth.MFAStepUpPass, error) {
	noStore(w)
	if !csrf.Verified(r) {
		return auth.MFAStepUpPass{}, ErrCSRFInvalid
	}
	if !b.MFASettingsAvailable() {
		return auth.MFAStepUpPass{}, auth.ErrMFAUnavailable
	}
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		return auth.MFAStepUpPass{}, auth.ErrCredentialsRequired
	}
	step, err := b.stepUpGate.Check(r.Context(), principal.SubjectID(), requestSource(r), action, proof, needCode, auditFailureLogger(r))
	if err != nil {
		var throttled *auth.LoginThrottledError
		if errors.As(err, &throttled) {
			w.Header().Set("Retry-After", retryAfterHeader(throttled.RetryAfter))
		}
		setPasswordWorkRetryAfter(w, err)
		return auth.MFAStepUpPass{}, err
	}
	return step, nil
}

// stepUpCodeSpent is MFAStepUpGate.Refused for this request.
func (b *Browser) stepUpCodeSpent(r *http.Request, action auth.SecurityEventKind, step auth.MFAStepUpPass, err error) error {
	return b.stepUpGate.Refused(r.Context(), action, step, err, auditFailureLogger(r))
}

// stepUpSucceeded resets the step-up counter after a completed change. A
// failed reset only leaves the counter higher, so it is logged, not returned:
// the change (and any one-time backup codes) already happened.
func (b *Browser) stepUpSucceeded(r *http.Request, step auth.MFAStepUpPass) {
	if err := b.stepUpGate.Succeeded(r.Context(), step); err != nil {
		web.Logger(r.Context()).Error("MFA step-up throttle reset failed", "path", r.URL.Path, "error", err)
	}
}

// auditFailureLogger logs an audit failure of a refusal in the request log.
func auditFailureLogger(r *http.Request) func(error) {
	return func(err error) { _, _ = logSecurityAuditFailure(r, err) }
}
