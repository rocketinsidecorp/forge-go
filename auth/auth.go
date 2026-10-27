// Package auth provides opaque session authentication over explicit bearer or
// secure-cookie transports, CSRF protection for browser sessions, and deny-by-
// default permission checks. Session resolvers must load current account,
// tenant membership and permissions on every request; long-lived role claims
// are deliberately not part of the token.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/internal/principalctx"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/web"
)

var (
	// ErrCredentialsRequired is the 401 error for a request that carries no credentials.
	ErrCredentialsRequired = fault.New("credentials_required", "authentication is required", fault.CategoryUnauthorized, 0)
	// ErrCredentialsInvalid is the 401 error for a token or cookie that is malformed, unknown,
	// revoked or expired.
	ErrCredentialsInvalid = fault.New("credentials_invalid", "credentials are invalid or expired", fault.CategoryUnauthorized, 0)
	// ErrPermissionDenied is the 403 error for an authenticated principal that lacks a required
	// permission.
	ErrPermissionDenied = fault.New("permission_denied", "permission denied", fault.CategoryForbidden, 0)
	permissionPattern   = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,62}:[a-z][a-z0-9_.-]{0,62}$`)
)

// Permission is a capability in lowercase resource:action form, such as "posts:read".
type Permission string

// NewPermission checks that value uses the lowercase resource:action syntax and returns it as a
// Permission.
func NewPermission(value string) (Permission, error) {
	if !permissionPattern.MatchString(value) {
		return "", fmt.Errorf("permission %q must use resource:action lowercase syntax", value)
	}
	return Permission(value), nil
}

// Principal is the authenticated subject of a request: its user ID, the tenant it acts in and the
// permissions it currently holds. Middleware.Authenticate stores it in the request context; read it
// with FromContext.
type Principal struct {
	subjectID   uuid.UUID
	tenant      tenancy.Tenant
	permissions []Permission
}

// NewPrincipal builds a Principal. The subject and tenant IDs must be UUIDv7 and every permission
// valid; duplicate permissions are dropped and the rest sorted.
func NewPrincipal(subjectID uuid.UUID, tenant tenancy.Tenant, permissions ...Permission) (Principal, error) {
	if subjectID.Version() != 7 || subjectID.Variant() != 2 {
		return Principal{}, errors.New("principal subject ID must be a UUIDv7")
	}
	if tenant.ID.Version() != 7 || tenant.ID.Variant() != 2 {
		return Principal{}, tenancy.ErrInvalid
	}
	unique := make(map[Permission]struct{}, len(permissions))
	result := make([]Permission, 0, len(permissions))
	for _, permission := range permissions {
		if _, err := NewPermission(string(permission)); err != nil {
			return Principal{}, err
		}
		if _, exists := unique[permission]; !exists {
			unique[permission] = struct{}{}
			result = append(result, permission)
		}
	}
	slices.Sort(result)
	return Principal{subjectID: subjectID, tenant: tenant, permissions: result}, nil
}

// SubjectID returns the authenticated user's ID.
func (p Principal) SubjectID() uuid.UUID { return p.subjectID }

// Tenant returns the tenant the principal acts in.
func (p Principal) Tenant() tenancy.Tenant { return p.tenant }

// Permissions returns a sorted copy of the principal's permissions.
func (p Principal) Permissions() []Permission { return slices.Clone(p.permissions) }

// Can reports whether the principal holds permission.
func (p Principal) Can(permission Permission) bool {
	_, err := NewPermission(string(permission))
	return err == nil && slices.Contains(p.permissions, permission)
}

// Session is the current server-side session state returned by Resolver.
// Resolver must verify that the session, subject and tenant membership are
// active and must resolve permissions from current roles, not token claims.
type Session struct {
	Principal Principal
	ExpiresAt time.Time
}

// Resolver returns the current session for digest. found=false is an invalid
// credential; err is an infrastructure failure and becomes a generic 500.
type Resolver interface {
	ResolveSession(ctx context.Context, digest Digest) (session Session, found bool, err error)
}

// ResolverFunc adapts a function to Resolver.
type ResolverFunc func(context.Context, Digest) (Session, bool, error)

// ResolveSession calls fn.
func (fn ResolverFunc) ResolveSession(ctx context.Context, digest Digest) (Session, bool, error) {
	return fn(ctx, digest)
}

// Middleware authenticates bearer-token requests: it resolves the session behind the Authorization
// header and puts the Principal and its tenant in the request context.
type Middleware struct {
	resolver Resolver
	auditor  SecurityAuditor
	now      func() time.Time
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
func WithAuthenticationAuditor(auditor SecurityAuditor) Option {
	return func(m *Middleware) error {
		if auditor == nil {
			return errors.New("authentication security auditor is required")
		}
		m.auditor = auditor
		return nil
	}
}

// NewMiddleware returns a Middleware that resolves sessions with resolver.
func NewMiddleware(resolver Resolver, options ...Option) (*Middleware, error) {
	if resolver == nil {
		return nil, errors.New("session resolver is required")
	}
	middleware := &Middleware{resolver: resolver, now: time.Now}
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
			if errors.Is(err, ErrCredentialsInvalid) {
				unauthorized(w, r, ErrCredentialsInvalid)
				return
			}
			web.Error(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx)) //nolint:contextcheck // the context comes from a helper that derives it from r.Context(); the analyzer cannot see through it
	})
}

func (m *Middleware) authenticateContext(r *http.Request, secret string) (context.Context, error) {
	token, err := ParseToken(secret)
	if err != nil {
		return nil, ErrCredentialsInvalid
	}
	session, found, err := m.resolver.ResolveSession(r.Context(), token.Digest())
	if err != nil {
		return nil, err
	}
	if !found || session.ExpiresAt.IsZero() || !m.now().Before(session.ExpiresAt) {
		m.recordRejectedSession(r, token.Digest())
		return nil, ErrCredentialsInvalid
	}
	if session.Principal.subjectID.Version() != 7 || session.Principal.subjectID.Variant() != 2 {
		return nil, fault.New("internal_error", "internal server error", fault.CategoryInternal, 0).
			WithCause(errors.New("session resolver returned an invalid principal"))
	}
	ctx, err := tenancy.WithContext(r.Context(), session.Principal.tenant)
	if err != nil {
		return nil, fault.New("internal_error", "internal server error", fault.CategoryInternal, 0).WithCause(err)
	}
	ctx = withPrincipal(ctx, session.Principal)
	return ctx, nil
}

// Require wraps next so that only a request whose Principal holds permission reaches it: it answers
// 401 without a Principal and 403 without the permission. Use it inside Middleware.Authenticate.
func Require(permission Permission, next http.Handler) (http.Handler, error) {
	return require(permission, next, nil)
}

// RequireAudited is Require with a security-audit event for an authenticated
// principal denied the requested permission.
func RequireAudited(permission Permission, next http.Handler, auditor SecurityAuditor) (http.Handler, error) {
	if auditor == nil {
		return nil, errors.New("authorization security auditor is required")
	}
	return require(permission, next, auditor)
}

func require(permission Permission, next http.Handler, auditor SecurityAuditor) (http.Handler, error) {
	if _, err := NewPermission(string(permission)); err != nil {
		return nil, err
	}
	if next == nil {
		return nil, errors.New("authorization handler is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := FromContext(r.Context())
		if !ok {
			unauthorized(w, r, ErrCredentialsRequired)
			return
		}
		if !principal.Can(permission) {
			recordAuthorizationDenial(r, principal, auditor)
			web.Error(w, r, ErrPermissionDenied)
			return
		}
		next.ServeHTTP(w, r)
	}), nil
}

func withPrincipal(ctx context.Context, principal Principal) context.Context {
	return principalctx.With(ctx, principal)
}

// FromContext returns the Principal that Middleware.Authenticate stored in ctx.
func FromContext(ctx context.Context) (Principal, bool) {
	principal, ok := principalctx.Value(ctx).(Principal)
	return principal, ok
}

func (m *Middleware) recordRejectedSession(r *http.Request, credentialDigest Digest) {
	if m.auditor == nil {
		return
	}
	err := m.auditor.RecordSecurityEvent(r.Context(), SecurityEvent{
		Kind:             SecuritySessionRejected,
		Outcome:          SecurityOutcomeDenied,
		CredentialDigest: credentialDigest,
	})
	logSecurityAuditFailure(r, securityAuditError(err, false))
}

func recordAuthorizationDenial(r *http.Request, principal Principal, auditor SecurityAuditor) {
	if auditor == nil {
		return
	}
	err := auditor.RecordSecurityEvent(r.Context(), SecurityEvent{
		Kind:    SecurityAuthorizationDenied,
		Outcome: SecurityOutcomeDenied,
		ActorID: principal.SubjectID(),
	})
	logSecurityAuditFailure(r, securityAuditError(err, false))
}

func bearer(values []string) (string, error) {
	if len(values) == 0 {
		return "", ErrCredentialsRequired
	}
	if len(values) != 1 || strings.Contains(values[0], ",") {
		return "", ErrCredentialsInvalid
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || parts[0] != "Bearer" || parts[1] == "" {
		return "", ErrCredentialsInvalid
	}
	return parts[1], nil
}

func unauthorized(w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="forge"`)
	web.Error(w, r, err)
}
