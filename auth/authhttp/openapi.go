package authhttp

import (
	"net/http"

	"github.com/rocketinsidecorp/forge-go/openapi"
	"github.com/rocketinsidecorp/forge-go/web"
)

// The handlers that need no guard of their own describe themselves
// (openapi.Describe), so an application that registers them directly
// documents them. Operation IDs are left to the route's method and path.
// Examples are given only for the login and the tenant slug, never for a
// password, token or other secret.
//
// The TOTP enrollment, confirmation, rotation and disable handlers are not
// described: an application mounts them behind its own guard (the registered
// handler is then the guard, which hides a description) and the statuses of
// their step-up authorizers are the application's.

const authTag = "auth"

// errorResponse declares an error answered with web.ErrorBody that also sets
// headers, which an Operation's Errors cannot name.
func errorResponse(status int, description string, headers ...string) openapi.Response {
	return openapi.Response{Status: status, Description: description, Body: openapi.TypeOf[web.ErrorBody](), Headers: headers}
}

// originHeader is the header cookie sign-in requires: a same-origin HTTPS
// Origin, which keeps another site from signing a browser in (login CSRF).
type originHeader struct {
	Origin string `header:"Origin" openapi:"required" doc:"The page's own HTTPS origin, exactly one value (https://host). Browsers send it with POST requests; anything else is answered 403."`
}

// csrfHeader is the double-submit header of cookie-authenticated unsafe
// requests; its name is csrf.HeaderName (a test pins the tag to it).
type csrfHeader struct {
	Token string `header:"X-CSRF-Token" openapi:"required" doc:"The csrf_token of cookie sign-in. It must equal the CSRF cookie, which the browser sends with the session cookie; otherwise 403."`
}

