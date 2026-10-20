package auth

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/rocketinsidecorp/forge-go/web"
)

type loginRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	TenantSlug string `json:"tenant"`
}

type loginResponse struct {
	AccessToken string    `json:"access_token"`
	TokenType   string    `json:"token_type"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type mfaRequiredResponse struct {
	MFARequired    bool      `json:"mfa_required"`
	ChallengeToken string    `json:"challenge_token"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// NewLoginHandler returns a thin HTTP adapter over LoginService.
func NewLoginHandler(service *LoginService) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("login service is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request loginRequest
		if err := web.DecodeJSON(r, &request); err != nil {
			web.Error(w, r, err)
			return
		}
		result, err := service.Login(r.Context(), LoginInput{
			Email:      request.Email,
			Password:   request.Password,
			TenantSlug: request.TenantSlug,
			Source:     requestSource(r),
		})
		if err != nil {
			var auditFailure *SecurityAuditError
			if errors.As(err, &auditFailure) &&
				(errors.Is(err, ErrCredentialsInvalid) || errors.Is(err, ErrLoginThrottled)) {
				web.Logger(r.Context()).Error("security audit failed",
					"method", r.Method,
					"path", r.URL.Path,
					"error", auditFailure.Cause,
				)
			}
			var throttled *LoginThrottledError
			switch {
			case errors.As(err, &throttled):
				w.Header().Set("Retry-After", retryAfterHeader(throttled.RetryAfter))
				web.Error(w, r, err)
			case errors.Is(err, ErrCredentialsInvalid):
				unauthorized(w, r, ErrCredentialsInvalid)
			default:
				web.Error(w, r, err)
			}
			return
		}
		noStore(w)
		if result.MFARequired {
			web.JSON(w, http.StatusAccepted, mfaRequiredResponse{
				MFARequired:    true,
				ChallengeToken: result.MFAChallenge.Reveal(),
				ExpiresAt:      result.ExpiresAt,
			})
			return
		}
		web.JSON(w, http.StatusOK, loginResponse{
			AccessToken: result.Token.Reveal(),
			TokenType:   "Bearer",
			ExpiresAt:   result.ExpiresAt,
		})
	}), nil
}

// NewLogoutHandler returns an idempotent logout endpoint. A syntactically
// valid but unknown/already-revoked token still returns 204 and does not become
// a credential oracle.
func NewLogoutHandler(revoker SessionRevoker) (http.Handler, error) {
	return newLogoutHandler(revoker, nil)
}

// NewAuditedLogoutHandler is NewLogoutHandler with structured security audit.
// A failure to persist the audit event is logged but does not undo a successful
// revocation or change the idempotent 204 response.
func NewAuditedLogoutHandler(revoker SessionRevoker, auditor SecurityAuditor) (http.Handler, error) {
	if auditor == nil {
		return nil, errors.New("security auditor is required")
	}
	return newLogoutHandler(revoker, auditor)
}

func newLogoutHandler(revoker SessionRevoker, auditor SecurityAuditor) (http.Handler, error) {
	if revoker == nil {
		return nil, errors.New("session revoker is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, err := bearer(r.Header.Values("Authorization"))
		if err != nil {
			unauthorized(w, r, err)
			return
		}
		token, err := ParseToken(secret)
		if err != nil {
			unauthorized(w, r, ErrCredentialsInvalid)
			return
		}
		if err := revoker.RevokeSession(r.Context(), token); err != nil {
			web.Error(w, r, err)
			return
		}
		if auditor != nil {
			if err := auditor.RecordSecurityEvent(r.Context(), SecurityEvent{
				Kind:             SecuritySessionRevoked,
				Outcome:          SecurityOutcomeSucceeded,
				CredentialDigest: token.Digest(),
			}); err != nil {
				web.Logger(r.Context()).Error("security audit failed",
					"method", r.Method,
					"path", r.URL.Path,
					"error", err,
				)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}), nil
}

// requestSource is the network source throttles and audit events record:
// the client address the HTTP server resolved (web.ClientIP), which is the
// TCP peer unless the peer is a trusted proxy (FORGE_HTTP_TRUSTED_PROXIES,
// ADR 0037).
func requestSource(r *http.Request) string {
	return web.ClientIP(r)
}

func retryAfterHeader(duration time.Duration) string {
	seconds := int64(duration / time.Second)
	if duration%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(seconds, 10)
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}
