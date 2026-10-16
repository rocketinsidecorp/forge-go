package auth

import (
	"errors"
	"net/http"

	"github.com/rocketinsidecorp/forge-go/web"
)

// logSecurityAuditFailure records an audit-persistence failure in the
// request-scoped application log. It returns whether one was present and
// whether the underlying security-sensitive operation had already committed.
func logSecurityAuditFailure(r *http.Request, err error) (found, operationApplied bool) {
	var failure *SecurityAuditError
	if !errors.As(err, &failure) {
		return false, false
	}
	web.Logger(r.Context()).Error(
		"security audit failed",
		"method", r.Method,
		"path", r.URL.Path,
		"error", failure.Cause,
		"operation_applied", failure.OperationApplied,
	)
	return true, failure.OperationApplied
}
