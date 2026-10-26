// Package tenancy carries the current tenant through request and database
// boundaries. It is fail-closed: callers must explicitly require a tenant;
// there is no global or default tenant.
package tenancy

import (
	"context"
	"errors"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

var (
	// ErrMissing is the 403 error for an operation that needs a tenant when the context carries
	// none.
	ErrMissing = fault.New("tenant_required", "a tenant is required", fault.CategoryForbidden, 0)
	// ErrInvalid is returned for a tenant whose ID is not a UUIDv7.
	ErrInvalid = errors.New("tenant ID must be a UUIDv7")
)

// Tenant identifies the tenant a request or operation acts for.
type Tenant struct {
	ID uuid.UUID
}

// New returns the Tenant with the given ID, which must be a UUIDv7.
func New(id uuid.UUID) (Tenant, error) {
	if id.Version() != 7 || id.Variant() != 2 {
		return Tenant{}, ErrInvalid
	}
	return Tenant{ID: id}, nil
}

type contextKey struct{}

// WithContext returns a context that carries tenant. The tenant ID must be a UUIDv7.
func WithContext(ctx context.Context, tenant Tenant) (context.Context, error) {
	if ctx == nil {
		return nil, errors.New("tenant context is required")
	}
	if tenant.ID.Version() != 7 || tenant.ID.Variant() != 2 {
		return nil, ErrInvalid
	}
	return context.WithValue(ctx, contextKey{}, tenant), nil
}

// FromContext returns the tenant that WithContext stored in ctx. The boolean is false when there is
// none or its ID is not a UUIDv7.
func FromContext(ctx context.Context) (Tenant, bool) {
	if ctx == nil {
		return Tenant{}, false
	}
	tenant, ok := ctx.Value(contextKey{}).(Tenant)
	return tenant, ok && tenant.ID.Version() == 7 && tenant.ID.Variant() == 2
}

// Require returns the tenant in ctx, or ErrMissing: tenant-sensitive operations call it first and
// fail closed without a tenant.
func Require(ctx context.Context) (Tenant, error) {
	tenant, ok := FromContext(ctx)
	if !ok {
		return Tenant{}, ErrMissing
	}
	return tenant, nil
}
