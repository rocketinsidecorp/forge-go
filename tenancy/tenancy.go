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
	ErrMissing = fault.New("tenant_required", "a tenant is required", fault.CategoryForbidden, 0)
	ErrInvalid = errors.New("tenant ID must be a UUIDv7")
)

type Tenant struct {
	ID uuid.UUID
}

func New(id uuid.UUID) (Tenant, error) {
	if id.Version() != 7 || id.Variant() != 2 {
		return Tenant{}, ErrInvalid
	}
	return Tenant{ID: id}, nil
}

type contextKey struct{}

func WithContext(ctx context.Context, tenant Tenant) (context.Context, error) {
	if ctx == nil {
		return nil, errors.New("tenant context is required")
	}
	if tenant.ID.Version() != 7 || tenant.ID.Variant() != 2 {
		return nil, ErrInvalid
	}
	return context.WithValue(ctx, contextKey{}, tenant), nil
}

func FromContext(ctx context.Context) (Tenant, bool) {
	if ctx == nil {
		return Tenant{}, false
	}
	tenant, ok := ctx.Value(contextKey{}).(Tenant)
	return tenant, ok && tenant.ID.Version() == 7 && tenant.ID.Variant() == 2
}

func Require(ctx context.Context) (Tenant, error) {
	tenant, ok := FromContext(ctx)
	if !ok {
		return Tenant{}, ErrMissing
	}
	return tenant, nil
}
