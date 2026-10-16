package persistence

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/application"
	"github.com/rocketinsidecorp/forge-go/postgres"
)

// Transactor is the PostgreSQL application.Transactor: each call runs in one
// READ COMMITTED transaction with repositories bound to it.
type Transactor struct {
	db *postgres.DB
}

var _ application.Transactor = (*Transactor)(nil)

func NewTransactor(db *postgres.DB) *Transactor {
	return &Transactor{db: db}
}

func (t *Transactor) InTx(ctx context.Context, fn func(tasks application.TaskRepository) error) error {
	err := t.db.InTx(ctx, func(tx pgx.Tx) error { return fn(NewTaskRepository(tx)) })
	// Faults from fn pass through unchanged; this translates BEGIN and
	// COMMIT failures such as a lost connection.
	return postgres.Translate(err)
}
