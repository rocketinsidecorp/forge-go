package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/csrf"
	"github.com/rocketinsidecorp/forge-go/i18n"
	"github.com/rocketinsidecorp/forge-go/openapi"
	"github.com/rocketinsidecorp/forge-go/web"
)

// The CSRF cookie, header and token format are shared with package csrf, so
// a token issued at cookie login also protects HTML forms and vice versa.
const (
	SessionCookieName = "__Host-forge_session"
	CSRFCookieName    = csrf.CookieName
	CSRFHeaderName    = csrf.HeaderName
)

var ErrCSRFInvalid = csrf.ErrInvalid

type cookieLoginResponse struct {
	CSRFToken string    `json:"csrf_token" doc:"The CSRF token, also set in the CSRF cookie: send it back in the X-CSRF-Token header of unsafe cookie-authenticated requests."`
	ExpiresAt time.Time `json:"expires_at" doc:"When the session expires."`
}

// AuthenticateCookie authenticates only the secure Forge session cookie.
// It never falls back to Authorization, so applications choose bearer and
// browser-cookie transports explicitly per route.
func (m *Middleware) AuthenticateCookie(next http.Handler) http.Handler {
	if next == nil {
		panic("auth: nil handler")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, err := m.cookieContext(r)
		if err != nil {
			web.Error(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AuthenticateBrowser is AuthenticateCookie for server-rendered pages. When
// the session cookie is missing, invalid, revoked or expired and the client
// prefers HTML (web.PrefersHTML: browser navigations and form submissions),
// the request is answered with 303 See Other to signInPath instead of the
// JSON 401. For GET and HEAD the requested path and query travel in the
// ReturnToParameter query parameter (with the locale path prefix of an
// i18n.Translator.Middleware using PathPrefix put back), so the sign-in form
// can send the user back; the sign-in handler must validate it again with ReturnPath. Other
// methods are redirected without a return path, because the submitted form
// cannot be replayed. JSON and API clients keep AuthenticateCookie's 401, and
// resolver failures stay 500. Authenticated responses are marked
// Cache-Control: no-store before next runs; next may still set its own value.
// signInPath must be a local path without a query
// or fragment, such as "/session/new".
func (m *Middleware) AuthenticateBrowser(signInPath string, next http.Handler) (http.Handler, error) {
	if next == nil {
		return nil, errors.New("auth: browser handler is required")
	}
	if !web.IsLocalPath(signInPath) || strings.ContainsAny(signInPath, "?#") {
		return nil, errors.New("auth: sign-in path must be a local path without query or fragment")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, err := m.cookieContext(r)
		if err == nil {
			// Pages behind a session are personal: no shared or browser
			// cache may keep them after sign-out. Set before next runs, so a
			// handler can still choose its own (stricter or deliberate) value.
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		if !web.PrefersHTML(r) || (!errors.Is(err, ErrCredentialsRequired) && !errors.Is(err, ErrCredentialsInvalid)) {
			web.Error(w, r, err)
			return
		}
		target := signInPath
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if requested := browserReturnTo(r); requested != "" {
				target += "?" + url.Values{ReturnToParameter: {requested}}.Encode()
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		if err := web.Redirect(w, target); err != nil {
			web.Error(w, r, err)
		}
	}), nil
}

// browserReturnTo is the page a visitor asked for, as a local path or "". It
// is built from the stripped request URI, with the locale path prefix that
// i18n.Translator.Middleware removed (PathPrefix) put back, so the visitor
// returns to the page in the language they used. A prefixed path that is not
// a safe local path is not used: the unprefixed one is.
func browserReturnTo(r *http.Request) string {
	requested := r.URL.RequestURI()
	if !web.IsLocalPath(requested) {
		return ""
	}
	if prefix := i18n.PathPrefix(r.Context()); prefix != "" {
		if prefixed := prefix + requested; web.IsLocalPath(prefixed) {
			return prefixed
		}
	}
	return requested
}

// cookieContext authenticates exactly one non-empty session cookie.
func (m *Middleware) cookieContext(r *http.Request) (context.Context, error) {
	cookies := r.CookiesNamed(SessionCookieName)
	if len(cookies) == 0 {
		return nil, ErrCredentialsRequired
	}
	if len(cookies) != 1 || cookies[0].Value == "" {
		return nil, ErrCredentialsInvalid
	}
	return m.authenticateContext(r, cookies[0].Value)
}

// NewCookieLoginHandler authenticates a password login and delivers the
// resulting opaque credential only as an HttpOnly Secure __Host- cookie. This
// endpoint requires a same-origin HTTPS Origin header to prevent login CSRF.
// Non-browser/API clients should use NewLoginHandler instead.
func NewCookieLoginHandler(service *LoginService) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("login service is required")
	}
	return openapi.Describe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		csrfToken, err := startCookieSession(w, result)
		if err != nil {
			web.Error(w, r, err)
			return
		}
		web.JSON(w, http.StatusOK, cookieLoginResponse{CSRFToken: csrfToken, ExpiresAt: result.ExpiresAt})
	}), cookieLoginOperation), nil
}

// NewCookieMFACompletionHandler exchanges a valid MFA challenge for a secure
// browser session. Like cookie login, it requires an HTTPS same-origin Origin
// header because it creates ambient browser credentials.
func NewCookieMFACompletionHandler(service *MFAService) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("MFA service is required")
	}
	return openapi.Describe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		csrfToken, err := startCookieSession(w, result)
		if err != nil {
			web.Error(w, r, err)
			return
		}
		web.JSON(w, http.StatusOK, cookieLoginResponse{
			CSRFToken: csrfToken,
			ExpiresAt: result.ExpiresAt,
		})
	}), cookieMFACompletionOperation), nil
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
		if err := endCookieSession(w, r, revoker, auditor); err != nil {
			web.Error(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	guarded, err := RequireCSRF(handler)
	if err != nil {
		return nil, err
	}
	return openapi.Describe(guarded, cookieLogoutOperation), nil
}

// startCookieSession delivers a completed login (password or MFA) to the
// browser: the session token in the HttpOnly session cookie and a fresh CSRF
// token, which replaces any token issued before authentication. Every cookie
// login path goes through it, so the cookies are identical whichever handler
// signed the user in.
func startCookieSession(w http.ResponseWriter, result LoginResult) (csrfToken string, err error) {
	if result.MFARequired {
		return "", errors.New("auth: an MFA challenge is not a session")
	}
	csrfToken, err = newCSRFToken()
	if err != nil {
		return "", err
	}
	setSessionCookies(w, result.Token.Reveal(), csrfToken, result.ExpiresAt)
	return csrfToken, nil
}

// endCookieSession revokes the request's session cookie when it holds a
// syntactically valid token, records the revocation when auditor is set, and
// clears both browser cookies. Unknown or already-revoked tokens stay
// idempotent at the repository layer. Only a failed revocation is an error;
// the cookies are then left in place so the user can retry.
func endCookieSession(w http.ResponseWriter, r *http.Request, revoker SessionRevoker, auditor SecurityAuditor) error {
	cookies := r.CookiesNamed(SessionCookieName)
	if len(cookies) == 1 && cookies[0].Value != "" {
		token, err := ParseToken(cookies[0].Value)
		if err == nil {
			if err := revoker.RevokeSession(r.Context(), token); err != nil {
				return err
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
	return nil
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

func newCSRFToken() (string, error) { return csrf.NewToken() }

func parseCSRFToken(value string) ([]byte, error) { return csrf.ParseToken(value) }

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
