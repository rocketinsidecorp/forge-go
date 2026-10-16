// Package sqldb holds what Forge's database adapters share: one translation of
// database errors into fault errors, transaction retries, and a database/sql
// wrapper used by the MySQL and SQLite adapters. It depends only on the
// standard library; drivers live in the adapter packages.
package sqldb

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/rocketinsidecorp/forge-go/fault"
)

// Kind is a database-independent category of failure. Each adapter maps its
// driver's error codes to a Kind; Translate maps Kinds to faults, so every
// database produces the same public error codes.
type Kind int

const (
	KindUnknown Kind = iota
	KindNotFound
	KindUniqueViolation
	KindForeignKeyViolation
	// KindExclusionViolation is PostgreSQL's EXCLUDE constraint.
	KindExclusionViolation
	// KindInvalidData is a violation a client can cause with bad values: a
	// CHECK constraint, a value too long, out of range or not a valid
	// datetime. Other data errors (a forgotten NOT NULL column, a failed
	// cast) are server bugs and stay KindUnknown.
	KindInvalidData
	// KindConflict is a serialization failure or deadlock: retrying the
	// whole transaction is expected to succeed.
	KindConflict
	KindTimeout
	KindUnavailable
)

// Classification is what an adapter knows about a driver error.
type Classification struct {
	Kind       Kind
	Constraint string
}

// Classifier inspects driver errors for one database.
type Classifier func(error) Classification

// Translate maps a database error to a *fault.Error that is safe to return to
// clients: stable code, generic message, correct HTTP status. The original
// error stays in the chain for logging. Errors that already are faults pass
// through unchanged, as does nil.
func Translate(err error, classify Classifier) error {
	if err == nil || fault.From(err) != nil {
		return err
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return newFault("not_found", "resource not found", fault.CategoryNotFound, http.StatusNotFound, false, err)
	case errors.Is(err, context.DeadlineExceeded):
		return newFault("timeout", "the operation timed out", fault.CategoryUnavailable, http.StatusServiceUnavailable, true, err)
	case errors.Is(err, context.Canceled):
		return newFault("canceled", "the operation was canceled", fault.CategoryUnavailable, http.StatusServiceUnavailable, true, err)
	}
	c := classify(err)
	switch c.Kind {
	case KindNotFound:
		return newFault("not_found", "resource not found", fault.CategoryNotFound, http.StatusNotFound, false, err)
	case KindUniqueViolation:
		return withConstraint(newFault("already_exists", "resource already exists", fault.CategoryConflict, http.StatusConflict, false, err), c)
	case KindForeignKeyViolation:
		return withConstraint(newFault("reference_violation", "a referenced resource does not exist or is still in use", fault.CategoryConflict, http.StatusConflict, false, err), c)
	case KindExclusionViolation:
		return withConstraint(newFault("conflict", "resource conflicts with an existing one", fault.CategoryConflict, http.StatusConflict, false, err), c)
	case KindInvalidData:
		return withConstraint(newFault("constraint_violation", "the request violates a data constraint", fault.CategoryInvalid, http.StatusBadRequest, false, err), c)
	case KindConflict:
		return newFault("transaction_conflict", "a concurrent update interfered; retry the request", fault.CategoryConflict, http.StatusConflict, true, err)
	case KindTimeout:
		return newFault("timeout", "the operation timed out", fault.CategoryUnavailable, http.StatusServiceUnavailable, true, err)
	case KindUnavailable:
		return newFault("database_unavailable", "the database is temporarily unavailable", fault.CategoryUnavailable, http.StatusServiceUnavailable, true, err)
	default:
		return newFault("internal_error", "internal server error", fault.CategoryInternal, http.StatusInternalServerError, false, err)
	}
}

func newFault(code, message string, category fault.Category, status int, retryable bool, cause error) *fault.Error {
	translated := fault.New(code, message, category, status).WithCause(cause)
	translated.Retryable = retryable
	return translated
}

// withConstraint records the constraint for logs and callers. Metadata is
// never serialized to clients by the web package.
func withConstraint(translated *fault.Error, c Classification) *fault.Error {
	if c.Constraint != "" {
		translated.Metadata = map[string]string{"constraint": c.Constraint}
	}
	return translated
}
