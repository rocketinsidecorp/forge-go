# ADR 0012: Explicit secure-cookie sessions with double-submit CSRF

Status: accepted (Phase 3)

## Decision

Forge keeps bearer and browser-cookie authentication as separate route-level
transports. `auth.Middleware.Authenticate` reads only an
`Authorization: Bearer` credential. `AuthenticateCookie` reads only the
Forge session cookie. Neither silently falls back to the other.

Browser sessions use two fixed host-only cookies:

- `__Host-forge_session`: the opaque session token, with `Secure`,
  `HttpOnly`, `Path=/`, and `SameSite=Lax`;
- `__Host-forge_csrf`: a separate 256-bit random CSRF token, with `Secure`,
  `Path=/`, and `SameSite=Lax`, intentionally readable by same-origin
  JavaScript.

The `__Host-` prefix forbids a `Domain` attribute and requires a secure
host-wide cookie, preventing a less-trusted subdomain from setting a parent
domain cookie with the same name.

Unsafe browser requests are protected by `auth.RequireCSRF`. The
`X-CSRF-Token` header must contain exactly the same canonical 256-bit
base64url value as the CSRF cookie. The comparison is constant-time. Safe HTTP
methods (`GET`, `HEAD`, `OPTIONS`, `TRACE`) do not require the token and
must remain side-effect free.

Cookie login additionally requires exactly one HTTPS `Origin` header whose
host matches the request host. This protects the login endpoint itself from
login CSRF before a CSRF cookie exists. API/non-browser callers use the bearer
login endpoint instead.

Cookie login never returns the session credential in the response body. It
returns only the CSRF token and expiry, and both bearer and cookie login
success responses are marked `Cache-Control: no-store` and
`Pragma: no-cache`.

Cookie logout requires CSRF, revokes a syntactically valid session token when
present, clears both cookies, and remains idempotent for unknown/already
revoked sessions.

## Consequences

- Applications choose one authentication transport explicitly for each route.
- A browser session token is unavailable to JavaScript through normal DOM APIs.
- CSRF protection does not depend only on SameSite behavior.
- Cookie mode intentionally requires HTTPS; Forge does not provide an
  insecure-cookie production escape hatch.
- XSS can still act with the user's privileges and can read the CSRF token;
  CSRF defenses are not an XSS defense.
- Unsafe GET handlers remain an application bug because safe methods bypass
  CSRF checks.
- Forwarded metadata is accepted only from explicitly configured trusted
  proxy CIDRs. Cookie-login origin validation continues to compare the
  browser-supplied HTTPS Origin host to `Request.Host`.
