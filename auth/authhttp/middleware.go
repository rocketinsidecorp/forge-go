package authhttp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/internal/principalctx/principalwrite"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/web"
)

// Middleware authenticates bearer-token requests: it resolves the session behind the Authorization
// header and puts the Principal and its tenant in the request context.
type Middleware struct {
	resolver auth.Resolver
	auditor  auth.SecurityAuditor
	now      func() time.Time
	// tenantOf is the tenant a principal acts in: Principal.Tenant. auth
	// never builds a principal without a tenant, so only the tests of this
	// package replace it, to prove such a principal is refused.
	tenantOf func(auth.Principal) tenancy.Tenant
}

// Option configures a Middleware.
type Option func(*Middleware) error

// WithClock sets the clock Middleware uses to check session expiry; the default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(m *Middleware) error {
		if now == nil {
			return errors.New("authentication clock is required")
		}
		m.now = now
		return nil
	}
}

// WithAuthenticationAuditor records rejected valid sessions and permission
// denials for requests that pass through this middleware.
func WithAuthenticationAuditor(auditor auth.SecurityAuditor) Option {
	return func(m *Middleware) error {
		if auditor == nil {
			return errors.New("authentication security auditor is required")
		}
		m.auditor = auditor
		return nil
	}
}

// NewMiddleware returns a Middleware that resolves sessions with resolver.
func NewMiddleware(resolver auth.Resolver, options ...Option) (*Middleware, error) {
	if resolver == nil {
		return nil, errors.New("session resolver is required")
	}
	middleware := &Middleware{resolver: resolver, now: time.Now, tenantOf: auth.Principal.Tenant}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("nil authentication option")
		}
		if err := option(middleware); err != nil {
			return nil, err
		}
	}
	return middleware, nil
}

// Authenticate wraps next so that it only runs for a request with a valid, unexpired bearer token.
// Otherwise it answers 401, or a generic 500 when the resolver fails. It panics when next is nil.
func (m *Middleware) Authenticate(next http.Handler) http.Handler {
	if next == nil {
		panic("auth: nil handler")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, err := bearer(r.Header.Values("Authorization"))
		if err != nil {
			unauthorized(w, r, err)
			return
		}
		ctx, err := m.authenticateContext(r, secret)
		if err != nil {
			if errors.Is(err, auth.ErrCredentialsInvalid) {
				unauthorized(w, r, auth.ErrCredentialsInvalid)
				return
			}
			web.Error(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx)) //nolint:contextcheck // the context comes from a helper that derives it from r.Context(); the analyzer cannot see through it
	})
}

func (m *Middleware) authenticateContext(r *http.Request, secret string) (context.Context, error) {
	token, err := auth.ParseToken(secret)
	if err != nil {
		return nil, auth.ErrCredentialsInvalid
	}
	session, found, err := m.resolver.ResolveSession(r.Context(), token.Digest())
	if err != nil {
		return nil, err
	}
	if !found || session.ExpiresAt.IsZero() || !m.now().Before(session.ExpiresAt) {
		m.recordRejectedSession(r, token.Digest())
		return nil, auth.ErrCredentialsInvalid
	}
	if session.Principal.SubjectID().Version() != 7 || session.Principal.SubjectID().Variant() != 2 {
		return nil, fault.New("internal_error", "internal server error", fault.CategoryInternal, 0).
			WithCause(errors.New("session resolver returned an invalid principal"))
	}
	ctx, err := tenancy.WithContext(r.Context(), m.tenantOf(session.Principal))
	if err != nil {
		return nil, fault.New("internal_error", "internal server error", fault.CategoryInternal, 0).WithCause(err)
	}
	ctx = principalwrite.With(ctx, session.Principal)
	return ctx, nil
}

// Require wraps next so that only a request whose Principal holds permission reaches it: it answers
// 401 without a Principal and 403 without the permission. Use it inside Middleware.Authenticate.
func Require(permission auth.Permission, next http.Handler) (http.Handler, error) {
	return require(permission, next, nil)
}

// RequireAudited is Require with a security-audit event for an authenticated
// principal denied the requested permission.
func RequireAudited(permission auth.Permission, next http.Handler, auditor auth.SecurityAuditor) (http.Handler, error) {
	if auditor == nil {
		return nil, errors.New("authorization security auditor is required")
	}
	return require(permission, next, auditor)
}

func require(permission auth.Permission, next http.Handler, auditor auth.SecurityAuditor) (http.Handler, error) {
	if _, err := auth.NewPermission(string(permission)); err != nil {
		return nil, err
	}
	if next == nil {
		return nil, errors.New("authorization handler is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.FromContext(r.Context())
		if !ok {
			unauthorized(w, r, auth.ErrCredentialsRequired)
			return
		}
		if !principal.Can(permission) {
			recordAuthorizationDenial(r, principal, auditor)
			web.Error(w, r, auth.ErrPermissionDenied)
			return
		}
		next.ServeHTTP(w, r)
	}), nil
}

func (m *Middleware) recordRejectedSession(r *http.Request, credentialDigest auth.Digest) {
	if m.auditor == nil {
		return
	}
	err := m.auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
		Kind:             auth.SecuritySessionRejected,
		Outcome:          auth.SecurityOutcomeDenied,
		CredentialDigest: credentialDigest,
	})
	logAuditFailure(r, err)
}

func recordAuthorizationDenial(r *http.Request, principal auth.Principal, auditor auth.SecurityAuditor) {
	if auditor == nil {
		return
	}
	err := auditor.RecordSecurityEvent(r.Context(), auth.SecurityEvent{
		Kind:    auth.SecurityAuthorizationDenied,
		Outcome: auth.SecurityOutcomeDenied,
		ActorID: principal.SubjectID(),
	})
	logAuditFailure(r, err)
}

func bearer(values []string) (string, error) {
	if len(values) == 0 {
		return "", auth.ErrCredentialsRequired
	}
	if len(values) != 1 || strings.Contains(values[0], ",") {
		return "", auth.ErrCredentialsInvalid
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || parts[0] != "Bearer" || parts[1] == "" {
		return "", auth.ErrCredentialsInvalid
	}
	return parts[1], nil
}

func unauthorized(w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="forge"`)
	web.Error(w, r, err)
}

// logAuditFailure logs err, a failure to record the security event of a
// refused request, in the request log; the refusal stands.
func logAuditFailure(r *http.Request, err error) {
	if err != nil {
		logSecurityAuditFailure(r, &auth.SecurityAuditError{Cause: err})
	}
}
