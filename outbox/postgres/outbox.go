// Package postgres provides PostgreSQL persistence for the transactional outbox.
package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/events"
	forgepostgres "github.com/rocketinsidecorp/forge-go/postgres"
)

// Insert appends event using the active transaction from postgres.InTx, so the
// event and the domain change that produced it commit or roll back together.
func Insert(ctx context.Context, tx pgx.Tx, event events.Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	if tx == nil {
		return errors.New("outbox transaction is required")
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO forge_outbox_events (id, event_type, schema_version, occurred_at, payload)
		VALUES ($1, $2, $3, $4, $5::jsonb)
	`, event.ID, event.Type, event.Version, event.OccurredAt.UTC(), []byte(event.Payload))
	return forgepostgres.Translate(err)
}
