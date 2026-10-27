package auth

import "errors"

// DefaultTenantSlug is the slug of the one tenant of a single-tenant
// application (`forge new --auth`, decision D33). Forge identity always signs
// a user in to a tenant, so `setup --email` creates this tenant, hidden from
// the user: memberships, roles and permissions work exactly as in a
// multi-tenant application, and the sign-in page asks for no organization.
// `forge generate tenancy` turns it into the first organization of a
// multi-tenant application.
const DefaultTenantSlug = "default"

// WithSingleTenant makes every sign-in sign in to slug, normally
// DefaultTenantSlug: the one tenant of a single-organization application.
// The tenant a LoginInput names is ignored, so a client that posts another
// organization's slug still signs in (or fails) against slug only. Without
// this option a sign-in must name its tenant.
func WithSingleTenant(slug string) LoginOption {
	return func(service *LoginService) error {
		canonical, ok := canonicalTenant(slug)
		if !ok {
			return errors.New("single tenant must be a tenant slug (a lowercase letter, then 1 to 62 lowercase letters, digits or hyphens)")
		}
		service.singleTenant = canonical
		return nil
	}
}

// WithRecoverySingleTenant is WithSingleTenant for password recovery: every
// reset request is for an account of slug, whatever tenant it names.
func WithRecoverySingleTenant(slug string) RecoveryOption {
	return func(service *RecoveryService) error {
		canonical, ok := canonicalTenant(slug)
		if !ok {
			return errors.New("single recovery tenant must be a tenant slug (a lowercase letter, then 1 to 62 lowercase letters, digits or hyphens)")
		}
		service.singleTenant = canonical
		return nil
	}
}

// effectiveTenant is single when it is set, whatever slug the input names,
// and slug otherwise.
func effectiveTenant(slug, single string) string {
	if single != "" {
		return single
	}
	return slug
}
