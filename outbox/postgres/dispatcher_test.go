package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/events"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

func TestConcurrentClaimersGetDistinctEvents(t *testing.T) {
	db := postgrestest.NewMigrated(t, Migrations())
	insertEvents(t, db, 12)
	d, err := NewDispatcher(db.Pool(), func(context.Context, events.Event) error { return nil }, DispatcherOptions{Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	const claimers = 12
	ids := make(chan string, claimers)
	errs := make(chan error, claimers)
	var workers sync.WaitGroup
	for range claimers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			event, _, _, found, err := d.claim(context.Background())
			if err != nil {
				errs <- err
				return
			}
			if !found {
				errs <- errors.New("claim returned no event")
				return
			}
			ids <- event.ID.String()
		}()
	}
	workers.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	seen := make(map[string]bool, claimers)
	for id := range ids {
		if seen[id] {
			t.Fatalf("event %s was claimed twice", id)
		}
		seen[id] = true
	}
	if len(seen) != claimers {
		t.Fatalf("claimed %d distinct events, want %d", len(seen), claimers)
	}
}

func TestExpiredLeaseFencesPreviousWorker(t *testing.T) {
	db := postgrestest.NewMigrated(t, Migrations())
	insertEvents(t, db, 1)
	d, err := NewDispatcher(db.Pool(), func(context.Context, events.Event) error { return nil }, DispatcherOptions{Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	event, oldToken, _, found, err := d.claim(context.Background())
	if err != nil || !found {
		t.Fatalf("first claim found=%v err=%v", found, err)
	}
	if _, err := db.Exec(context.Background(), `UPDATE forge_outbox_events SET lease_until = clock_timestamp() - interval '1 second' WHERE id = $1`, event.ID); err != nil {
		t.Fatal(err)
	}
	_, newToken, _, found, err := d.claim(context.Background())
	if err != nil || !found {
		t.Fatalf("reclaim found=%v err=%v", found, err)
	}
	if oldToken == newToken {
		t.Fatal("reclaimed lease reused its fencing token")
	}
	if err := d.ack(context.Background(), event.ID, oldToken); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale ack error=%v, want ErrLeaseLost", err)
	}
	if err := d.retry(context.Background(), event.ID, oldToken, 1); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale retry error=%v, want ErrLeaseLost", err)
	}
	if err := d.ack(context.Background(), event.ID, newToken); err != nil {
		t.Fatalf("current ack: %v", err)
	}
}

