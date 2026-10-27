package authhttp

import (
	"errors"
	"net/http"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/openapi"
	"github.com/rocketinsidecorp/forge-go/web"
)

type recoveryRequestBody struct {
	Email      string `json:"email" doc:"The account's login." example:"alice@example.com"`
	TenantSlug string `json:"tenant" doc:"The tenant the account belongs to: a lowercase slug." example:"acme"`
}

type recoveryAcceptedResponse struct {
	Status string `json:"status" doc:"Always accepted, whether or not an account matched."`
}

type passwordResetBody struct {
	Token       string `json:"token" doc:"The single-use recovery token from the reset link, a secret."`
	NewPassword string `json:"new_password" doc:"The new password."`
}

// NewPasswordRecoveryRequestHandler returns the handler that requests a reset message. It answers
// 202 whether or not an account matched, so it does not reveal which accounts exist.
func NewPasswordRecoveryRequestHandler(service *auth.RecoveryService) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("recovery service is required")
	}
	return openapi.Describe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recoveryNoStore(w)
		var request recoveryRequestBody
		if err := web.DecodeJSON(r, &request); err != nil {
			web.Error(w, r, err)
			return
		}
		err := service.Request(r.Context(), auth.RecoveryRequest{
			Email:      request.Email,
			TenantSlug: request.TenantSlug,
			Source:     requestSource(r),
		})
		if err != nil {
			auditFailure, _ := logSecurityAuditFailure(r, err)
			var throttled *auth.RecoveryThrottledError
			var delivery *auth.RecoveryDeliveryError
			switch {
			case errors.As(err, &throttled):
				w.Header().Set("Retry-After", retryAfterHeader(throttled.RetryAfter))
				web.Error(w, r, err)
				return
			case errors.As(err, &delivery):
				// Delivery failures are account-dependent. Preserve a
				// non-enumerating public response; sender implementations
				// remain responsible for logging/alerting their failure.
			case auditFailure:
				// Audit persistence is also account-dependent for known
				// identities. Keep the public accepted response uniform.
			default:
				web.Error(w, r, err)
				return
			}
		}
		web.JSON(w, http.StatusAccepted, recoveryAcceptedResponse{Status: "accepted"})
	}), passwordRecoveryRequestOperation), nil
}

// NewPasswordResetHandler returns the handler that completes a reset with the token and the new
// password; it answers 204.
func NewPasswordResetHandler(service *auth.RecoveryService) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("recovery service is required")
	}
	return openapi.Describe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recoveryNoStore(w)
		var request passwordResetBody
		if err := web.DecodeJSON(r, &request); err != nil {
			web.Error(w, r, err)
			return
		}
		if err := service.Reset(r.Context(), auth.PasswordReset{
			Token:       request.Token,
			NewPassword: request.NewPassword,
			Source:      requestSource(r),
		}); err != nil {
			_, operationApplied := logSecurityAuditFailure(r, err)
			if operationApplied {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			var throttled *auth.RecoveryThrottledError
			if errors.As(err, &throttled) {
				w.Header().Set("Retry-After", retryAfterHeader(throttled.RetryAfter))
			}
			setPasswordWorkRetryAfter(w, err)
			web.Error(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), passwordResetOperation), nil
}

func recoveryNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

// NewAuditedPasswordRecoveryRequestHandler is NewPasswordRecoveryRequestHandler with structured security audit.
func NewAuditedPasswordRecoveryRequestHandler(service *auth.RecoveryService, auditor auth.SecurityAuditor) (http.Handler, error) {
	if auditor == nil {
		return nil, errors.New("security auditor is required")
	}
	return newPasswordRecoveryRequestHandler(service, auditor)
}

func newPasswordRecoveryRequestHandler(service *auth.RecoveryService, auditor auth.SecurityAuditor) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("recovery service is required")
	}
	return openapi.Describe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recoveryNoStore(w)
		var request recoveryRequestBody
		if err := web.DecodeJSON(r, &request); err != nil {
			web.Error(w, r, err)
			return
		}
		err := service.Request(r.Context(), auth.RecoveryRequest{
			Email:      request.Email,
			TenantSlug: request.TenantSlug,
			Source:     requestSource(r),
		})
		if err != nil {
			var throttled *auth.RecoveryThrottledError
			var delivery *auth.RecoveryDeliveryError
			switch {
			case errors.As(err, &throttled):
				w.Header().Set("Retry-After", retryAfterHeader(throttled.RetryAfter))
				web.Error(w, r, err)
				return
			case errors.As(err, &delivery):
				// Delivery failures are account-dependent. Preserve a
				// non-enumerating public response; sender implementations
				// remain responsible for logging/alerting their failure.
				if auditor != nil {
					if auditErr := auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
						Kind:    auth.SecurityRecoveryRequested,
						Outcome: auth.SecurityOutcomeFailed,
					}); auditErr != nil {
						web.Logger(r.Context()).Error("security audit failed",
							"method", r.Method,
							"path", r.URL.Path,
							"error", auditErr,
						)
					}
				}
				web.JSON(w, http.StatusAccepted, recoveryAcceptedResponse{Status: "accepted"})
				return
			default:
				web.Error(w, r, err)
				return
			}
		}
		if auditor != nil {
			if auditErr := auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
				Kind:    auth.SecurityRecoveryRequested,
				Outcome: auth.SecurityOutcomeSucceeded,
			}); auditErr != nil {
				web.Logger(r.Context()).Error("security audit failed",
					"method", r.Method,
					"path", r.URL.Path,
					"error", auditErr,
				)
			}
		}
		web.JSON(w, http.StatusAccepted, recoveryAcceptedResponse{Status: "accepted"})
	}), passwordRecoveryRequestOperation), nil
}

// NewAuditedPasswordResetHandler is NewPasswordResetHandler with structured security audit.
func NewAuditedPasswordResetHandler(service *auth.RecoveryService, auditor auth.SecurityAuditor) (http.Handler, error) {
	if auditor == nil {
		return nil, errors.New("security auditor is required")
	}
	return newPasswordResetHandler(service, auditor)
}

func newPasswordResetHandler(service *auth.RecoveryService, auditor auth.SecurityAuditor) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("recovery service is required")
	}
	return openapi.Describe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recoveryNoStore(w)
		var request passwordResetBody
		if err := web.DecodeJSON(r, &request); err != nil {
			web.Error(w, r, err)
			return
		}
		if err := service.Reset(r.Context(), auth.PasswordReset{
			Token:       request.Token,
			NewPassword: request.NewPassword,
			Source:      requestSource(r),
		}); err != nil {
			var throttled *auth.RecoveryThrottledError
			if errors.As(err, &throttled) {
				w.Header().Set("Retry-After", retryAfterHeader(throttled.RetryAfter))
			}
			setPasswordWorkRetryAfter(w, err)
			if auditor != nil {
				if auditErr := auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
					Kind:    auth.SecurityPasswordReset,
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
				Kind:    auth.SecurityPasswordReset,
				Outcome: auth.SecurityOutcomeSucceeded,
			}); auditErr != nil {
				web.Logger(r.Context()).Error("security audit failed",
					"method", r.Method,
					"path", r.URL.Path,
					"error", auditErr,
				)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}), passwordResetOperation), nil
}
