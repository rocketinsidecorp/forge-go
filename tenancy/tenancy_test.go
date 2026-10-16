package tenancy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestContextRequiresExplicitValidTenant(t *testing.T) {
	t.Parallel()
	if _, err := tenancy.Require(context.Background()); !errors.Is(err, tenancy.ErrMissing) {
		t.Fatalf("missing tenant error=%v", err)
	}
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := tenancy.WithContext(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tenancy.Require(ctx)
	if err != nil || got != tenant {
		t.Fatalf("Require()=(%v, %v), want %v", got, err, tenant)
	}
}

func TestContextRejectsZeroTenant(t *testing.T) {
	t.Parallel()
	if _, err := tenancy.New(uuid.UUID{}); !errors.Is(err, tenancy.ErrInvalid) {
		t.Fatalf("New() error=%v", err)
	}
	if _, err := tenancy.WithContext(context.Background(), tenancy.Tenant{}); !errors.Is(err, tenancy.ErrInvalid) {
		t.Fatalf("WithContext() error=%v", err)
	}
}
