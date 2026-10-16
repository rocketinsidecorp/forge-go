package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/web"
)

const (
	SessionCookieName = "__Host-forge_session"
	CSRFCookieName    = "__Host-forge_csrf"
	CSRFHeaderName    = "X-CSRF-Token"

	csrfTokenBytes = 32
)

var ErrCSRFInvalid = fault.New("csrf_invalid", "CSRF token is missing or invalid", fault.CategoryForbidden, 0)

type cookieLoginResponse struct {
	CSRFToken string    `json:"csrf_token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// AuthenticateCookie authenticates only the secure Forge session cookie.
// It never falls back to Authorization, so applications choose bearer and
// browser-cookie transports explicitly per route.
func (m *Middleware) AuthenticateCookie(next http.Handler) http.Handler {
	if next == nil {
		panic("auth: nil handler")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookies := r.CookiesNamed(SessionCookieName)
		if len(cookies) == 0 {
			web.Error(w, r, ErrCredentialsRequired)
			return
		}
		if len(cookies) != 1 || cookies[0].Value == "" {
			web.Error(w, r, ErrCredentialsInvalid)
			return
		}
		ctx, err := m.authenticateContext(r, cookies[0].Value)
		if err != nil {
			web.Error(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// NewCookieLoginHandler authenticates a password login and delivers the
// resulting opaque credential only as an HttpOnly Secure __Host- cookie. This
// endpoint requires a same-origin HTTPS Origin header to prevent login CSRF.
// Non-browser/API clients should use NewLoginHandler instead.
func NewCookieLoginHandler(service *LoginService) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("login service is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		if err := requireSecureSameOrigin(r); err != nil {
			web.Error(w, r, err)
			return
		}
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
			_, _ = logSecurityAuditFailure(r, err)
			var throttled *LoginThrottledError
			switch {
			case errors.As(err, &throttled):
				w.Header().Set("Retry-After", retryAfterHeader(throttled.RetryAfter))
				web.Error(w, r, err)
			case errors.Is(err, ErrCredentialsInvalid):
				web.Error(w, r, ErrCredentialsInvalid)
			default:
				web.Error(w, r, err)
			}
			return
		}
		if result.MFARequired {
			web.JSON(w, http.StatusAccepted, mfaRequiredResponse{
				MFARequired:    true,
				ChallengeToken: result.MFAChallenge.Reveal(),
				ExpiresAt:      result.ExpiresAt,
			})
			return
		}
		csrfToken, err := newCSRFToken()
		if err != nil {
			web.Error(w, r, err)
			return
		}
		setSessionCookies(w, result.Token.Reveal(), csrfToken, result.ExpiresAt)
		web.JSON(w, http.StatusOK, cookieLoginResponse{CSRFToken: csrfToken, ExpiresAt: result.ExpiresAt})
	}), nil
}

// NewCookieMFACompletionHandler exchanges a valid MFA challenge for a secure
// browser session. Like cookie login, it requires an HTTPS same-origin Origin
// header because it creates ambient browser credentials.
func NewCookieMFACompletionHandler(service *MFAService) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("MFA service is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		if err := requireSecureSameOrigin(r); err != nil {
			web.Error(w, r, err)
			return
		}
		var request mfaCompletionRequest
		if err := web.DecodeJSON(r, &request); err != nil {
			web.Error(w, r, err)
			return
		}
		result, err := service.CompleteLogin(r.Context(), MFACompletion{
			ChallengeToken: request.ChallengeToken,
			Code:           request.Code,
			Source:         requestSource(r),
		})
		if err != nil {
			_, _ = logSecurityAuditFailure(r, err)
			var throttled *LoginThrottledError
			if errors.As(err, &throttled) {
				w.Header().Set("Retry-After", retryAfterHeader(throttled.RetryAfter))
			}
			web.Error(w, r, err)
			return
		}
		csrfToken, err := newCSRFToken()
		if err != nil {
			web.Error(w, r, err)
			return
		}
		setSessionCookies(w, result.Token.Reveal(), csrfToken, result.ExpiresAt)
		web.JSON(w, http.StatusOK, cookieLoginResponse{
			CSRFToken: csrfToken,
			ExpiresAt: result.ExpiresAt,
		})
	}), nil
}

// RequireCSRF protects unsafe cookie-authenticated routes using a double-submit
// token: the __Host- CSRF cookie and X-CSRF-Token header must
// contain the same canonical 256-bit value. Safe methods pass through.
func RequireCSRF(next http.Handler) (http.Handler, error) {
	if next == nil {
		return nil, errors.New("CSRF handler is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := validateCSRF(r); err != nil {
			web.Error(w, r, err)
			return
		}
		next.ServeHTTP(w, r)
	}), nil
}

// NewCookieLogoutHandler revokes the session cookie, requires CSRF, and clears
// both browser credentials. Unknown or already-revoked syntactically valid
// sessions remain idempotent at the repository layer.
func NewCookieLogoutHandler(revoker SessionRevoker) (http.Handler, error) {
	return newCookieLogoutHandler(revoker, nil)
}

// NewAuditedCookieLogoutHandler is NewCookieLogoutHandler with structured
// security audit. Audit failures are logged without undoing a successful
// revocation or changing the idempotent 204 response.
func NewAuditedCookieLogoutHandler(revoker SessionRevoker, auditor SecurityAuditor) (http.Handler, error) {
	if auditor == nil {
		return nil, errors.New("security auditor is required")
	}
	return newCookieLogoutHandler(revoker, auditor)
}

func newCookieLogoutHandler(revoker SessionRevoker, auditor SecurityAuditor) (http.Handler, error) {
	if revoker == nil {
		return nil, errors.New("session revoker is required")
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookies := r.CookiesNamed(SessionCookieName)
		if len(cookies) == 1 && cookies[0].Value != "" {
			token, err := ParseToken(cookies[0].Value)
			if err == nil {
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
			}
		}
		clearSessionCookies(w)
		w.WriteHeader(http.StatusNoContent)
	})
	return RequireCSRF(handler)
}

func validateCSRF(r *http.Request) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return nil
	}
	cookies := r.CookiesNamed(CSRFCookieName)
	headers := r.Header.Values(CSRFHeaderName)
	if len(cookies) != 1 || len(headers) != 1 || cookies[0].Value == "" || headers[0] == "" || strings.Contains(headers[0], ",") {
		return ErrCSRFInvalid
	}
	cookieToken, err := parseCSRFToken(cookies[0].Value)
	if err != nil {
		return ErrCSRFInvalid
	}
	headerToken, err := parseCSRFToken(headers[0])
	if err != nil {
		return ErrCSRFInvalid
	}
	if subtle.ConstantTimeCompare(cookieToken, headerToken) != 1 {
		return ErrCSRFInvalid
	}
	return nil
}

func requireSecureSameOrigin(r *http.Request) error {
	origins := r.Header.Values("Origin")
	if len(origins) != 1 {
		return ErrCSRFInvalid
	}
	origin, err := url.Parse(origins[0])
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.Host != r.Host || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return ErrCSRFInvalid
	}
	return nil
}

func newCSRFToken() (string, error) {
	raw := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func parseCSRFToken(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) != csrfTokenBytes || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, ErrCSRFInvalid
	}
	return decoded, nil
}

func setSessionCookies(w http.ResponseWriter, sessionToken, csrfToken string, expiresAt time.Time) {
	expiresAt = expiresAt.UTC()
	// #nosec G124 -- Lax is the intentional SameSite policy; all cookie security flags are set explicitly.
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    sessionToken,
		Path:     "/",
		Expires:  expiresAt,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	// #nosec G124 -- JavaScript must read the CSRF double-submit cookie; Secure and SameSite remain enabled.
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    csrfToken,
		Path:     "/",
		Expires:  expiresAt,
		Secure:   true,
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookies(w http.ResponseWriter) {
	expired := time.Unix(1, 0).UTC()
	for _, cookie := range []*http.Cookie{
		// #nosec G124 -- Lax is the intentional SameSite policy; all cookie security flags are set explicitly.
		{Name: SessionCookieName, Path: "/", Expires: expired, MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode},
		// #nosec G124 -- the CSRF double-submit cookie is intentionally readable by JavaScript.
		{Name: CSRFCookieName, Path: "/", Expires: expired, MaxAge: -1, Secure: true, HttpOnly: false, SameSite: http.SameSiteLaxMode},
	} {
		http.SetCookie(w, cookie)
	}
}
