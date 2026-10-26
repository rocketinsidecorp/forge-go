package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/waittest"
	"github.com/rocketinsidecorp/forge-go/queue"
	queuesqlite "github.com/rocketinsidecorp/forge-go/queue/sqlite"
	"github.com/rocketinsidecorp/forge-go/queue/storetest"
	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
)

func newDB(t testing.TB) *sqldb.DB {
	t.Helper()
	return sqlitetest.NewMigrated(t, queuesqlite.Migrations())
}

func newStore(t testing.TB) queue.Store {
	t.Helper()
	store, err := queuesqlite.NewStore(newDB(t))
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

func newTxFixture(t *testing.T) (*sqldb.DB, *queuesqlite.Store, *queue.Registry, *atomic.Int32) {
	t.Helper()
	db := newDB(t)
	if _, err := db.ExecContext(context.Background(), "CREATE TABLE orders (id TEXT PRIMARY KEY) STRICT"); err != nil {
		t.Fatal(err)
	}
	store, err := queuesqlite.NewStore(db)
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

func count(t *testing.T, db *sqldb.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEnqueueRollsBackWithTheTransaction(t *testing.T) {
	db, store, registry, _ := newTxFixture(t)
	ctx := context.Background()
	wantErr := errors.New("business rule failed after enqueue")
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO orders VALUES ('o-1')"); err != nil {
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

	err = db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO orders VALUES ('o-2')"); err != nil {
			return err
		}
		if _, err := queue.Enqueue(ctx, store.In(tx), registry, "order.confirm", order{ID: "o-2"}); err != nil {
			return err
		}
		// The job exists for this transaction only: a running worker must not
		// see, claim or run it before commit.
		time.Sleep(300 * time.Millisecond) // real-clock: negative check: a running worker must not see the uncommitted job in real time
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
	if !waittest.Poll(10*time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatal("job did not run after commit")
	}
	if jobs, orders := count(t, db, "forge_jobs"), count(t, db, "orders"); jobs != 1 || orders != 1 {
		t.Fatalf("after commit: %d jobs, %d orders; want one of each", jobs, orders)
	}
}

func TestDuplicateKeyInsideATransaction(t *testing.T) {
	db, store, registry, _ := newTxFixture(t)
	ctx := context.Background()
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		first, err := queue.Enqueue(ctx, store.In(tx), registry, "order.confirm", order{ID: "a"}, queue.WithKey("k"))
		if err != nil || first.Duplicate {
			t.Fatalf("first = %+v, %v", first, err)
		}
		second, err := queue.Enqueue(ctx, store.In(tx), registry, "order.confirm", order{ID: "b"}, queue.WithKey("k"))
		if err != nil || !second.Duplicate || second.ID != first.ID {
			t.Fatalf("second = %+v, %v; want a duplicate of the first", second, err)
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO orders VALUES ('o-3')")
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
		var found int
		err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?", index).Scan(&found)
		if err != nil || found != 1 {
			t.Errorf("index %s missing (%v)", index, err)
		}
	}
	var strict int
	if err := db.QueryRowContext(context.Background(), "SELECT strict FROM pragma_table_list WHERE name = 'forge_jobs'").Scan(&strict); err != nil || strict != 1 {
		t.Errorf("forge_jobs is not a STRICT table (%d, %v)", strict, err)
	}
}

func TestClaimUsesTheIndexInsteadOfScanningTheTable(t *testing.T) {
	db := newDB(t)
	rows, err := db.QueryContext(context.Background(), `
		EXPLAIN QUERY PLAN
		SELECT id FROM forge_jobs
		WHERE state = 'pending' AND queue = 'default' AND kind IN ('a') AND run_at <= '2999-01-01 00:00:00.000000'
		ORDER BY priority, run_at, id LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if !strings.Contains(plan, "forge_jobs_claim_idx") {
		t.Fatalf("claim query does not use the claim index:\n%s", plan)
	}
}

func TestStoreRejectsNilDatabase(t *testing.T) {
	if _, err := queuesqlite.NewStore(nil); err == nil {
		t.Fatal("NewStore(nil) succeeded")
	}
}

func TestRunDrainsAndReturnsWhenItsContextEnds(t *testing.T) {
	_, store, registry, ran := newTxFixture(t)
	worker, err := queue.NewWorker(store, registry, queue.WorkerOptions{PollInterval: 10 * time.Millisecond, MaxPollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(context.Background(), store, registry, "order.confirm", order{ID: "r"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	if !waittest.Poll(10*time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatal("job did not run")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil after a clean drain", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestStoreFailsWhenItsTableIsUnavailable(t *testing.T) {
	storetest.RunUnavailable(t, func(tb testing.TB) (queue.Store, func(testing.TB) func()) {
		db := newDB(tb)
		store, err := queuesqlite.NewStore(db)
		if err != nil {
			tb.Fatal(err)
		}
		rename := func(tb testing.TB, from, to string) {
			tb.Helper()
			if _, err := db.ExecContext(context.Background(), "ALTER TABLE "+from+" RENAME TO "+to); err != nil {
				tb.Fatal(err)
			}
		}
		return store, func(tb testing.TB) func() {
			rename(tb, "forge_jobs", "forge_jobs_gone")
			return func() { rename(tb, "forge_jobs_gone", "forge_jobs") }
		}
	})
}

func TestStoreRefusesInvalidArguments(t *testing.T) {
	storetest.RunInvalidArguments(t, newStore)
}
