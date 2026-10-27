package auth

import (
	"context"
	"testing"
)

func TestSingleTenantOptions(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "A B", "x"} {
		if _, err := NewLoginService(&loginStoreStub{}, &sessionStub{}, WithSingleTenant(bad)); err == nil {
			t.Fatalf("default tenant %q accepted", bad)
		}
		if _, err := NewRecoveryService(&recoveryStoreStub{}, &recoverySenderStub{}, WithRecoverySingleTenant(bad)); err == nil {
			t.Fatalf("default recovery tenant %q accepted", bad)
		}
	}
	store := &loginStoreStub{}
	service, err := NewLoginService(store, &sessionStub{}, WithSingleTenant(" Default "))
	if err != nil {
		t.Fatal(err)
	}
	for input, want := range map[string]string{"": DefaultTenantSlug, "  ": DefaultTenantSlug, "acme": DefaultTenantSlug} {
		_, _ = service.Login(context.Background(), LoginInput{Email: "a@example.com", Password: "pw", TenantSlug: input, Source: "192.0.2.1"})
		if store.lookTenant != want {
			t.Fatalf("tenant %q looked up %q, want %q", input, store.lookTenant, want)
		}
	}
	recoveryStore := &recoveryStoreStub{}
	recovery, err := NewRecoveryService(recoveryStore, &recoverySenderStub{}, WithRecoveryResponseTime(0), WithRecoverySingleTenant(DefaultTenantSlug))
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Request(context.Background(), RecoveryRequest{Email: "a@example.com", Source: "192.0.2.1"}); err != nil {
		t.Fatal(err)
	}
	if recoveryStore.lookupTenant != DefaultTenantSlug {
		t.Fatalf("recovery looked up %q", recoveryStore.lookupTenant)
	}
	// Without the option an empty slug names no tenant.
	plain := &loginStoreStub{}
	service, err = NewLoginService(plain, &sessionStub{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = service.Login(context.Background(), LoginInput{Email: "a@example.com", Password: "pw", Source: "192.0.2.1"})
	if plain.lookTenant == DefaultTenantSlug {
		t.Fatal("an empty slug signed in to the default tenant without the option")
	}
}

// A single-tenant service ignores the tenant a client posts: login and
// recovery look up the single tenant only.
func TestSingleTenantIgnoresAPostedTenant(t *testing.T) {
	t.Parallel()
	store := &loginStoreStub{}
	service, err := NewLoginService(store, &sessionStub{}, WithSingleTenant(DefaultTenantSlug))
	if err != nil {
		t.Fatal(err)
	}
	for _, posted := range []string{"other", "acme", " Other ", "x"} {
		_, _ = service.Login(context.Background(), LoginInput{Email: "a@example.com", Password: "pw", TenantSlug: posted, Source: "192.0.2.1"})
		if store.lookTenant != DefaultTenantSlug {
			t.Fatalf("posting tenant %q looked up %q, want only %q", posted, store.lookTenant, DefaultTenantSlug)
		}
	}
	recoveryStore := &recoveryStoreStub{}
	recovery, err := NewRecoveryService(recoveryStore, &recoverySenderStub{}, WithRecoveryResponseTime(0), WithRecoverySingleTenant(DefaultTenantSlug))
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Request(context.Background(), RecoveryRequest{Email: "a@example.com", TenantSlug: "other", Source: "192.0.2.1"}); err != nil {
		t.Fatal(err)
	}
	if recoveryStore.lookupTenant != DefaultTenantSlug {
		t.Fatalf("a reset request posting another tenant looked up %q", recoveryStore.lookupTenant)
	}
}
