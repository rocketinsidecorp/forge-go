package mysql_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/queue"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// A business transaction that enqueues a duplicate of a running job must not
// lock that job: the worker's heartbeat and acknowledgement would block until
// the business transaction ends, the handler would be canceled and the job
// would run again.
func TestDuplicateEnqueueInABusinessTransactionDoesNotBlockTheRunningJob(t *testing.T) {
	db, store, registry, _ := newTxFixture(t)
	ctx := context.Background()
	if _, err := queue.Enqueue(ctx, store, registry, "order.confirm", order{ID: "x"}, queue.WithKey("k")); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.Claim(ctx, queue.ClaimRequest{Queue: "default", Kinds: []string{"order.confirm"}, WorkerID: "w", Lease: time.Minute})
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	// The job is running; its key is still held, so a new enqueue is a duplicate.
	inTx := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- db.InTx(ctx, func(tx *sql.Tx) error {
			result, err := queue.Enqueue(ctx, store.In(tx), registry, "order.confirm", order{ID: "x"}, queue.WithKey("k"))
			if err != nil {
				return err
			}
			if !result.Duplicate {
				t.Error("expected a duplicate")
			}
			close(inTx)
			<-release
			return nil
		})
	}()
	<-inTx

	renewCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	began := time.Now()
	if err := store.Renew(renewCtx, claimed.Lease, time.Minute); err != nil {
		t.Errorf("Renew blocked or failed while a business transaction holds a duplicate enqueue: %v after %s", err, time.Since(began))
	}
	if err := store.Complete(renewCtx, claimed.Lease); err != nil {
		t.Errorf("Complete blocked or failed: %v after %s", err, time.Since(began))
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Lease recovery must not wait for locks held on live jobs: at READ COMMITTED
// it reads the latest committed row instead of queueing behind a lock on a row
// that does not match.
func TestRecoverDoesNotWaitForTheLocksOfLiveJobs(t *testing.T) {
	db, store, _, _ := newTxFixture(t)
	ctx := context.Background()
	insert := func() uuid.UUID {
		id := uuid.MustNew()
		if _, err := store.Insert(ctx, queue.NewJob{ID: id, Kind: "k", Queue: "default", Payload: json.RawMessage(`{}`), MaxAttempts: 3}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	insert()
	insert()
	short, ok, err := store.Claim(ctx, queue.ClaimRequest{Queue: "default", Kinds: []string{"k"}, WorkerID: "a", Lease: 1500 * time.Millisecond})
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	live, ok, err := store.Claim(ctx, queue.ClaimRequest{Queue: "default", Kinds: []string{"k"}, WorkerID: "b", Lease: time.Hour})
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	time.Sleep(2200 * time.Millisecond) // real-clock: the first lease expires on the database clock

	// A heartbeat in flight: the live job's lease_until index entry is locked
	// by an uncommitted transaction.
	tx, err := db.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "UPDATE forge_jobs SET lease_until = DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 2 HOUR) WHERE id = ?", live.Job.ID); err != nil {
		t.Fatal(err)
	}

	sweepCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	began := time.Now()
	n, err := store.Recover(sweepCtx)
	if err != nil || n != 1 {
		t.Fatalf("Recover = %d, %v after %s; want the expired job recovered without waiting for the live job's lock", n, err, time.Since(began))
	}
	if info, _ := store.Get(ctx, short.Job.ID); info.State != queue.StatePending {
		t.Fatalf("expired job is %s, want pending", info.State)
	}
}

func TestRunAtOutsideTheSupportedYearsIsRejected(t *testing.T) {
	_, store, registry, _ := newTxFixture(t)
	for _, year := range []int{10000, 999, 0} {
		_, err := queue.Enqueue(context.Background(), store, registry, "order.confirm", order{ID: "x"}, queue.RunAt(time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)))
		if err == nil {
			t.Errorf("year %d accepted", year)
		}
	}
	// The boundaries are stored.
	for _, year := range []int{1000, 9999} {
		if _, err := queue.Enqueue(context.Background(), store, registry, "order.confirm", order{ID: "x"}, queue.RunAt(time.Date(year, 6, 1, 0, 0, 0, 0, time.UTC))); err != nil {
			t.Errorf("year %d rejected: %v", year, err)
		}
	}
}
