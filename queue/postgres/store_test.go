package postgres_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/queue"
	queuepostgres "github.com/rocketinsidecorp/forge-go/queue/postgres"
	"github.com/rocketinsidecorp/forge-go/queue/storetest"
)

func newDB(t testing.TB) *postgres.DB {
	t.Helper()
	return postgrestest.NewMigrated(t, queuepostgres.Migrations())
}

func newStore(t testing.TB) queue.Store {
	t.Helper()
	store, err := queuepostgres.NewStore(newDB(t))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestStoreConformance(t *testing.T) {
	storetest.Run(t, newStore)
}

type order struct {
	ID string `json:"id"`
}

func newTxFixture(t *testing.T) (*postgres.DB, *queuepostgres.Store, *queue.Registry, *atomic.Int32) {
	t.Helper()
	db := newDB(t)
	if _, err := db.Exec(context.Background(), "CREATE TABLE orders (id text PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	store, err := queuepostgres.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	var ran atomic.Int32
	registry := queue.NewRegistry()
	if err := queue.Register(registry, "order.confirm", func(context.Context, order) error { ran.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	return db, store, registry, &ran
}

func count(t *testing.T, db *postgres.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEnqueueRollsBackWithTheTransaction(t *testing.T) {
	db, store, registry, _ := newTxFixture(t)
	ctx := context.Background()
	wantErr := errors.New("business rule failed after enqueue")
	err := db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO orders VALUES ('o-1')"); err != nil {
			return err
		}
		if _, err := queue.Enqueue(ctx, store.In(tx), registry, "order.confirm", order{ID: "o-1"}); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("InTx = %v, want %v", err, wantErr)
	}
	if jobs, orders := count(t, db, "forge_jobs"), count(t, db, "orders"); jobs != 0 || orders != 0 {
		t.Fatalf("after rollback: %d jobs, %d orders; want none of either", jobs, orders)
	}
}

func TestEnqueueCommitsWithTheTransactionAndStaysInvisibleUntilThen(t *testing.T) {
	db, store, registry, ran := newTxFixture(t)
	ctx := context.Background()
	worker, err := queue.NewWorker(store, registry, queue.WorkerOptions{PollInterval: 10 * time.Millisecond, MaxPollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = worker.Shutdown(stop)
	})

	var jobID string
	err = db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO orders VALUES ('o-2')"); err != nil {
			return err
		}
		result, err := queue.Enqueue(ctx, store.In(tx), registry, "order.confirm", order{ID: "o-2"})
		if err != nil {
			return err
		}
		jobID = result.ID.String()
		// The job exists for this transaction only: a running worker must not
		// see, claim or run it before commit.
		time.Sleep(300 * time.Millisecond)
		if n := ran.Load(); n != 0 {
			t.Errorf("handler ran %d time(s) before the transaction committed", n)
		}
		if n := count(t, db, "forge_jobs"); n != 0 {
			t.Errorf("other connections see %d job(s) before commit", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for ran.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not run after commit", jobID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if jobs, orders := count(t, db, "forge_jobs"), count(t, db, "orders"); jobs != 1 || orders != 1 {
		t.Fatalf("after commit: %d jobs, %d orders; want one of each", jobs, orders)
	}
}

func TestDuplicateKeyInsideATransactionDoesNotAbortIt(t *testing.T) {
	db, store, registry, _ := newTxFixture(t)
	ctx := context.Background()
	err := db.InTx(ctx, func(tx pgx.Tx) error {
		first, err := queue.Enqueue(ctx, store.In(tx), registry, "order.confirm", order{ID: "a"}, queue.WithKey("k"))
		if err != nil || first.Duplicate {
			t.Fatalf("first = %+v, %v", first, err)
		}
		second, err := queue.Enqueue(ctx, store.In(tx), registry, "order.confirm", order{ID: "b"}, queue.WithKey("k"))
		if err != nil || !second.Duplicate || second.ID != first.ID {
			t.Fatalf("second = %+v, %v; want a duplicate of the first", second, err)
		}
		// PostgreSQL aborts a transaction after an error; this must still work.
		_, err = tx.Exec(ctx, "INSERT INTO orders VALUES ('o-3')")
		return err
	})
	if err != nil {
		t.Fatalf("transaction with a duplicate enqueue failed: %v", err)
	}
	if jobs := count(t, db, "forge_jobs"); jobs != 1 {
		t.Fatalf("%d jobs stored, want 1", jobs)
	}
}

func TestMigrationsCreateTheClaimingIndexes(t *testing.T) {
	db := newDB(t)
	for _, index := range []string{"forge_jobs_claim_idx", "forge_jobs_lease_idx", "forge_jobs_dead_idx", "forge_jobs_idempotency_idx"} {
		var found bool
		if err := db.QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)", index).Scan(&found); err != nil || !found {
			t.Errorf("index %s missing (%v)", index, err)
		}
	}
	var dataType string
	if err := db.QueryRow(context.Background(), "SELECT data_type FROM information_schema.columns WHERE table_name = 'forge_jobs' AND column_name = 'id'").Scan(&dataType); err != nil || dataType != "uuid" {
		t.Errorf("id column type = %q (%v), want uuid", dataType, err)
	}
}

func TestStoreRejectsNilDependencies(t *testing.T) {
	if _, err := queuepostgres.NewStore(nil); err == nil {
		t.Fatal("NewStore(nil) succeeded")
	}
	if _, err := queuepostgres.NewStoreFromPool(nil); err == nil {
		t.Fatal("NewStoreFromPool(nil) succeeded")
	}
}

func TestStoreFailsWhenItsTableIsUnavailable(t *testing.T) {
	storetest.RunUnavailable(t, func(tb testing.TB) (queue.Store, func(testing.TB) func()) {
		db := postgrestest.NewMigrated(tb, queuepostgres.Migrations())
		store, err := queuepostgres.NewStore(db)
		if err != nil {
			tb.Fatal(err)
		}
		rename := func(tb testing.TB, from, to string) {
			tb.Helper()
			if _, err := db.Exec(context.Background(), "ALTER TABLE "+from+" RENAME TO "+to); err != nil {
				tb.Fatal(err)
			}
		}
		return store, func(tb testing.TB) func() {
			rename(tb, "forge_jobs", "forge_jobs_gone")
			return func() { rename(tb, "forge_jobs_gone", "forge_jobs") }
		}
	})
}
