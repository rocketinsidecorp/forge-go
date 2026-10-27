// Package authhttp is the HTTP adapter of package auth: opaque session
// authentication over explicit bearer or secure-cookie transports
// (Middleware), deny-by-default permission checks (Require, RequireAudited),
// CSRF protection for cookie sessions (RequireCSRF), the JSON sign-in,
// sign-out, two-factor and password-recovery handlers with their OpenAPI
// operations, and Browser, the security-critical half of server-rendered
// authentication forms.
//
// The services, ports and rules it drives live in auth, which imports no
// HTTP package (ADR 0052). These names used to be in auth;
// only the import path and the package qualifier changed.
package authhttp
