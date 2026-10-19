// Package csrf protects browser requests against cross-site request forgery
// with two independent checks: a double-submit token bound to the
// __Host-forge_csrf cookie, and cross-origin rejection based on the browser's
// Sec-Fetch-Site and Origin headers (net/http.CrossOriginProtection).
//
// The token is the one the auth package issues at cookie login (ADR 0012):
// 32 random bytes, base64url without padding, in a Secure, SameSite=Lax,
// host-only cookie that same-origin JavaScript may read. HTML forms send it
// in the authenticity_token field and scripts in the X-CSRF-Token header.
package csrf

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"sync"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/web"
)

const (
	// CookieName is the double-submit cookie shared with auth cookie login.
	CookieName = "__Host-forge_csrf"
	// HeaderName carries the token in script-initiated requests.
	HeaderName = "X-CSRF-Token"
	// FormField carries the token in application/x-www-form-urlencoded forms.
	FormField = web.CSRFTokenField

	tokenBytes = 32
)

var (
	// ErrInvalid rejects a missing, malformed or mismatched token.
	ErrInvalid = fault.New("csrf_invalid", "CSRF token is missing or invalid", fault.CategoryForbidden, 0)
	// ErrCrossOrigin rejects an unsafe request a browser marked as coming
	// from another origin.
	ErrCrossOrigin = fault.New("cross_origin_request", "cross-origin request rejected", fault.CategoryForbidden, 0)
	// ErrNotInstalled is returned by Token outside Protection.Handler.
	ErrNotInstalled = errors.New("csrf: protection middleware is not installed")
)

// NewToken returns a fresh 256-bit token in canonical form.
func NewToken() (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ParseToken decodes a token, accepting only the canonical unpadded
// base64url encoding of exactly 32 bytes. It returns ErrInvalid otherwise.
func ParseToken(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) != tokenBytes || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, ErrInvalid
	}
	return decoded, nil
}

// Match reports whether submitted is a canonical token equal to the
// canonical cookie token, comparing in constant time.
func Match(cookieToken, submitted string) bool {
	expected, err := ParseToken(cookieToken)
	if err != nil {
		return false
	}
	actual, err := ParseToken(submitted)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(expected, actual) == 1
}

// Options configures Protection.
type Options struct {
	// TrustedOrigins are origins ("https://admin.example.com") whose
	// cross-origin unsafe requests pass the origin check. Form and other
	// non-JSON requests from them still need a valid token; JSON and bearer
	// requests skip the token check (see Handler), so never combine a trusted
	// origin with credentialed CORS on cookie-authenticated JSON routes that
	// lack auth.RequireCSRF.
	TrustedOrigins []string
	// Exempt lists http.ServeMux patterns ("POST /webhooks/payments") that
	// skip both checks. Use it only for endpoints that authenticate requests
	// by other means, such as a signature. A pattern matches only the routes
	// it names: a subtree pattern ("POST /hooks/") does not exempt the path
	// without its trailing slash, and a form POST carrying a _method field is
	// never exempt, so a method override cannot turn an exempt POST into a
	// protected PUT, PATCH or DELETE.
	Exempt []string
}

// Protection is CSRF middleware. Build it with New.
type Protection struct {
	origins *http.CrossOriginProtection
	exempt  *http.ServeMux
}

// New validates options and builds the middleware.
func New(opts Options) (*Protection, error) {
	protection := &Protection{origins: http.NewCrossOriginProtection()}
	for _, origin := range opts.TrustedOrigins {
		if err := protection.origins.AddTrustedOrigin(origin); err != nil {
			return nil, fmt.Errorf("csrf: trusted origin: %w", err)
		}
	}
	if len(opts.Exempt) > 0 {
		protection.exempt = http.NewServeMux()
		for _, pattern := range opts.Exempt {
			if err := handlePattern(protection.exempt, pattern); err != nil {
				return nil, err
			}
		}
	}
	return protection, nil
}

func handlePattern(mux *http.ServeMux, pattern string) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("csrf: invalid exempt pattern %q: %v", pattern, recovered)
		}
	}()
	mux.Handle(pattern, exemptMarker)
	return nil
}

// exemptMarker is the only handler registered in the exempt mux. Matching it
// by identity, not by a non-empty pattern, excludes the redirect handlers
// ServeMux returns for "/tree" when only "/tree/" is registered and for
// unclean paths.
var exemptMarker = &exemptHandler{}

type exemptHandler struct{}

func (*exemptHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	http.NotFound(w, nil)
}

type stateKey struct{}

type state struct {
	mu    sync.Mutex
	token string
}

