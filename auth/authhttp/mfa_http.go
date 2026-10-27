package authhttp

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/openapi"
	"github.com/rocketinsidecorp/forge-go/web"
)

type totpEnrollmentResponse struct {
	Secret          string    `json:"secret"`
	ProvisioningURI string    `json:"provisioning_uri"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type totpConfirmationRequest struct {
	Code string `json:"code"`
}

type totpConfirmationResponse struct {
	BackupCodes []string `json:"backup_codes"`
}

type mfaCompletionRequest struct {
	ChallengeToken string `json:"challenge_token" doc:"The challenge_token of the 202 sign-in answer."`
	Code           string `json:"code" doc:"A current authenticator code, or an unused backup code."`
}

// NewTOTPEnrollmentHandler starts TOTP enrollment for the authenticated
// principal only after an application-supplied step-up authorizer succeeds.
// The issuer is server-controlled; the subject UUID is used as the default
// authenticator label so the handler never trusts a client-supplied identity.
func NewTOTPEnrollmentHandler(service *auth.MFAService, issuer string, authorizer auth.MFAEnrollmentAuthorizer) (http.Handler, error) {
	if service == nil || authorizer == nil {
		return nil, errors.New("MFA service and enrollment authorizer are required")
	}
	issuer = strings.TrimSpace(issuer)
	if !auth.ValidTOTPIssuer(issuer) {
		return nil, errors.New("TOTP issuer is required and must not contain control characters")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		principal, ok := auth.FromContext(r.Context())
		if !ok {
			web.Error(w, r, auth.ErrCredentialsRequired)
			return
		}
		if err := authorizer.AuthorizeMFAEnrollment(r.Context(), principal); err != nil {
			web.Error(w, r, err)
			return
		}
		enrollment, err := service.BeginTOTPEnrollment(
			r.Context(),
			principal.SubjectID(),
			issuer,
			principal.SubjectID().String(),
		)
		if err != nil {
			_, operationApplied := logSecurityAuditFailure(r, err)
			if !operationApplied {
				web.Error(w, r, err)
				return
			}
		}
		web.JSON(w, http.StatusOK, totpEnrollmentResponse{
			Secret:          enrollment.Secret.Reveal(),
			ProvisioningURI: enrollment.ProvisioningURI.Reveal(),
			ExpiresAt:       enrollment.ExpiresAt,
		})
	}), nil
}

// NewTOTPConfirmationHandler activates the pending TOTP factor for the
// authenticated principal. Confirmation invalidates all existing sessions,
// including the session used for enrollment, and returns the one-time backup
// codes exactly once so the next login exercises MFA.
func NewTOTPConfirmationHandler(service *auth.MFAService) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("MFA service is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		principal, ok := auth.FromContext(r.Context())
		if !ok {
			web.Error(w, r, auth.ErrCredentialsRequired)
			return
		}
		var request totpConfirmationRequest
		if err := web.DecodeJSON(r, &request); err != nil {
			web.Error(w, r, err)
			return
		}
		confirmation, err := service.ConfirmTOTPEnrollment(r.Context(), principal.SubjectID(), request.Code)
		if err != nil {
			_, operationApplied := logSecurityAuditFailure(r, err)
			if !operationApplied {
				web.Error(w, r, err)
				return
			}
		}
		backupCodes := make([]string, len(confirmation.BackupCodes))
		for index, backupCode := range confirmation.BackupCodes {
			backupCodes[index] = backupCode.Reveal()
		}
		web.JSON(w, http.StatusOK, totpConfirmationResponse{BackupCodes: backupCodes})
	}), nil
}

// NewMFACompletionHandler exchanges a valid password-login challenge plus TOTP
// code for the normal opaque bearer session.
func NewMFACompletionHandler(service *auth.MFAService) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("MFA service is required")
	}
	return openapi.Describe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		var request mfaCompletionRequest
		if err := web.DecodeJSON(r, &request); err != nil {
			web.Error(w, r, err)
			return
		}
		result, err := service.CompleteLogin(r.Context(), auth.MFACompletion{
			ChallengeToken: request.ChallengeToken,
			Code:           request.Code,
			Source:         requestSource(r),
		})
		if err != nil {
			_, _ = logSecurityAuditFailure(r, err)
			var throttled *auth.LoginThrottledError
			if errors.As(err, &throttled) {
				w.Header().Set("Retry-After", retryAfterHeader(throttled.RetryAfter))
			}
			web.Error(w, r, err)
			return
		}
		web.JSON(w, http.StatusOK, loginResponse{
			AccessToken: result.Token.Reveal(),
			TokenType:   "Bearer",
			ExpiresAt:   result.ExpiresAt,
		})
	}), mfaCompletionOperation), nil
}

// NewTOTPRotationHandler starts replacement of an active TOTP factor. The
// current factor remains active until the new seed is confirmed.
func NewTOTPRotationHandler(service *auth.MFAService, issuer string, authorizer auth.MFAChangeAuthorizer) (http.Handler, error) {
	if service == nil || authorizer == nil {
		return nil, errors.New("MFA service and change authorizer are required")
	}
	issuer = strings.TrimSpace(issuer)
	if !auth.ValidTOTPIssuer(issuer) {
		return nil, errors.New("TOTP issuer is required and must not contain control characters")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		principal, ok := auth.FromContext(r.Context())
		if !ok {
			web.Error(w, r, auth.ErrCredentialsRequired)
			return
		}
		proof, err := auth.AuthorizeMFAChange(r.Context(), authorizer)
		if err != nil {
			web.Error(w, r, err)
			return
		}
		enrollment, err := service.BeginTOTPRotation(
			r.Context(),
			principal.SubjectID(),
			issuer,
			principal.SubjectID().String(),
			// The authorizer is these handlers' step-up.
			proof,
		)
		if err != nil {
			_, operationApplied := logSecurityAuditFailure(r, err)
			if !operationApplied {
				web.Error(w, r, err)
				return
			}
		}
		web.JSON(w, http.StatusOK, totpEnrollmentResponse{
			Secret:          enrollment.Secret.Reveal(),
			ProvisioningURI: enrollment.ProvisioningURI.Reveal(),
			ExpiresAt:       enrollment.ExpiresAt,
		})
	}), nil
}

// NewTOTPRotationConfirmationHandler activates the pending replacement factor,
// creates a fresh backup-code set, and revokes all sessions.
func NewTOTPRotationConfirmationHandler(service *auth.MFAService, authorizer auth.MFAChangeAuthorizer) (http.Handler, error) {
	if service == nil || authorizer == nil {
		return nil, errors.New("MFA service and change authorizer are required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		principal, ok := auth.FromContext(r.Context())
		if !ok {
			web.Error(w, r, auth.ErrCredentialsRequired)
			return
		}
		proof, err := auth.AuthorizeMFAChange(r.Context(), authorizer)
		if err != nil {
			web.Error(w, r, err)
			return
		}
		var request totpConfirmationRequest
		if err := web.DecodeJSON(r, &request); err != nil {
			web.Error(w, r, err)
			return
		}
		confirmation, err := service.ConfirmTOTPRotation(r.Context(), principal.SubjectID(), request.Code, proof)
		if err != nil {
			_, operationApplied := logSecurityAuditFailure(r, err)
			if !operationApplied {
				web.Error(w, r, err)
				return
			}
		}
		backupCodes := make([]string, len(confirmation.BackupCodes))
		for index, backupCode := range confirmation.BackupCodes {
			backupCodes[index] = backupCode.Reveal()
		}
		web.JSON(w, http.StatusOK, totpConfirmationResponse{BackupCodes: backupCodes})
	}), nil
}

// NewMFADisableHandler disables the active factor only after application-
// supplied step-up authorization. Disabling MFA revokes all current sessions.
func NewMFADisableHandler(service *auth.MFAService, authorizer auth.MFAChangeAuthorizer) (http.Handler, error) {
	if service == nil || authorizer == nil {
		return nil, errors.New("MFA service and change authorizer are required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		principal, ok := auth.FromContext(r.Context())
		if !ok {
			web.Error(w, r, auth.ErrCredentialsRequired)
			return
		}
		proof, err := auth.AuthorizeMFAChange(r.Context(), authorizer)
		if err != nil {
			web.Error(w, r, err)
			return
		}
		if err := service.DisableMFA(r.Context(), principal.SubjectID(), proof); err != nil {
			_, operationApplied := logSecurityAuditFailure(r, err)
			if !operationApplied {
				web.Error(w, r, err)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}), nil
}

// NewAuditedTOTPEnrollmentHandler is NewTOTPEnrollmentHandler with structured security audit.
func NewAuditedTOTPEnrollmentHandler(service *auth.MFAService, issuer string, authorizer auth.MFAEnrollmentAuthorizer, auditor auth.SecurityAuditor) (http.Handler, error) {
	if auditor == nil {
		return nil, errors.New("security auditor is required")
	}
	return newTOTPEnrollmentHandler(service, issuer, authorizer, auditor)
}

func newTOTPEnrollmentHandler(service *auth.MFAService, issuer string, authorizer auth.MFAEnrollmentAuthorizer, auditor auth.SecurityAuditor) (http.Handler, error) {
	if service == nil || authorizer == nil {
		return nil, errors.New("MFA service and enrollment authorizer are required")
	}
	issuer = strings.TrimSpace(issuer)
	if !auth.ValidTOTPIssuer(issuer) {
		return nil, errors.New("TOTP issuer is required and must not contain control characters")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		principal, ok := auth.FromContext(r.Context())
		if !ok {
			web.Error(w, r, auth.ErrCredentialsRequired)
			return
		}
		if err := authorizer.AuthorizeMFAEnrollment(r.Context(), principal); err != nil {
			web.Error(w, r, err)
			return
		}
		enrollment, err := service.BeginTOTPEnrollment(
			r.Context(),
			principal.SubjectID(),
			issuer,
			principal.SubjectID().String(),
		)
		if err != nil {
			if auditor != nil {
				if auditErr := auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
					Kind:      auth.SecurityMFAEnrollmentStarted,
					Outcome:   auth.SecurityOutcomeFailed,
					ActorID:   principal.SubjectID(),
					SubjectID: principal.SubjectID(),
				}); auditErr != nil {
					web.Logger(r.Context()).Error("security audit failed",
						"method", r.Method,
						"path", r.URL.Path,
						"error", auditErr,
					)
				}
			}
			web.Error(w, r, err)
			return
		}
		if auditor != nil {
			if auditErr := auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
				Kind:      auth.SecurityMFAEnrollmentStarted,
				Outcome:   auth.SecurityOutcomeSucceeded,
				ActorID:   principal.SubjectID(),
				SubjectID: principal.SubjectID(),
			}); auditErr != nil {
				web.Logger(r.Context()).Error("security audit failed",
					"method", r.Method,
					"path", r.URL.Path,
					"error", auditErr,
				)
			}
		}
		web.JSON(w, http.StatusOK, totpEnrollmentResponse{
			Secret:          enrollment.Secret.Reveal(),
			ProvisioningURI: enrollment.ProvisioningURI.Reveal(),
			ExpiresAt:       enrollment.ExpiresAt,
		})
	}), nil
}

// NewAuditedTOTPConfirmationHandler is NewTOTPConfirmationHandler with structured security audit.
func NewAuditedTOTPConfirmationHandler(service *auth.MFAService, auditor auth.SecurityAuditor) (http.Handler, error) {
	if auditor == nil {
		return nil, errors.New("security auditor is required")
	}
	return newTOTPConfirmationHandler(service, auditor)
}

func newTOTPConfirmationHandler(service *auth.MFAService, auditor auth.SecurityAuditor) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("MFA service is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		principal, ok := auth.FromContext(r.Context())
		if !ok {
			web.Error(w, r, auth.ErrCredentialsRequired)
			return
		}
		var request totpConfirmationRequest
		if err := web.DecodeJSON(r, &request); err != nil {
			web.Error(w, r, err)
			return
		}
		confirmation, err := service.ConfirmTOTPEnrollment(r.Context(), principal.SubjectID(), request.Code)
		if err != nil {
			if auditor != nil {
				if auditErr := auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
					Kind:      auth.SecurityMFAEnabled,
					Outcome:   auth.SecurityOutcomeFailed,
					ActorID:   principal.SubjectID(),
					SubjectID: principal.SubjectID(),
				}); auditErr != nil {
					web.Logger(r.Context()).Error("security audit failed",
						"method", r.Method,
						"path", r.URL.Path,
						"error", auditErr,
					)
				}
			}
			web.Error(w, r, err)
			return
		}
		if auditor != nil {
			if auditErr := auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
				Kind:      auth.SecurityMFAEnabled,
				Outcome:   auth.SecurityOutcomeSucceeded,
				ActorID:   principal.SubjectID(),
				SubjectID: principal.SubjectID(),
			}); auditErr != nil {
				web.Logger(r.Context()).Error("security audit failed",
					"method", r.Method,
					"path", r.URL.Path,
					"error", auditErr,
				)
			}
		}
		backupCodes := make([]string, len(confirmation.BackupCodes))
		for index, backupCode := range confirmation.BackupCodes {
			backupCodes[index] = backupCode.Reveal()
		}
		web.JSON(w, http.StatusOK, totpConfirmationResponse{BackupCodes: backupCodes})
	}), nil
}

// NewAuditedMFACompletionHandler is NewMFACompletionHandler with structured security audit.
func NewAuditedMFACompletionHandler(service *auth.MFAService, auditor auth.SecurityAuditor) (http.Handler, error) {
	if auditor == nil {
		return nil, errors.New("security auditor is required")
	}
	return newMFACompletionHandler(service, auditor)
}

func newMFACompletionHandler(service *auth.MFAService, auditor auth.SecurityAuditor) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("MFA service is required")
	}
	return openapi.Describe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		var request mfaCompletionRequest
		if err := web.DecodeJSON(r, &request); err != nil {
			web.Error(w, r, err)
			return
		}
		result, err := service.CompleteLogin(r.Context(), auth.MFACompletion{
			ChallengeToken: request.ChallengeToken,
			Code:           request.Code,
			Source:         requestSource(r),
		})
		if err != nil {
			var throttled *auth.LoginThrottledError
			if errors.As(err, &throttled) {
				w.Header().Set("Retry-After", retryAfterHeader(throttled.RetryAfter))
			}
			if auditor != nil {
				if auditErr := auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
					Kind:    auth.SecurityMFAChallengeFailed,
					Outcome: auth.SecurityOutcomeFailed,
				}); auditErr != nil {
					web.Logger(r.Context()).Error("security audit failed",
						"method", r.Method,
						"path", r.URL.Path,
						"error", auditErr,
					)
				}
			}
			web.Error(w, r, err)
			return
		}
		if auditor != nil {
			if auditErr := auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
				Kind:    auth.SecurityMFAChallengeSucceeded,
				Outcome: auth.SecurityOutcomeSucceeded,
			}); auditErr != nil {
				web.Logger(r.Context()).Error("security audit failed",
					"method", r.Method,
					"path", r.URL.Path,
					"error", auditErr,
				)
			}
		}
		web.JSON(w, http.StatusOK, loginResponse{
			AccessToken: result.Token.Reveal(),
			TokenType:   "Bearer",
			ExpiresAt:   result.ExpiresAt,
		})
	}), mfaCompletionOperation), nil
}

// NewAuditedTOTPRotationHandler is NewTOTPRotationHandler with structured security audit.
func NewAuditedTOTPRotationHandler(service *auth.MFAService, issuer string, authorizer auth.MFAChangeAuthorizer, auditor auth.SecurityAuditor) (http.Handler, error) {
	if auditor == nil {
		return nil, errors.New("security auditor is required")
	}
	return newTOTPRotationHandler(service, issuer, authorizer, auditor)
}

func newTOTPRotationHandler(service *auth.MFAService, issuer string, authorizer auth.MFAChangeAuthorizer, auditor auth.SecurityAuditor) (http.Handler, error) {
	if service == nil || authorizer == nil {
		return nil, errors.New("MFA service and change authorizer are required")
	}
	issuer = strings.TrimSpace(issuer)
	if !auth.ValidTOTPIssuer(issuer) {
		return nil, errors.New("TOTP issuer is required and must not contain control characters")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		principal, ok := auth.FromContext(r.Context())
		if !ok {
			web.Error(w, r, auth.ErrCredentialsRequired)
			return
		}
		proof, err := auth.AuthorizeMFAChange(r.Context(), authorizer)
		if err != nil {
			web.Error(w, r, err)
			return
		}
		enrollment, err := service.BeginTOTPRotation(
			r.Context(),
			principal.SubjectID(),
			issuer,
			principal.SubjectID().String(),
			// The authorizer is these handlers' step-up.
			proof,
		)
		if err != nil {
			if auditor != nil {
				if auditErr := auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
					Kind:      auth.SecurityMFARotationStarted,
					Outcome:   auth.SecurityOutcomeFailed,
					ActorID:   principal.SubjectID(),
					SubjectID: principal.SubjectID(),
				}); auditErr != nil {
					web.Logger(r.Context()).Error("security audit failed",
						"method", r.Method,
						"path", r.URL.Path,
						"error", auditErr,
					)
				}
			}
			web.Error(w, r, err)
			return
		}
		if auditor != nil {
			if auditErr := auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
				Kind:      auth.SecurityMFARotationStarted,
				Outcome:   auth.SecurityOutcomeSucceeded,
				ActorID:   principal.SubjectID(),
				SubjectID: principal.SubjectID(),
			}); auditErr != nil {
				web.Logger(r.Context()).Error("security audit failed",
					"method", r.Method,
					"path", r.URL.Path,
					"error", auditErr,
				)
			}
		}
		web.JSON(w, http.StatusOK, totpEnrollmentResponse{
			Secret:          enrollment.Secret.Reveal(),
			ProvisioningURI: enrollment.ProvisioningURI.Reveal(),
			ExpiresAt:       enrollment.ExpiresAt,
		})
	}), nil
}

// NewAuditedTOTPRotationConfirmationHandler is NewTOTPRotationConfirmationHandler with structured security audit.
func NewAuditedTOTPRotationConfirmationHandler(service *auth.MFAService, authorizer auth.MFAChangeAuthorizer, auditor auth.SecurityAuditor) (http.Handler, error) {
	if auditor == nil {
		return nil, errors.New("security auditor is required")
	}
	return newTOTPRotationConfirmationHandler(service, authorizer, auditor)
}

func newTOTPRotationConfirmationHandler(service *auth.MFAService, authorizer auth.MFAChangeAuthorizer, auditor auth.SecurityAuditor) (http.Handler, error) {
	if service == nil || authorizer == nil {
		return nil, errors.New("MFA service and change authorizer are required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		principal, ok := auth.FromContext(r.Context())
		if !ok {
			web.Error(w, r, auth.ErrCredentialsRequired)
			return
		}
		proof, err := auth.AuthorizeMFAChange(r.Context(), authorizer)
		if err != nil {
			web.Error(w, r, err)
			return
		}
		var request totpConfirmationRequest
		if err := web.DecodeJSON(r, &request); err != nil {
			web.Error(w, r, err)
			return
		}
		confirmation, err := service.ConfirmTOTPRotation(r.Context(), principal.SubjectID(), request.Code, proof)
		if err != nil {
			if auditor != nil {
				if auditErr := auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
					Kind:      auth.SecurityMFARotated,
					Outcome:   auth.SecurityOutcomeFailed,
					ActorID:   principal.SubjectID(),
					SubjectID: principal.SubjectID(),
				}); auditErr != nil {
					web.Logger(r.Context()).Error("security audit failed",
						"method", r.Method,
						"path", r.URL.Path,
						"error", auditErr,
					)
				}
			}
			web.Error(w, r, err)
			return
		}
		if auditor != nil {
			if auditErr := auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
				Kind:      auth.SecurityMFARotated,
				Outcome:   auth.SecurityOutcomeSucceeded,
				ActorID:   principal.SubjectID(),
				SubjectID: principal.SubjectID(),
			}); auditErr != nil {
				web.Logger(r.Context()).Error("security audit failed",
					"method", r.Method,
					"path", r.URL.Path,
					"error", auditErr,
				)
			}
		}
		backupCodes := make([]string, len(confirmation.BackupCodes))
		for index, backupCode := range confirmation.BackupCodes {
			backupCodes[index] = backupCode.Reveal()
		}
		web.JSON(w, http.StatusOK, totpConfirmationResponse{BackupCodes: backupCodes})
	}), nil
}

// NewAuditedMFADisableHandler is NewMFADisableHandler with structured security audit.
func NewAuditedMFADisableHandler(service *auth.MFAService, authorizer auth.MFAChangeAuthorizer, auditor auth.SecurityAuditor) (http.Handler, error) {
	if auditor == nil {
		return nil, errors.New("security auditor is required")
	}
	return newMFADisableHandler(service, authorizer, auditor)
}

func newMFADisableHandler(service *auth.MFAService, authorizer auth.MFAChangeAuthorizer, auditor auth.SecurityAuditor) (http.Handler, error) {
	if service == nil || authorizer == nil {
		return nil, errors.New("MFA service and change authorizer are required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		principal, ok := auth.FromContext(r.Context())
		if !ok {
			web.Error(w, r, auth.ErrCredentialsRequired)
			return
		}
		proof, err := auth.AuthorizeMFAChange(r.Context(), authorizer)
		if err != nil {
			web.Error(w, r, err)
			return
		}
		if err := service.DisableMFA(r.Context(), principal.SubjectID(), proof); err != nil {
			if auditor != nil {
				if auditErr := auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
					Kind:      auth.SecurityMFADisabled,
					Outcome:   auth.SecurityOutcomeFailed,
					ActorID:   principal.SubjectID(),
					SubjectID: principal.SubjectID(),
				}); auditErr != nil {
					web.Logger(r.Context()).Error("security audit failed",
						"method", r.Method,
						"path", r.URL.Path,
						"error", auditErr,
					)
				}
			}
			web.Error(w, r, err)
			return
		}
		if auditor != nil {
			if auditErr := auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
				Kind:      auth.SecurityMFADisabled,
				Outcome:   auth.SecurityOutcomeSucceeded,
				ActorID:   principal.SubjectID(),
				SubjectID: principal.SubjectID(),
			}); auditErr != nil {
				web.Logger(r.Context()).Error("security audit failed",
					"method", r.Method,
					"path", r.URL.Path,
					"error", auditErr,
				)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}), nil
}
