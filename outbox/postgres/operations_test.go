package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/events"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestStatsReportsQueueStates(t *testing.T) {
	db := postgrestest.NewMigrated(t, Migrations())
	created := insertEvents(t, db, 4)
	d, err := NewDispatcher(db.Pool(), func(context.Context, events.Event) error { return nil }, DispatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, found, err := d.claim(context.Background()); err != nil || !found {
		t.Fatalf("claim event to lease: found=%v err=%v", found, err)
	}
	if _, err := db.Exec(context.Background(), `UPDATE forge_outbox_events SET available_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, created[2].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(context.Background(), `UPDATE forge_outbox_events SET dead_lettered_at = clock_timestamp() WHERE id = $1`, created[3].ID); err != nil {
		t.Fatal(err)
	}

	stats, err := d.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Ready != 1 || stats.Scheduled != 1 || stats.Leased != 1 || stats.DeadLettered != 1 {
		t.Fatalf("queue counts = %+v, want ready=1 scheduled=1 leased=1 dead_lettered=1", stats)
	}
	if stats.OldestPendingAt == nil {
		t.Fatal("stats omitted oldest pending event timestamp")
	}
}

func TestListDeadLettersUsesStableBoundedCursor(t *testing.T) {
	db := postgrestest.NewMigrated(t, Migrations())
	created := insertEvents(t, db, 3)
	base := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	for index, event := range created {
		if _, err := db.Exec(context.Background(), `UPDATE forge_outbox_events SET dead_lettered_at = $2 WHERE id = $1`, event.ID, base.Add(time.Duration(index)*time.Microsecond)); err != nil {
			t.Fatal(err)
		}
	}
	d, err := NewDispatcher(db.Pool(), func(context.Context, events.Event) error { return nil }, DispatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}

	first, err := d.ListDeadLetters(context.Background(), nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.Next == nil {
		t.Fatalf("first page = %+v, want two items and next cursor", first)
	}
	second, err := d.ListDeadLetters(context.Background(), first.Next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Next != nil {
		t.Fatalf("second page = %+v, want one item and no next cursor", second)
	}
	seen := map[uuid.UUID]bool{}
	for _, item := range append(first.Items, second.Items...) {
		if seen[item.ID] {
			t.Fatalf("duplicate dead-letter ID %s across pages", item.ID)
		}
		seen[item.ID] = true
		if item.ID != created[0].ID && item.ID != created[1].ID && item.ID != created[2].ID {
			t.Fatalf("unexpected dead-letter ID %s", item.ID)
		}
	}
	if len(seen) != len(created) {
		t.Fatalf("listed %d unique dead letters, want %d", len(seen), len(created))
	}
	if first.Items[0].ID != created[0].ID || first.Items[0].Attempts != 0 {
		t.Fatalf("dead-letter metadata = %+v, payload should be excluded and insertion order preserved", first.Items[0])
	}
}

func TestListDeadLettersRejectsUnboundedOrInvalidRequests(t *testing.T) {
	db := postgrestest.NewMigrated(t, Migrations())
	d, err := NewDispatcher(db.Pool(), func(context.Context, events.Event) error { return nil }, DispatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{-1, MaxDeadLetterPageSize + 1} {
		if _, err := d.ListDeadLetters(context.Background(), nil, limit); err == nil {
			t.Errorf("ListDeadLetters(limit=%d) succeeded, want validation error", limit)
		}
	}
	invalid := &DeadLetterCursor{DeadLetteredAt: time.Now(), ID: uuid.UUID{}}
	if _, err := d.ListDeadLetters(context.Background(), invalid, 10); err == nil {
		t.Fatal("ListDeadLetters accepted an invalid cursor")
	}
}

func TestReplayDeadLetterResetsEventToQueue(t *testing.T) {
	db := postgrestest.NewMigrated(t, Migrations())
	created := insertEvents(t, db, 1)
	if _, err := db.Exec(context.Background(), `UPDATE forge_outbox_events SET dead_lettered_at = clock_timestamp() WHERE id = $1`, created[0].ID); err != nil {
		t.Fatal(err)
	}
	d, err := NewDispatcher(db.Pool(), func(context.Context, events.Event) error { return nil }, DispatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ReplayDeadLetter(context.Background(), created[0].ID); err != nil {
		t.Fatalf("ReplayDeadLetter failed: %v", err)
	}
	stats, err := d.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.DeadLettered != 0 || stats.Ready != 1 {
		t.Fatalf("stats after replay = %+v, want dead_lettered=0 ready=1", stats)
	}
	// Replaying an already replayed event should fail
	if err := d.ReplayDeadLetter(context.Background(), created[0].ID); err == nil {
		t.Fatal("ReplayDeadLetter on non-dead-lettered event succeeded, want error")
	}
}
