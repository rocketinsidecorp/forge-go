// Package fault defines stable application errors without exposing internal causes.
package fault

import (
	"errors"
	"fmt"
	"net/http"
)

// Category is the class of an Error; it selects the default HTTP status.
type Category string

// The categories of Error and their default HTTP statuses: 400, 401, 403, 404, 409, 503, 429 and
// 500.
const (
	CategoryInvalid      Category = "invalid"
	CategoryUnauthorized Category = "unauthorized"
	CategoryForbidden    Category = "forbidden"
	CategoryNotFound     Category = "not_found"
	CategoryConflict     Category = "conflict"
	CategoryUnavailable  Category = "unavailable"
	CategoryRateLimited  Category = "rate_limited"
	CategoryInternal     Category = "internal"
)

// Error is a stable application error: a machine-readable Code and a safe Message for clients, with
// an optional Cause and Metadata that stay internal.
type Error struct {
	Code       string
	Message    string
	Category   Category
	Cause      error
	Metadata   map[string]string
	Retryable  bool
	HTTPStatus int
}

// New returns an Error. A status of 0 selects the default for the category.
func New(code, message string, category Category, status int) *Error {
	return &Error{Code: code, Message: message, Category: category, HTTPStatus: status}
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

// Is matches another *Error with the same Code, so errors.Is recognizes a
// sentinel even after WithCause or Metadata produced a copy of it.
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && e != nil && other != nil && e.Code == other.Code
}

// WithCause returns a copy of e with cause set; e itself is unchanged.
func (e *Error) WithCause(cause error) *Error {
	copy := *e
	copy.Cause = cause
	return &copy
}

// Status returns the HTTP status for the error: the explicit HTTPStatus when
// set, otherwise the default for its category.
func (e *Error) Status() int {
	if e.HTTPStatus != 0 {
		return e.HTTPStatus
	}
	switch e.Category {
	case CategoryInvalid:
		return http.StatusBadRequest
	case CategoryUnauthorized:
		return http.StatusUnauthorized
	case CategoryForbidden:
		return http.StatusForbidden
	case CategoryNotFound:
		return http.StatusNotFound
	case CategoryConflict:
		return http.StatusConflict
	case CategoryUnavailable:
		return http.StatusServiceUnavailable
	case CategoryRateLimited:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

// From returns the first *Error in err's chain, or nil.
func From(err error) *Error {
	var target *Error
	if errors.As(err, &target) {
		return target
	}
	return nil
}