func TestRetryExhaustionMovesEventToDeadLetter(t *testing.T) {
	db := postgrestest.NewMigrated(t, Migrations())
	event := insertEvents(t, db, 1)[0]
	d, err := NewDispatcher(db.Pool(), func(context.Context, events.Event) error { return nil }, DispatcherOptions{MaxAttempts: 2, RetryBase: time.Millisecond, RetryMax: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	_, token, attempts, found, err := d.claim(context.Background())
	if err != nil || !found || attempts != 1 {
		t.Fatalf("first claim attempts=%d found=%v err=%v", attempts, found, err)
	}
	if err := d.retry(context.Background(), event.ID, token, attempts); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(context.Background(), `UPDATE forge_outbox_events SET available_at = clock_timestamp() - interval '1 second' WHERE id = $1`, event.ID); err != nil {
		t.Fatal(err)
	}
	_, token, attempts, found, err = d.claim(context.Background())
	if err != nil || !found || attempts != 2 {
		t.Fatalf("second claim attempts=%d found=%v err=%v", attempts, found, err)
	}
	if err := d.retry(context.Background(), event.ID, token, attempts); err != nil {
		t.Fatal(err)
	}
	var deadLettered bool
	if err := db.QueryRow(context.Background(), `SELECT dead_lettered_at IS NOT NULL FROM forge_outbox_events WHERE id = $1`, event.ID).Scan(&deadLettered); err != nil {
		t.Fatal(err)
	}
	if !deadLettered {
		t.Fatal("event was not marked dead-lettered after the final attempt")
	}
}

func TestRunDeliversConcurrentEventsAndStopsOnCancellation(t *testing.T) {
	db := postgrestest.NewMigrated(t, Migrations())
	const total = 20
	insertEvents(t, db, total)
	delivered := make(chan struct{}, total)
	d, err := NewDispatcher(db.Pool(), func(ctx context.Context, _ events.Event) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
			delivered <- struct{}{}
			return nil
		}
	}, DispatcherOptions{Workers: 4, PollInterval: time.Millisecond, Lease: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- d.Run(ctx) }()
	for range total {
		select {
		case <-delivered:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("timed out waiting for handlers")
		}
	}
	deadline := time.After(5 * time.Second)
	for {
		var count int
		if err := db.QueryRow(context.Background(), `SELECT count(*) FROM forge_outbox_events WHERE delivered_at IS NOT NULL`).Scan(&count); err != nil {
			cancel()
			t.Fatal(err)
		}
		if count == total {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatalf("timed out waiting for acknowledgements: got %d of %d", count, total)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error=%v, want context.Canceled", err)
	}
	var count int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM forge_outbox_events WHERE delivered_at IS NOT NULL`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != total {
		t.Fatalf("delivered rows=%d, want %d", count, total)
	}
}

// The handler outlives the lease it was claimed with. It does not sleep for
// a guessed time: it returns once the database shows both that the initial
// lease has run out and that the lease was extended past it, so the event
// can only be delivered if renewals really happened. The lease is five
// seconds and renewals come every third of it; a scheduling stall would
// have to last well over three seconds to lose the lease.
func TestRunRenewsLeaseDuringLongHandler(t *testing.T) {
	db := postgrestest.NewMigrated(t, Migrations())
	insertEvents(t, db, 1)
	var calls atomic.Int32
	var renewed atomic.Bool
	d, err := NewDispatcher(db.Pool(), func(ctx context.Context, event events.Event) error {
		calls.Add(1)
		var initial time.Time
		if err := db.QueryRow(ctx, `SELECT lease_until FROM forge_outbox_events WHERE id = $1`, event.ID).Scan(&initial); err != nil {
			return err
		}
		for {
			var expired, extended bool
			if err := db.QueryRow(ctx, `SELECT clock_timestamp() > $1, lease_until > $1 FROM forge_outbox_events WHERE id = $2`, initial, event.ID).Scan(&expired, &extended); err != nil {
				return err
			}
			if expired && extended {
				renewed.Store(true)
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
	}, DispatcherOptions{Workers: 2, PollInterval: time.Millisecond, Lease: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- d.Run(ctx) }()
	deadline := time.After(60 * time.Second)
	for {
		var count int
		if err := db.QueryRow(context.Background(), `SELECT count(*) FROM forge_outbox_events WHERE delivered_at IS NOT NULL`).Scan(&count); err != nil {
			cancel()
			t.Fatal(err)
		}
		if count == 1 {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("timed out waiting for event acknowledgement")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error=%v, want context.Canceled", err)
	}
	if !renewed.Load() {
		t.Error("the event was delivered without the lease being extended past its first expiry")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls=%d, want one despite handler exceeding the initial lease", got)
	}
}

func TestExpiredClaimsCannotExceedMaximumAttempts(t *testing.T) {
	db := postgrestest.NewMigrated(t, Migrations())
	event := insertEvents(t, db, 1)[0]
	d, err := NewDispatcher(db.Pool(), func(context.Context, events.Event) error { return nil }, DispatcherOptions{MaxAttempts: 2, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for wantAttempt := 1; wantAttempt <= 2; wantAttempt++ {
		claimed, _, attempts, found, err := d.claim(context.Background())
		if err != nil || !found || attempts != wantAttempt || claimed.ID != event.ID {
			t.Fatalf("claim #%d: id=%s attempts=%d found=%v err=%v", wantAttempt, claimed.ID, attempts, found, err)
		}
		if _, err := db.Exec(context.Background(), `UPDATE forge_outbox_events SET lease_until = clock_timestamp() - interval '1 second' WHERE id = $1`, event.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, found, err := d.claim(context.Background()); err != nil || found {
		t.Fatalf("claim past maximum: found=%v err=%v, want no event", found, err)
	}
	var attempts int
	var deadLettered bool
	if err := db.QueryRow(context.Background(), `SELECT attempts, dead_lettered_at IS NOT NULL FROM forge_outbox_events WHERE id = $1`, event.ID).Scan(&attempts, &deadLettered); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || !deadLettered {
		t.Fatalf("after expired claims attempts=%d dead_lettered=%v, want 2 and true", attempts, deadLettered)
	}
}

func insertEvents(t *testing.T, db interface {
	InTx(context.Context, func(tx pgx.Tx) error) error
}, count int) []events.Event {
	t.Helper()
	created := make([]events.Event, count)
	ctx := context.Background()
	for i := range count {
		event, err := events.New("task.created", 1, json.RawMessage(fmt.Sprintf(`{"task_id":"task-%d"}`, i)))
		if err != nil {
			t.Fatal(err)
		}
		created[i] = event
		if err := db.InTx(ctx, func(tx pgx.Tx) error { return Insert(ctx, tx, event) }); err != nil {
			t.Fatal(err)
		}
	}
	return created
}
