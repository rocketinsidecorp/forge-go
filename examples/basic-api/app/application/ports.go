package application

import (
	"context"

	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/domain"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// TaskRepository persists tasks. Implementations report domain.ErrTaskNotFound
// for missing tasks and domain.ErrStaleVersion for lost optimistic-lock races.
type TaskRepository interface {
	// Create stores a new task and returns it as stored.
	Create(ctx context.Context, task domain.Task) (domain.Task, error)
	Get(ctx context.Context, id uuid.UUID) (domain.Task, error)
	// List returns up to limit tasks newest first, starting after the task
	// with id after, or from the newest task when after is the zero UUID.
	List(ctx context.Context, after uuid.UUID, limit int) ([]domain.Task, error)
	// Update writes task only if the stored version still equals
	// task.Version, advances the version and returns the task as stored.
	Update(ctx context.Context, task domain.Task) (domain.Task, error)
}

// Transactor runs fn atomically: every write through the repository it
// receives commits when fn returns nil and rolls back otherwise.
type Transactor interface {
	InTx(ctx context.Context, fn func(tasks TaskRepository) error) error
}
