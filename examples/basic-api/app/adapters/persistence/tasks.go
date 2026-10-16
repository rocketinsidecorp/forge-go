// Package persistence implements the application's storage ports on
// PostgreSQL with explicit SQL.
package persistence

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/application"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/domain"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

const taskColumns = "id, title, done, version, created_at, updated_at"

const (
	insertTask = `INSERT INTO tasks (` + taskColumns + `)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING ` + taskColumns

	selectTask = `SELECT ` + taskColumns + ` FROM tasks WHERE id = $1`

	// UUIDv7 ids sort by creation time, so the primary key index serves
	// newest-first keyset pagination directly.
	listFirstPage = `SELECT ` + taskColumns + ` FROM tasks ORDER BY id DESC LIMIT $1`
	listAfter     = `SELECT ` + taskColumns + ` FROM tasks WHERE id < $1 ORDER BY id DESC LIMIT $2`

	// The version predicate is the optimistic lock: the row is written only
	// if nobody else wrote it since the caller read it.
	updateTask = `UPDATE tasks
SET title = $3, done = $4, updated_at = $5, version = version + 1
WHERE id = $1 AND version = $2
RETURNING ` + taskColumns

	taskExists = `SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1)`
)

// TaskRepository is the PostgreSQL application.TaskRepository. It runs on a
// pool or inside a transaction, whichever DBTX it is given.
type TaskRepository struct {
	db postgres.DBTX
}

var _ application.TaskRepository = (*TaskRepository)(nil)

func NewTaskRepository(db postgres.DBTX) *TaskRepository {
	return &TaskRepository{db: db}
}

func (r *TaskRepository) Create(ctx context.Context, task domain.Task) (domain.Task, error) {
	row := r.db.QueryRow(ctx, insertTask, task.ID, task.Title, task.Done, task.Version, task.CreatedAt, task.UpdatedAt)
	created, err := scanTask(row)
	if err != nil {
		return domain.Task{}, postgres.Translate(err)
	}
	return created, nil
}

func (r *TaskRepository) Get(ctx context.Context, id uuid.UUID) (domain.Task, error) {
	task, err := scanTask(r.db.QueryRow(ctx, selectTask, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Task{}, domain.ErrTaskNotFound
	}
	if err != nil {
		return domain.Task{}, postgres.Translate(err)
	}
	return task, nil
}

func (r *TaskRepository) List(ctx context.Context, after uuid.UUID, limit int) ([]domain.Task, error) {
	var rows pgx.Rows
	var err error
	if after == (uuid.UUID{}) {
		rows, err = r.db.Query(ctx, listFirstPage, limit)
	} else {
		rows, err = r.db.Query(ctx, listAfter, after, limit)
	}
	if err != nil {
		return nil, postgres.Translate(err)
	}
	tasks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Task, error) { return scanTask(row) })
	if err != nil {
		return nil, postgres.Translate(err)
	}
	return tasks, nil
}

func (r *TaskRepository) Update(ctx context.Context, task domain.Task) (domain.Task, error) {
	updated, err := scanTask(r.db.QueryRow(ctx, updateTask, task.ID, task.Version, task.Title, task.Done, task.UpdatedAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Task{}, r.explainMissedUpdate(ctx, task.ID)
	}
	if err != nil {
		return domain.Task{}, postgres.Translate(err)
	}
	return updated, nil
}

// explainMissedUpdate tells a deleted task from a version mismatch after the
// guarded UPDATE matched no row.
func (r *TaskRepository) explainMissedUpdate(ctx context.Context, id uuid.UUID) error {
	var exists bool
	if err := r.db.QueryRow(ctx, taskExists, id).Scan(&exists); err != nil {
		return postgres.Translate(err)
	}
	if !exists {
		return domain.ErrTaskNotFound
	}
	return domain.ErrStaleVersion
}

func scanTask(row pgx.Row) (domain.Task, error) {
	var task domain.Task
	if err := row.Scan(&task.ID, &task.Title, &task.Done, &task.Version, &task.CreatedAt, &task.UpdatedAt); err != nil {
		return domain.Task{}, err
	}
	// pgx returns timestamptz in the local zone; the API speaks UTC.
	task.CreatedAt = task.CreatedAt.UTC()
	task.UpdatedAt = task.UpdatedAt.UTC()
	return task, nil
}