// Handler protects next.
//
// Safe methods (GET, HEAD, OPTIONS, TRACE) always pass; they must not change
// state. A valid CSRF cookie becomes the request's token (see Token).
//
// Unsafe methods must pass both checks unless their pattern is exempt:
//
//  1. Origin: a request the browser marks cross-origin (Sec-Fetch-Site, or an
//     Origin whose host differs from Host) is 403 cross_origin_request, unless
//     the origin is trusted. Requests without either header (non-browser
//     clients) pass this check.
//  2. Token: the X-CSRF-Token header, or, only when there is no such header
//     and the body is application/x-www-form-urlencoded, the
//     authenticity_token form field, must equal the cookie token (constant
//     time). Query-string values are never read. A missing, repeated,
//     malformed or mismatched token is 403 csrf_invalid. Requests a
//     cross-site page cannot produce skip this check: those with an
//     "Authorization: Bearer" header, and those with a JSON body
//     (application/json or +json), which browsers send cross-origin only
//     after a CORS preflight Forge does not grant. Cookie-authenticated JSON
//     routes still use auth.RequireCSRF.
//
// Rejections are answered in the JSON error format, or with a minimal HTML
// page when the client prefers HTML. Form bodies are read through
// web.ParseForm, so oversized or malformed bodies are rejected too.
func (p *Protection) Handler(next http.Handler) http.Handler {
	if next == nil {
		panic("csrf: nil handler")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := &state{token: cookieToken(r)}
		if !safeMethod(r.Method) && !p.exempted(r) {
			if err := p.verify(r, current.token); err != nil {
				reject(w, r, err)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), stateKey{}, current)))
	})
}

func (p *Protection) exempted(r *http.Request) bool {
	if p.exempt == nil {
		return false
	}
	if handler, _ := p.exempt.Handler(r); handler != exemptMarker {
		return false
	}
	return !overridesMethod(r)
}

// overridesMethod reports whether r is a form POST carrying a method-override
// field, whatever its value. A body that cannot be parsed counts as one, so
// the request falls through to verification, which rejects it.
func overridesMethod(r *http.Request) bool {
	if r.Method != http.MethodPost || !web.IsForm(r) {
		return false
	}
	values, err := web.ParseForm(r)
	if err != nil {
		return true
	}
	_, present := values[web.MethodOverrideField]
	return present
}

func (p *Protection) verify(r *http.Request, cookie string) error {
	if err := p.origins.Check(r); err != nil {
		return ErrCrossOrigin.WithCause(err)
	}
	if bearerRequest(r) || jsonRequest(r) {
		return nil
	}
	if cookie == "" {
		return ErrInvalid
	}
	submitted, err := submittedToken(r)
	if err != nil {
		return err
	}
	if !Match(cookie, submitted) {
		return ErrInvalid
	}
	return nil
}

func submittedToken(r *http.Request) (string, error) {
	if headers := r.Header.Values(HeaderName); len(headers) > 0 {
		if len(headers) != 1 {
			return "", ErrInvalid
		}
		return headers[0], nil
	}
	if !web.IsForm(r) {
		return "", ErrInvalid
	}
	values, err := web.ParseForm(r)
	if err != nil {
		return "", err
	}
	if fields := values[FormField]; len(fields) == 1 {
		return fields[0], nil
	}
	return "", ErrInvalid
}

// Token returns the request's CSRF token for a form field, meta tag or
// script. When the request has no valid CSRF cookie it creates a token and
// sets the cookie on w, so call it before the response is written (views
// render into a buffer first, so template helpers qualify). Only pages that
// embed a token issue the cookie; health checks, assets and API responses
// never carry one. A response carrying a token is marked
// Cache-Control: private so a shared cache cannot hand one user's token to
// another.
func Token(w http.ResponseWriter, r *http.Request) (string, error) {
	current, ok := r.Context().Value(stateKey{}).(*state)
	if !ok {
		return "", ErrNotInstalled
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.token == "" {
		token, err := NewToken()
		if err != nil {
			return "", err
		}
		http.SetCookie(w, cookieFor(token))
		current.token = token
	}
	if cacheControl := strings.ToLower(w.Header().Get("Cache-Control")); !strings.Contains(cacheControl, "private") && !strings.Contains(cacheControl, "no-store") {
		w.Header().Set("Cache-Control", "private")
	}
	return current.token, nil
}

func cookieFor(token string) *http.Cookie {
	// #nosec G124 -- the double-submit cookie is intentionally readable by same-origin JavaScript; Secure and SameSite are set.
	return &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		Secure:   true,
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	}
}

// cookieToken returns the single canonical CSRF cookie value, or "".
func cookieToken(r *http.Request) string {
	cookies := r.CookiesNamed(CookieName)
	if len(cookies) != 1 {
		return ""
	}
	if _, err := ParseToken(cookies[0].Value); err != nil {
		return ""
	}
	return cookies[0].Value
}

func safeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

func bearerRequest(r *http.Request) bool {
	headers := r.Header.Values("Authorization")
	if len(headers) != 1 {
		return false
	}
	scheme, credential, found := strings.Cut(headers[0], " ")
	return found && strings.EqualFold(scheme, "Bearer") && strings.TrimSpace(credential) != ""
}

func jsonRequest(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && (mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"))
}

const rejectionPage = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>%[1]d %[2]s</title></head>
<body><main><h1>%[2]s</h1><p>%[3]s</p></main></body>
</html>
`

func reject(w http.ResponseWriter, r *http.Request, err error) {
	public := fault.From(err)
	if public == nil {
		public, err = ErrInvalid, ErrInvalid
	}
	web.Logger(r.Context()).Warn("request rejected by CSRF protection", "method", r.Method, "path", r.URL.Path, "code", public.Code)
	if !web.PrefersHTML(r) {
		web.Error(w, r, err)
		return
	}
	status := public.Status()
	message := "The request could not be processed."
	if status == http.StatusForbidden {
		message = "The request could not be verified. Go back, reload the page and try again."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// #nosec G705 -- every value is a constant or a net/http status text; nothing comes from the request.
	_, _ = fmt.Fprintf(w, rejectionPage, status, http.StatusText(status), message)
}
