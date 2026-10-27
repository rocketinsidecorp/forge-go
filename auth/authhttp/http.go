package authhttp

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/openapi"
	"github.com/rocketinsidecorp/forge-go/web"
)

type loginRequest struct {
	Email      string `json:"email" doc:"The account's login." example:"alice@example.com"`
	Password   string `json:"password" doc:"The account's password."`
	TenantSlug string `json:"tenant" doc:"The tenant the account belongs to: a lowercase slug." example:"acme"`
}

type loginResponse struct {
	AccessToken string    `json:"access_token" doc:"Opaque bearer session token, a secret: send it as Authorization: Bearer."`
	TokenType   string    `json:"token_type" doc:"Always Bearer."`
	ExpiresAt   time.Time `json:"expires_at" doc:"When the session expires."`
}

type mfaRequiredResponse struct {
	MFARequired    bool      `json:"mfa_required" doc:"Always true: the sign-in is not complete."`
	ChallengeToken string    `json:"challenge_token" doc:"Single-use secret that, with a code, completes the sign-in at the MFA completion route."`
	ExpiresAt      time.Time `json:"expires_at" doc:"When the challenge expires."`
}

// NewLoginHandler returns a thin HTTP adapter over LoginService.
func NewLoginHandler(service *auth.LoginService) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("login service is required")
	}
	return openapi.Describe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request loginRequest
		if err := web.DecodeJSON(r, &request); err != nil {
			web.Error(w, r, err)
			return
		}
		result, err := service.Login(r.Context(), auth.LoginInput{
			Email:      request.Email,
			Password:   request.Password,
			TenantSlug: request.TenantSlug,
			Source:     requestSource(r),
		})
		if err != nil {
			var auditFailure *auth.SecurityAuditError
			if errors.As(err, &auditFailure) &&
				(errors.Is(err, auth.ErrCredentialsInvalid) || errors.Is(err, auth.ErrLoginThrottled)) {
				web.Logger(r.Context()).Error("security audit failed",
					"method", r.Method,
					"path", r.URL.Path,
					"error", auditFailure.Cause,
				)
			}
			var throttled *auth.LoginThrottledError
			switch {
			case errors.As(err, &throttled):
				w.Header().Set("Retry-After", retryAfterHeader(throttled.RetryAfter))
				web.Error(w, r, err)
			case errors.Is(err, auth.ErrCredentialsInvalid):
				unauthorized(w, r, auth.ErrCredentialsInvalid)
			default:
				setPasswordWorkRetryAfter(w, err)
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
	}), loginOperation), nil
}

// NewLogoutHandler returns an idempotent logout endpoint. A syntactically
// valid but unknown/already-revoked token still returns 204 and does not become
// a credential oracle.
func NewLogoutHandler(revoker auth.SessionRevoker) (http.Handler, error) {
	return newLogoutHandler(revoker, nil)
}

// NewAuditedLogoutHandler is NewLogoutHandler with structured security audit.
// A failure to persist the audit event is logged but does not undo a successful
// revocation or change the idempotent 204 response.
func NewAuditedLogoutHandler(revoker auth.SessionRevoker, auditor auth.SecurityAuditor) (http.Handler, error) {
	if auditor == nil {
		return nil, errors.New("security auditor is required")
	}
	return newLogoutHandler(revoker, auditor)
}

func newLogoutHandler(revoker auth.SessionRevoker, auditor auth.SecurityAuditor) (http.Handler, error) {
	if revoker == nil {
		return nil, errors.New("session revoker is required")
	}
	return openapi.Describe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, err := bearer(r.Header.Values("Authorization"))
		if err != nil {
			unauthorized(w, r, err)
			return
		}
		token, err := auth.ParseToken(secret)
		if err != nil {
			unauthorized(w, r, auth.ErrCredentialsInvalid)
			return
		}
		if err := revoker.RevokeSession(r.Context(), token); err != nil {
			web.Error(w, r, err)
			return
		}
		if auditor != nil {
			if err := auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
				Kind:             auth.SecuritySessionRevoked,
				Outcome:          auth.SecurityOutcomeSucceeded,
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
	}), logoutOperation), nil
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
