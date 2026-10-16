package domain

import "github.com/rocketinsidecorp/forge-go/fault"

// Domain errors carry stable codes that are part of the API contract. They
// are sentinels: compare them with errors.Is and never modify them. The HTTP
// status follows from the category, so the domain stays transport-agnostic.
var (
	ErrInvalidID      = fault.New("invalid_id", "task id must be a UUIDv7", fault.CategoryInvalid, 0)
	ErrInvalidTitle   = fault.New("invalid_title", "title must be 1 to 200 characters of single-line text", fault.CategoryInvalid, 0)
	ErrInvalidVersion = fault.New("invalid_version", "version must be a positive integer", fault.CategoryInvalid, 0)
	ErrTaskNotFound   = fault.New("task_not_found", "task not found", fault.CategoryNotFound, 0)
	ErrStaleVersion   = fault.New("stale_version", "the task was changed by another request; reload it and retry", fault.CategoryConflict, 0)
)
