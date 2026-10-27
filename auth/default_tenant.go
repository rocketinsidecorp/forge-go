package auth

import (
	"errors"
	"strings"
)

// DefaultTenantSlug is the slug of the one tenant of a single-tenant
// application (`forge new --auth`, decision D33). Forge identity always signs
// a user in to a tenant, so `setup --email` creates this tenant, hidden from
// the user: memberships, roles and permissions work exactly as in a
// multi-tenant application, and the sign-in page asks for no organization.
// `forge generate tenancy` turns it into the first organization of a
// multi-tenant application.
const DefaultTenantSlug = "default"

// WithDefaultTenant makes a sign-in that names no tenant (an empty
// LoginInput.TenantSlug, as the sign-in form of a single-tenant application
// sends) sign in to slug, normally DefaultTenantSlug. A sign-in that names a
// tenant still uses that one. Without this option a sign-in must name its
// tenant.
func WithDefaultTenant(slug string) LoginOption {
	return func(service *LoginService) error {
		canonical, ok := canonicalTenant(slug)
		if !ok {
			return errors.New("default tenant must be a tenant slug (a lowercase letter, then 1 to 62 lowercase letters, digits or hyphens)")
		}
		service.defaultTenant = canonical
		return nil
	}
}

// WithRecoveryDefaultTenant is WithDefaultTenant for password recovery: a
// reset request that names no tenant is for an account of slug.
func WithRecoveryDefaultTenant(slug string) RecoveryOption {
	return func(service *RecoveryService) error {
		canonical, ok := canonicalTenant(slug)
		if !ok {
			return errors.New("default recovery tenant must be a tenant slug (a lowercase letter, then 1 to 62 lowercase letters, digits or hyphens)")
		}
		service.defaultTenant = canonical
		return nil
	}
}

// tenantOrDefault is slug, or fallback when slug is blank.
func tenantOrDefault(slug, fallback string) string {
	if fallback != "" && strings.TrimSpace(slug) == "" {
		return fallback
	}
	return slug
}
