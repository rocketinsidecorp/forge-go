package sqlite_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/queue"
	queuesqlite "github.com/rocketinsidecorp/forge-go/queue/sqlite"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// SQLite keeps timestamps as text, so a row written by hand or by another
// tool can hold one the store cannot read. Every read names the problem and
// never returns a half-read job or a zero time.
func TestStoreReportsUnreadableStoredTimes(t *testing.T) {
	t.Parallel()
	for _, column := range []string{"run_at", "enqueued_at", "lease_until", "finished_at"} {
		t.Run(column, func(t *testing.T) {
			t.Parallel()
			db := newDB(t)
			store, err := queuesqlite.NewStore(db)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			insert := func(kind string) uuid.UUID {
				id := uuid.MustNew()
				if _, err := store.Insert(ctx, queue.NewJob{ID: id, Kind: kind, Queue: queue.DefaultQueue, Payload: json.RawMessage(`{}`), MaxAttempts: 1}); err != nil {
					t.Fatal(err)
				}
				return id
			}
			// A running job has a lease, a dead one a finish time.
			var id uuid.UUID
			switch column {
			case "lease_until":
				id = insert("k.running")
				if _, ok, err := store.Claim(ctx, queue.ClaimRequest{Queue: queue.DefaultQueue, Kinds: []string{"k.running"}, WorkerID: "w", Lease: time.Hour}); err != nil || !ok {
					t.Fatalf("claim: %v %v", ok, err)
				}
			case "finished_at":
				id = insert("k.dead")
				claimed, ok, err := store.Claim(ctx, queue.ClaimRequest{Queue: queue.DefaultQueue, Kinds: []string{"k.dead"}, WorkerID: "w", Lease: time.Hour})
				if err != nil || !ok {
					t.Fatalf("claim: %v %v", ok, err)
				}
				if err := store.Fail(ctx, claimed.Lease, queue.Failure{Message: "boom", Dead: true}); err != nil {
					t.Fatal(err)
				}
			default:
				id = insert("k.pending")
			}
			if _, err := db.ExecContext(ctx, "UPDATE forge_jobs SET "+column+" = '0-not-a-time' WHERE id = ?", id); err != nil {
				t.Fatal(err)
			}

			if _, err := store.Get(ctx, id); err == nil || !strings.Contains(err.Error(), "parse stored time") {
				t.Errorf("Get = %v, want the unreadable time named", err)
			}
			if column == "finished_at" {
				if _, err := store.ListDead(ctx, queue.DeadQuery{}); err == nil || !strings.Contains(err.Error(), "scan dead job") {
					t.Errorf("ListDead = %v, want the unreadable row named", err)
				}
			}
			if column == "run_at" {
				// The oldest ready job's due time is part of the statistics.
				if _, err := store.Stats(ctx); err == nil || !strings.Contains(err.Error(), "parse stored time") {
					t.Errorf("Stats = %v, want the unreadable time named", err)
				}
			}
		})
	}
}
