package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

const MaxDeadLetterPageSize = 100

// QueueStats is an exact database snapshot of the dispatch queue at query
// time. OldestPendingAt is nil when no undelivered, non-dead-lettered event
// exists. Counts are global because the outbox is a trusted global worker.
type QueueStats struct {
	Ready           int64
	Scheduled       int64
	Leased          int64
	DeadLettered    int64
	OldestPendingAt *time.Time
}

// DeadLetter is the operational metadata for a terminal event. Payloads and
// handler error text are intentionally omitted to reduce accidental exposure.
type DeadLetter struct {
	ID             uuid.UUID
	Type           string
	Version        int
	OccurredAt     time.Time
	Attempts       int
	DeadLetteredAt time.Time
}

// DeadLetterCursor continues a stable keyset page ordered by terminalization
// time and event ID.
type DeadLetterCursor struct {
	DeadLetteredAt time.Time
	ID             uuid.UUID
}

// DeadLetterPage contains one bounded page and a cursor only when more rows
// are available.
type DeadLetterPage struct {
	Items []DeadLetter
	Next  *DeadLetterCursor
}

// Stats returns queue counts using the PostgreSQL database clock.
func (d *Dispatcher) Stats(ctx context.Context) (QueueStats, error) {
	if ctx == nil {
		return QueueStats{}, errors.New("outbox stats context is required")
	}
	var stats QueueStats
	var oldest pgtype.Timestamptz
	err := d.pool.QueryRow(ctx, `
		WITH probe_clock AS (SELECT clock_timestamp() AS value)
		SELECT
			(SELECT count(*) FROM forge_outbox_events, probe_clock
			 WHERE delivered_at IS NULL AND dead_lettered_at IS NULL
			   AND available_at <= probe_clock.value
			   AND (lease_until IS NULL OR lease_until <= probe_clock.value)),
			(SELECT count(*) FROM forge_outbox_events, probe_clock
			 WHERE delivered_at IS NULL AND dead_lettered_at IS NULL
			   AND available_at > probe_clock.value
			   AND (lease_until IS NULL OR lease_until <= probe_clock.value)),
			(SELECT count(*) FROM forge_outbox_events, probe_clock
			 WHERE delivered_at IS NULL AND dead_lettered_at IS NULL
			   AND lease_until > probe_clock.value),
			(SELECT count(*) FROM forge_outbox_events WHERE dead_lettered_at IS NOT NULL),
			(SELECT min(enqueued_at) FROM forge_outbox_events
			 WHERE delivered_at IS NULL AND dead_lettered_at IS NULL)
	`).Scan(&stats.Ready, &stats.Scheduled, &stats.Leased, &stats.DeadLettered, &oldest)
	if err != nil {
		return QueueStats{}, fmt.Errorf("read outbox queue stats: %w", err)
	}
	if oldest.Valid {
		value := oldest.Time
		stats.OldestPendingAt = &value
	}
	return stats, nil
}

// ListDeadLetters returns a bounded page of terminal events ordered by
// dead-letter time then event ID. A zero limit uses 25; limits above 100 are
// rejected. Payloads and arbitrary handler errors are not returned.
func (d *Dispatcher) ListDeadLetters(ctx context.Context, after *DeadLetterCursor, limit int) (DeadLetterPage, error) {
	if ctx == nil {
		return DeadLetterPage{}, errors.New("outbox dead-letter context is required")
	}
	if limit == 0 {
		limit = 25
	}
	if limit < 1 || limit > MaxDeadLetterPageSize {
		return DeadLetterPage{}, fmt.Errorf("outbox dead-letter page size must be between 1 and %d", MaxDeadLetterPageSize)
	}
	if after != nil && (after.DeadLetteredAt.IsZero() || after.ID.Version() != 7 || after.ID.Variant() != 2) {
		return DeadLetterPage{}, errors.New("outbox dead-letter cursor is invalid")
	}

	query := `
		SELECT id, event_type, schema_version, occurred_at, attempts, dead_lettered_at
		FROM forge_outbox_events
		WHERE dead_lettered_at IS NOT NULL
		ORDER BY dead_lettered_at, id
		LIMIT $1
	`
	args := []any{limit + 1}
	if after != nil {
		query = `
			SELECT id, event_type, schema_version, occurred_at, attempts, dead_lettered_at
			FROM forge_outbox_events
			WHERE dead_lettered_at IS NOT NULL
			  AND (dead_lettered_at, id) > ($1, $2)
			ORDER BY dead_lettered_at, id
			LIMIT $3
		`
		args = []any{after.DeadLetteredAt, after.ID, limit + 1}
	}
	rows, err := d.pool.Query(ctx, query, args...)
	if err != nil {
		return DeadLetterPage{}, fmt.Errorf("list outbox dead letters: %w", err)
	}
	defer rows.Close()

	items := make([]DeadLetter, 0, limit+1)
	for rows.Next() {
		var item DeadLetter
		if err := rows.Scan(&item.ID, &item.Type, &item.Version, &item.OccurredAt, &item.Attempts, &item.DeadLetteredAt); err != nil {
			return DeadLetterPage{}, fmt.Errorf("scan outbox dead letter: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return DeadLetterPage{}, fmt.Errorf("iterate outbox dead letters: %w", err)
	}
	page := DeadLetterPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[len(page.Items)-1]
		page.Next = &DeadLetterCursor{DeadLetteredAt: last.DeadLetteredAt, ID: last.ID}
	}
	return page, nil
}

// ReplayDeadLetter resets a terminal event back to the dispatch queue.
// The event must be in the dead-lettered state. Its attempt count is reset
// and it becomes immediately available for claiming. Payload and metadata
// are unchanged; operator intent is the only authority for replay.
func (d *Dispatcher) ReplayDeadLetter(ctx context.Context, id uuid.UUID) error {
	if ctx == nil {
		return errors.New("outbox replay context is required")
	}
	tag, err := d.pool.Exec(ctx, `
		UPDATE forge_outbox_events
		SET attempts = 0,
		    available_at = clock_timestamp(),
		    dead_lettered_at = NULL,
		    lease_token = NULL,
		    lease_until = NULL
		WHERE id = $1 AND dead_lettered_at IS NOT NULL
	`, id)
	if err != nil {
		return fmt.Errorf("replay outbox dead letter: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("outbox dead letter not found or already replayed")
	}
	return nil
}
