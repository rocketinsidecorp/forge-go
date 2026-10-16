package application_test

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/application"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/domain"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// memoryRepository is an in-memory TaskRepository and Transactor with the
// same error contract as the PostgreSQL adapter. Its InTx applies writes
// immediately and cannot roll back: every use case here writes at most once
// per transaction, and rollback is covered by the persistence tests against
// PostgreSQL.
type memoryRepository struct {
	mu    sync.Mutex
	tasks map[uuid.UUID]domain.Task
	// afterGet, when set, runs after every Get, simulating a concurrent
	// writer acting between a use case's read and its write.
	afterGet func(id uuid.UUID)
}

var (
	_ application.TaskRepository = (*memoryRepository)(nil)
	_ application.Transactor     = (*memoryRepository)(nil)
)

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{tasks: make(map[uuid.UUID]domain.Task)}
}

func (r *memoryRepository) Create(_ context.Context, task domain.Task) (domain.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks[task.ID] = task
	return task, nil
}

func (r *memoryRepository) Get(_ context.Context, id uuid.UUID) (domain.Task, error) {
	r.mu.Lock()
	task, ok := r.tasks[id]
	r.mu.Unlock()
	if !ok {
		return domain.Task{}, domain.ErrTaskNotFound
	}
	if r.afterGet != nil {
		r.afterGet(id)
	}
	return task, nil
}

func (r *memoryRepository) List(_ context.Context, after uuid.UUID, limit int) ([]domain.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	all := slices.SortedFunc(maps.Values(r.tasks), func(a, b domain.Task) int { return cmp.Compare(0, a.ID.Compare(b.ID)) })
	var page []domain.Task
	for _, task := range all {
		if after != (uuid.UUID{}) && task.ID.Compare(after) >= 0 {
			continue
		}
		if len(page) == limit {
			break
		}
		page = append(page, task)
	}
	return page, nil
}

func (r *memoryRepository) Update(_ context.Context, task domain.Task) (domain.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, ok := r.tasks[task.ID]
	switch {
	case !ok:
		return domain.Task{}, domain.ErrTaskNotFound
	case stored.Version != task.Version:
		return domain.Task{}, domain.ErrStaleVersion
	}
	task.Version++
	r.tasks[task.ID] = task
	return task, nil
}

func (r *memoryRepository) InTx(_ context.Context, fn func(application.TaskRepository) error) error {
	return fn(r)
}

// bump simulates another request completing a write to id.
func (r *memoryRepository) bump(id uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	task := r.tasks[id]
	task.Version++
	r.tasks[id] = task
}
