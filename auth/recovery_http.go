package auth

import (
	"errors"
	"net/http"

	"github.com/rocketinsidecorp/forge-go/web"
)

type recoveryRequestBody struct {
	Email      string `json:"email"`
	TenantSlug string `json:"tenant"`
}

type recoveryAcceptedResponse struct {
	Status string `json:"status"`
}

type passwordResetBody struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

func NewPasswordRecoveryRequestHandler(service *RecoveryService) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("recovery service is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recoveryNoStore(w)
		var request recoveryRequestBody
		if err := web.DecodeJSON(r, &request); err != nil {
			web.Error(w, r, err)
			return
		}
		err := service.Request(r.Context(), RecoveryRequest{
			Email:      request.Email,
			TenantSlug: request.TenantSlug,
			Source:     requestSource(r),
		})
		if err != nil {
			auditFailure, _ := logSecurityAuditFailure(r, err)
			var throttled *RecoveryThrottledError
			var delivery *RecoveryDeliveryError
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
	}), nil
}

func NewPasswordResetHandler(service *RecoveryService) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("recovery service is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recoveryNoStore(w)
		var request passwordResetBody
		if err := web.DecodeJSON(r, &request); err != nil {
			web.Error(w, r, err)
			return
		}
		if err := service.Reset(r.Context(), PasswordReset{
			Token:       request.Token,
			NewPassword: request.NewPassword,
			Source:      requestSource(r),
		}); err != nil {
			_, operationApplied := logSecurityAuditFailure(r, err)
			if operationApplied {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			var throttled *RecoveryThrottledError
			if errors.As(err, &throttled) {
				w.Header().Set("Retry-After", retryAfterHeader(throttled.RetryAfter))
			}
			web.Error(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), nil
}

func recoveryNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

// NewAuditedPasswordRecoveryRequestHandler is NewPasswordRecoveryRequestHandler with structured security audit.
func NewAuditedPasswordRecoveryRequestHandler(service *RecoveryService, auditor SecurityAuditor) (http.Handler, error) {
	if auditor == nil {
		return nil, errors.New("security auditor is required")
	}
	return newPasswordRecoveryRequestHandler(service, auditor)
}

func newPasswordRecoveryRequestHandler(service *RecoveryService, auditor SecurityAuditor) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("recovery service is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recoveryNoStore(w)
		var request recoveryRequestBody
		if err := web.DecodeJSON(r, &request); err != nil {
			web.Error(w, r, err)
			return
		}
		err := service.Request(r.Context(), RecoveryRequest{
			Email:      request.Email,
			TenantSlug: request.TenantSlug,
			Source:     requestSource(r),
		})
		if err != nil {
			var throttled *RecoveryThrottledError
			var delivery *RecoveryDeliveryError
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
					if auditErr := auditor.RecordSecurityEvent(r.Context(), SecurityEvent{
						Kind:    SecurityRecoveryRequested,
						Outcome: SecurityOutcomeFailed,
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
			if auditErr := auditor.RecordSecurityEvent(r.Context(), SecurityEvent{
				Kind:    SecurityRecoveryRequested,
				Outcome: SecurityOutcomeSucceeded,
			}); auditErr != nil {
				web.Logger(r.Context()).Error("security audit failed",
					"method", r.Method,
					"path", r.URL.Path,
					"error", auditErr,
				)
			}
		}
		web.JSON(w, http.StatusAccepted, recoveryAcceptedResponse{Status: "accepted"})
	}), nil
}

// NewAuditedPasswordResetHandler is NewPasswordResetHandler with structured security audit.
func NewAuditedPasswordResetHandler(service *RecoveryService, auditor SecurityAuditor) (http.Handler, error) {
	if auditor == nil {
		return nil, errors.New("security auditor is required")
	}
	return newPasswordResetHandler(service, auditor)
}

func newPasswordResetHandler(service *RecoveryService, auditor SecurityAuditor) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("recovery service is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recoveryNoStore(w)
		var request passwordResetBody
		if err := web.DecodeJSON(r, &request); err != nil {
			web.Error(w, r, err)
			return
		}
		if err := service.Reset(r.Context(), PasswordReset{
			Token:       request.Token,
			NewPassword: request.NewPassword,
			Source:      requestSource(r),
		}); err != nil {
			var throttled *RecoveryThrottledError
			if errors.As(err, &throttled) {
				w.Header().Set("Retry-After", retryAfterHeader(throttled.RetryAfter))
			}
			if auditor != nil {
				if auditErr := auditor.RecordSecurityEvent(r.Context(), SecurityEvent{
					Kind:    SecurityPasswordReset,
					Outcome: SecurityOutcomeFailed,
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
			if auditErr := auditor.RecordSecurityEvent(r.Context(), SecurityEvent{
				Kind:    SecurityPasswordReset,
				Outcome: SecurityOutcomeSucceeded,
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
