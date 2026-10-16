// Package application implements the task use cases on top of the domain and
// the ports (interfaces) that adapters implement.
package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/domain"
	"github.com/rocketinsidecorp/forge-go/pagination"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// Service implements the task use cases. It is safe for concurrent use.
type Service struct {
	tasks TaskRepository
	tx    Transactor
	now   func() time.Time
	newID func() (uuid.UUID, error)
}

// NewService wires the use cases. now and newID are injected so tests control
// time and identifiers; production passes time.Now and uuid.New.
func NewService(tasks TaskRepository, tx Transactor, now func() time.Time, newID func() (uuid.UUID, error)) (*Service, error) {
	if tasks == nil || tx == nil || now == nil || newID == nil {
		return nil, errors.New("application: task repository, transactor, clock and id source are required")
	}
	return &Service{tasks: tasks, tx: tx, now: now, newID: newID}, nil
}

// Create validates title and stores a new pending task.
func (s *Service) Create(ctx context.Context, title string) (domain.Task, error) {
	id, err := s.newID()
	if err != nil {
		return domain.Task{}, fmt.Errorf("generate task id: %w", err)
	}
	task, err := domain.NewTask(id, title, s.now())
	if err != nil {
		return domain.Task{}, err
	}
	return s.tasks.Create(ctx, task)
}

// Get returns the task with id.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (domain.Task, error) {
	return s.tasks.Get(ctx, id)
}

// List returns one page of tasks, newest first. A zero limit means
// pagination.DefaultLimit; limits above pagination.MaxLimit are capped.
func (s *Service) List(ctx context.Context, request pagination.Request) (pagination.Page[domain.Task], error) {
	limit := request.Limit
	if limit < 1 {
		limit = pagination.DefaultLimit
	}
	limit = min(limit, pagination.MaxLimit)
	// One extra row tells NewPage whether another page exists.
	rows, err := s.tasks.List(ctx, request.After, limit+1)
	if err != nil {
		return pagination.Page[domain.Task]{}, err
	}
	return pagination.NewPage(rows, limit, func(task domain.Task) uuid.UUID { return task.ID }), nil
}

// Complete marks the task done for a caller who last saw version. The read,
// the domain decision and the conditional write share one transaction; the
// write itself is guarded by the version, so a concurrent change between the
// read and the write surfaces as domain.ErrStaleVersion rather than being
// overwritten.
func (s *Service) Complete(ctx context.Context, id uuid.UUID, version int) (domain.Task, error) {
	// Reject impossible input before touching the store.
	if err := domain.ValidateVersion(version); err != nil {
		return domain.Task{}, err
	}
	var result domain.Task
	err := s.tx.InTx(ctx, func(tasks TaskRepository) error {
		task, err := tasks.Get(ctx, id)
		if err != nil {
			return err
		}
		changed, err := task.Complete(version, s.now())
		if err != nil {
			return err
		}
		if !changed {
			result = task
			return nil
		}
		result, err = tasks.Update(ctx, task)
		return err
	})
	if err != nil {
		return domain.Task{}, err
	}
	return result, nil
}
