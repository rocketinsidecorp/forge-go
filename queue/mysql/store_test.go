package mysql_test

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/mysql/mysqltest"
	"github.com/rocketinsidecorp/forge-go/queue"
	queuemysql "github.com/rocketinsidecorp/forge-go/queue/mysql"
	"github.com/rocketinsidecorp/forge-go/queue/storetest"
	"github.com/rocketinsidecorp/forge-go/sqldb"
)

func newDB(t testing.TB) *sqldb.DB {
	t.Helper()
	return mysqltest.NewMigrated(t, queuemysql.Migrations())
}

func newStore(t testing.TB) queue.Store {
	t.Helper()
	store, err := queuemysql.NewStore(newDB(t))
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

func newTxFixture(t *testing.T) (*sqldb.DB, *queuemysql.Store, *queue.Registry, *atomic.Int32) {
	t.Helper()
	db := newDB(t)
	if _, err := db.ExecContext(context.Background(), "CREATE TABLE orders (id VARCHAR(40) PRIMARY KEY) ENGINE = InnoDB"); err != nil {
		t.Fatal(err)
	}
	store, err := queuemysql.NewStore(db)
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
			t.Fatal("job did not run after commit")
		}
		time.Sleep(10 * time.Millisecond)
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
	for _, index := range []string{"forge_jobs_claim_idx", "forge_jobs_lease_idx", "forge_jobs_finished_idx", "forge_jobs_idempotency_idx"} {
		var found int
		err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'forge_jobs' AND index_name = ?", index).Scan(&found)
		if err != nil || found == 0 {
			t.Errorf("index %s missing (%v)", index, err)
		}
	}
	var dataType string
	if err := db.QueryRowContext(context.Background(), "SELECT DATA_TYPE FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'forge_jobs' AND column_name = 'id'").Scan(&dataType); err != nil || dataType != "char" {
		t.Errorf("id column type = %q (%v), want char(36)", dataType, err)
	}
}

func TestSkipLockedIsSupported(t *testing.T) {
	// The claim depends on SKIP LOCKED; fail loudly if a server lacks it.
	db := newDB(t)
	var version string
	if err := db.QueryRowContext(context.Background(), "SELECT VERSION()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("server version: %s", version)
	err := db.InTx(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(context.Background(), "SELECT id FROM forge_jobs LIMIT 1 FOR UPDATE SKIP LOCKED")
		if err != nil {
			return err
		}
		return rows.Close()
	})
	if err != nil {
		t.Fatalf("server does not support FOR UPDATE SKIP LOCKED: %v", err)
	}
}

func TestStoreRejectsNilDatabase(t *testing.T) {
	if _, err := queuemysql.NewStore(nil); err == nil {
		t.Fatal("NewStore(nil) succeeded")
	}
}
