package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/queue"
)

// jsonb's text form adds a space after every ',' and ':', so a payload the
// application accepts (1 MiB of compact JSON) is larger in the database. The
// table's size CHECK must not reject it.
func TestPayloadAtTheApplicationLimitIsStored(t *testing.T) {
	_, store, _, _ := newTxFixture(t)
	reg := queue.NewRegistry()
	type big struct {
		A []int `json:"a"`
	}
	if err := queue.Register(reg, "big", func(context.Context, big) error { return nil }); err != nil {
		t.Fatal(err)
	}
	job, err := queue.Prepare(reg, "big", big{A: make([]int, (queue.MaxPayloadBytes-10)/2)})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(job.Payload) < queue.MaxPayloadBytes-20 {
		t.Fatalf("test payload is only %d bytes", len(job.Payload))
	}
	if _, err := store.Insert(context.Background(), job); err != nil {
		t.Fatalf("Insert of a %d byte payload the application accepts: %v", len(job.Payload), err)
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
}

// Under REPEATABLE READ and SERIALIZABLE, PostgreSQL reports a concurrent
// enqueue of the same key as a serialization failure (40001); the caller's
// retry policy (postgres.TxOptions.Attempts) turns that into one insert plus
// duplicates. At READ COMMITTED no retry is needed.
func TestConcurrentKeyedEnqueueInTransactionsConverges(t *testing.T) {
	for _, iso := range []pgx.TxIsoLevel{postgres.ReadCommitted, postgres.RepeatableRead, postgres.Serializable} {
		db, store, registry, _ := newTxFixture(t)
		var wg sync.WaitGroup
		var mu sync.Mutex
		inserted, duplicates := 0, 0
		start := make(chan struct{})
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				err := db.InTxWith(context.Background(), postgres.TxOptions{Isolation: iso, Attempts: 10}, func(tx pgx.Tx) error {
					if _, err := tx.Exec(context.Background(), "SELECT 1"); err != nil { // take the snapshot first
						return err
					}
					time.Sleep(time.Duration(i) * time.Millisecond) // real-clock: staggers concurrent transactions so their snapshots differ
					r, err := queue.Enqueue(context.Background(), store.In(tx), registry, "order.confirm", order{ID: "x"}, queue.WithKey("same"))
					if err != nil {
						return err
					}
					mu.Lock()
					defer mu.Unlock()
					if r.Duplicate {
						duplicates++
					} else {
						inserted++
					}
					return nil
				})
				if err != nil {
					t.Errorf("isolation %s: %v", iso, err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if rows := count(t, db, "forge_jobs"); rows != 1 {
			t.Errorf("isolation %s: %d jobs stored for one key, want 1", iso, rows)
		}
		if inserted < 1 {
			t.Errorf("isolation %s: no caller reported the insert (%d duplicates)", iso, duplicates)
		}
	}
}
