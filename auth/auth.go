// Package auth is Forge's authentication and authorization domain: opaque
// session tokens, principals and permissions, password hashing, sign-in,
// two-factor authentication, password recovery, throttling and the security
// audit, with the ports identity stores implement. Session resolvers must
// load current account, tenant membership and permissions on every request;
// long-lived role claims are deliberately not part of the token.
//
// auth speaks no HTTP: the bearer and cookie middleware, the JSON handlers,
// the browser forms (Browser), CSRF enforcement and the OpenAPI operations
// live in package auth/authhttp (ADR 0052).
package auth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/internal/principalctx"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
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
// permissions it currently holds. authhttp.Middleware stores it in the request context; read it
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

// FromContext returns the Principal that authhttp.Middleware stored in ctx.
func FromContext(ctx context.Context) (Principal, bool) {
	principal, ok := principalctx.Value(ctx).(Principal)
	return principal, ok
}
