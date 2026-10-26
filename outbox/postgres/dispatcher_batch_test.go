package postgres

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/events"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

type eventRow struct {
	attempts                int
	leased, delivered, dead bool
	availableLater          bool
}

func readEvent(t *testing.T, db *postgres.DB, id uuid.UUID) eventRow {
	t.Helper()
	var row eventRow
	if err := db.QueryRow(context.Background(), `
		SELECT attempts, lease_token IS NOT NULL, delivered_at IS NOT NULL, dead_lettered_at IS NOT NULL,
		       available_at > clock_timestamp()
		FROM forge_outbox_events WHERE id = $1`, id).
		Scan(&row.attempts, &row.leased, &row.delivered, &row.dead, &row.availableLater); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestDefaultBatchSize(t *testing.T) {
	t.Parallel()
	d, err := NewDispatcher(postgrestest.NewMigrated(t, Migrations()).Pool(), noopHandler, DispatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.opts.BatchSize != DefaultBatchSize {
		t.Fatalf("BatchSize = %d, want %d", d.opts.BatchSize, DefaultBatchSize)
	}
}

// One failing publish in a batch schedules a retry of that event only; the
// others are delivered once each, in claim order.
func TestBatchWithOneFailingPublish(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, Migrations())
	created := insertEvents(t, db, 5)
	failing := created[2].ID
	var order []uuid.UUID
	d, err := NewDispatcher(db.Pool(), func(_ context.Context, event events.Event) error {
		order = append(order, event.ID)
		if event.ID == failing {
			return errors.New("downstream returned 503")
		}
		return nil
	}, DispatcherOptions{BatchSize: 10, RetryBase: time.Hour, RetryMax: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := d.dispatchBatch(context.Background()); err != nil || !worked {
		t.Fatalf("dispatchBatch = %v, %v", worked, err)
	}
	if len(order) != len(created) {
		t.Fatalf("handled %d events, want %d", len(order), len(created))
	}
	for i, event := range created {
		if order[i] != event.ID {
			t.Fatalf("event %d delivered out of claim order", i)
		}
		row := readEvent(t, db, event.ID)
		switch {
		case row.attempts != 1 || row.leased || row.dead:
			t.Fatalf("event %d: %+v", i, row)
		case event.ID == failing && (row.delivered || !row.availableLater):
			t.Fatalf("the failed event must wait for its retry: %+v", row)
		case event.ID != failing && !row.delivered:
			t.Fatalf("event %d was not delivered: %+v", i, row)
		}
	}
	if worked, err := d.dispatchBatch(context.Background()); err != nil || worked {
		t.Fatalf("second dispatchBatch = %v, %v; the retry is not due", worked, err)
	}
}

// Several dispatchers with several workers each, claiming in batches,
// deliver every event exactly once (leases never expire here).
func TestConcurrentBatchDispatchersDeliverEachEventOnce(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, Migrations())
	const total = 300
	created := insertEvents(t, db, total)
	var mu sync.Mutex
	seen := make(map[uuid.UUID]int, total)
	var delivered atomic.Int32
	handler := func(_ context.Context, event events.Event) error {
		mu.Lock()
		seen[event.ID]++
		mu.Unlock()
		delivered.Add(1)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, 4)
	for range 4 {
		d, err := NewDispatcher(db.Pool(), handler, DispatcherOptions{Workers: 2, BatchSize: 7, PollInterval: 5 * time.Millisecond, Lease: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		go func() { results <- d.Run(ctx) }()
	}
	deadline := time.Now().Add(30 * time.Second)
	for delivered.Load() < total {
		if time.Now().After(deadline) {
			t.Fatalf("delivered %d of %d events", delivered.Load(), total)
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // a duplicate would show up now
	cancel()
	for range 4 {
		if err := <-results; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != total {
		t.Fatalf("%d distinct events delivered, want %d", len(seen), total)
	}
	for _, event := range created {
		if seen[event.ID] != 1 {
			t.Fatalf("event %s delivered %d times", event.ID, seen[event.ID])
		}
		if row := readEvent(t, db, event.ID); !row.delivered || row.attempts != 1 {
			t.Fatalf("event %s: %+v", event.ID, row)
		}
	}
}

// Shutdown in the middle of a batch: the running event is scheduled for a
// retry and the events not yet started go back without using an attempt.
func TestShutdownReleasesTheUnstartedRestOfABatch(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, Migrations())
	created := insertEvents(t, db, 4)
	started := make(chan struct{})
	var calls atomic.Int32
	d, err := NewDispatcher(db.Pool(), func(ctx context.Context, _ events.Event) error {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}, DispatcherOptions{BatchSize: 4, PollInterval: time.Millisecond, Lease: time.Minute, RetryBase: time.Hour, RetryMax: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- d.Run(ctx) }()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("%d handlers ran, want 1", calls.Load())
	}
	if row := readEvent(t, db, created[0].ID); row.attempts != 1 || row.leased || row.delivered || !row.availableLater {
		t.Fatalf("interrupted event: %+v; want a retry after its backoff", row)
	}
	for _, event := range created[1:] {
		if row := readEvent(t, db, event.ID); row.attempts != 0 || row.leased || row.delivered || row.dead || row.availableLater {
			t.Fatalf("unstarted event %s: %+v; want it released with its attempt", event.ID, row)
		}
	}
}

// Events waiting behind a handler slower than the lease keep their leases:
// another dispatcher cannot take them, and each is delivered once.
func TestWaitingEventsKeepTheirLeasesDuringASlowHandler(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, Migrations())
	created := insertEvents(t, db, 3)
	var calls atomic.Int32
	slow, err := NewDispatcher(db.Pool(), func(context.Context, events.Event) error {
		if calls.Add(1) == 1 {
			time.Sleep(time.Second)
		}
		return nil
	}, DispatcherOptions{BatchSize: 3, Lease: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := slow.dispatchBatch(context.Background())
		done <- err
	}()
	other, err := NewDispatcher(db.Pool(), noopHandler, DispatcherOptions{BatchSize: 3, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		time.Sleep(100 * time.Millisecond)
		batch, _, err := other.claimBatch(context.Background(), 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) != 0 {
			t.Fatalf("another dispatcher took %d event(s) still leased to the slow batch", len(batch))
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("%d deliveries, want 3", calls.Load())
	}
	for _, event := range created {
		if row := readEvent(t, db, event.ID); !row.delivered || row.attempts != 1 {
			t.Fatalf("event %s: %+v", event.ID, row)
		}
	}
}

// An event of the batch that another dispatcher reclaimed before its turn is
// skipped, not delivered twice, and the batch goes on.
func TestAnEventReclaimedBeforeItsTurnIsSkipped(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, Migrations())
	created := insertEvents(t, db, 3)
	reclaimed := created[1].ID
	var handled sync.Map
	var first sync.Once
	d, err := NewDispatcher(db.Pool(), func(ctx context.Context, event events.Event) error {
		handled.Store(event.ID, true)
		first.Do(func() {
			// Another dispatcher takes the second event (its token changes);
			// wait for a renewal to notice.
			if _, err := db.Exec(ctx, `UPDATE forge_outbox_events SET lease_token = $2 WHERE id = $1`, reclaimed, uuid.MustNew()); err != nil {
				t.Error(err)
			}
			time.Sleep(500 * time.Millisecond)
		})
		return nil
	}, DispatcherOptions{BatchSize: 3, Lease: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := d.dispatchBatch(context.Background()); err != nil || !worked {
		t.Fatalf("dispatchBatch = %v, %v", worked, err)
	}
	if _, ok := handled.Load(reclaimed); ok {
		t.Fatal("the reclaimed event was delivered by the dispatcher that lost it")
	}
	for _, event := range []events.Event{created[0], created[2]} {
		if row := readEvent(t, db, event.ID); !row.delivered {
			t.Fatalf("event %s was not delivered: %+v", event.ID, row)
		}
	}
	if row := readEvent(t, db, reclaimed); row.delivered || !row.leased {
		t.Fatalf("reclaimed event: %+v; it belongs to the other dispatcher", row)
	}
}