var (
	loginOperation = openapi.Operation{
		Summary: "Sign in with a password",
		Description: "Verifies the email, password and tenant and answers 200 with an opaque bearer session token. " +
			"When the account has MFA enabled it answers 202 with a challenge instead, to complete at the MFA completion route. " +
			"Bad credentials always answer 401 (an unknown account cannot be told from a wrong password); attempts are throttled per account and per source. " +
			"Successful responses are sent with Cache-Control: no-store.",
		Tags:    []string{authTag},
		Request: openapi.TypeOf[loginRequest](),
		Responses: []openapi.Response{
			openapi.JSON[loginResponse](http.StatusOK),
			openapi.JSON[mfaRequiredResponse](http.StatusAccepted),
			errorResponse(http.StatusUnauthorized, "The credentials are invalid", "WWW-Authenticate"),
			errorResponse(http.StatusTooManyRequests, "Too many attempts; retry after the Retry-After seconds", "Retry-After"),
			errorResponse(http.StatusServiceUnavailable, "Password hashing is saturated; retry after the Retry-After seconds", "Retry-After"),
		},
	}

	logoutOperation = openapi.Operation{
		Summary: "Sign out",
		Description: "Revokes the bearer session named by the Authorization header. " +
			"It is idempotent: a well-formed token that is unknown or already revoked still answers 204.",
		Tags:          []string{authTag},
		Authenticated: true,
		Responses: []openapi.Response{
			openapi.Empty(http.StatusNoContent),
			errorResponse(http.StatusUnauthorized, "The Authorization header is missing or not a well-formed bearer token", "WWW-Authenticate"),
		},
	}

	mfaCompletionOperation = openapi.Operation{
		Summary: "Complete a sign-in that needs MFA",
		Description: "Exchanges the challenge of a 202 sign-in answer and an authenticator or backup code for a bearer session token. " +
			"A wrong, reused or expired code or challenge answers 401; attempts are throttled. Successful responses are sent with Cache-Control: no-store.",
		Tags:    []string{authTag},
		Request: openapi.TypeOf[mfaCompletionRequest](),
		Responses: []openapi.Response{
			openapi.JSON[loginResponse](http.StatusOK),
			errorResponse(http.StatusTooManyRequests, "Too many attempts; retry after the Retry-After seconds", "Retry-After"),
		},
		Errors: []int{http.StatusUnauthorized},
	}

	passwordRecoveryRequestOperation = openapi.Operation{
		Summary: "Request a password reset link",
		Description: "Answers 202 for every well-formed request, whether or not an account matches, so it cannot be used to find accounts; " +
			"the link is sent only to the address the account holds. Requests are throttled. Successful responses are sent with Cache-Control: no-store.",
		Tags:    []string{authTag},
		Request: openapi.TypeOf[recoveryRequestBody](),
		Responses: []openapi.Response{
			openapi.JSON[recoveryAcceptedResponse](http.StatusAccepted),
			errorResponse(http.StatusTooManyRequests, "Too many requests; retry after the Retry-After seconds", "Retry-After"),
		},
	}

	passwordResetOperation = openapi.Operation{
		Summary: "Reset a password with a recovery token",
		Description: "Consumes the single-use token of a reset link and sets the new password. " +
			"400 also answers an invalid, expired or used token (recovery_invalid) and an unacceptable password (password_invalid). " +
			"Attempts are throttled. Successful responses are sent with Cache-Control: no-store.",
		Tags:    []string{authTag},
		Request: openapi.TypeOf[passwordResetBody](),
		Responses: []openapi.Response{
			openapi.Empty(http.StatusNoContent),
			errorResponse(http.StatusTooManyRequests, "Too many attempts; retry after the Retry-After seconds", "Retry-After"),
			errorResponse(http.StatusServiceUnavailable, "Password hashing is saturated; retry after the Retry-After seconds", "Retry-After"),
		},
	}

	cookieLoginOperation = openapi.Operation{
		Summary: "Sign in with a password and a browser session cookie",
		Description: "Like the bearer sign-in, but the session travels only in an HttpOnly, Secure, SameSite=Lax __Host- cookie, " +
			"and a CSRF cookie and csrf_token are issued for later unsafe requests. Needs a same-origin HTTPS Origin header (403 otherwise). " +
			"With MFA enabled it answers 202 with a challenge and sets no cookie. Successful responses are sent with Cache-Control: no-store.",
		Tags:    []string{authTag},
		Params:  openapi.TypeOf[originHeader](),
		Request: openapi.TypeOf[loginRequest](),
		Responses: []openapi.Response{
			{Status: http.StatusOK, Body: openapi.TypeOf[cookieLoginResponse](), Headers: []string{"Set-Cookie"}},
			openapi.JSON[mfaRequiredResponse](http.StatusAccepted),
			errorResponse(http.StatusTooManyRequests, "Too many attempts; retry after the Retry-After seconds", "Retry-After"),
			errorResponse(http.StatusServiceUnavailable, "Password hashing is saturated; retry after the Retry-After seconds", "Retry-After"),
		},
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden},
	}

	cookieMFACompletionOperation = openapi.Operation{
		Summary: "Complete a cookie sign-in that needs MFA",
		Description: "Exchanges the challenge of a 202 cookie sign-in answer and an authenticator or backup code for the session and CSRF cookies. " +
			"Needs a same-origin HTTPS Origin header (403 otherwise); a wrong, reused or expired code or challenge answers 401. Successful responses are sent with Cache-Control: no-store.",
		Tags:    []string{authTag},
		Params:  openapi.TypeOf[originHeader](),
		Request: openapi.TypeOf[mfaCompletionRequest](),
		Responses: []openapi.Response{
			{Status: http.StatusOK, Body: openapi.TypeOf[cookieLoginResponse](), Headers: []string{"Set-Cookie"}},
			errorResponse(http.StatusTooManyRequests, "Too many attempts; retry after the Retry-After seconds", "Retry-After"),
		},
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden},
	}

	cookieLogoutOperation = openapi.Operation{
		Summary: "Sign out of a browser session",
		Description: "Revokes the session named by the session cookie and clears the session and CSRF cookies. " +
			"The CSRF cookie and the X-CSRF-Token header must match (403 otherwise). It is idempotent: an unknown or already-revoked session still answers 204.",
		Tags:   []string{authTag},
		Params: openapi.TypeOf[csrfHeader](),
		Responses: []openapi.Response{
			{Status: http.StatusNoContent, Headers: []string{"Set-Cookie"}},
		},
		Errors: []int{http.StatusForbidden},
	}
)
