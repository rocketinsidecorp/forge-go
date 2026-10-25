package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/queue"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// NewSabotagedStore returns a store on a fresh, migrated database and a
// function that makes the store's jobs table unavailable, as a dropped or
// renamed table, a revoked permission or a lost connection would. The
// function returns the one that makes it available again.
type NewSabotagedStore func(t testing.TB) (store queue.Store, sabotage func(t testing.TB) (restore func()))

// RunUnavailable checks what every queue.Store must do when its database
// fails: every operation returns the infrastructure error, never one of the
// outcomes that mean something to a worker (a lost lease, a missing or
// conflicting job), and a failed write changes nothing, so the lease a
// worker held is still good when the database returns.
func RunUnavailable(t *testing.T, newStore NewSabotagedStore) {
	t.Helper()
	ctx := context.Background()
	store, sabotage := newStore(t)

	insert := func(kind string, maxAttempts int) uuid.UUID {
		t.Helper()
		id := uuid.MustNew()
		if _, err := store.Insert(ctx, queue.NewJob{
			ID: id, Kind: kind, Queue: queue.DefaultQueue, Payload: json.RawMessage(`{}`), MaxAttempts: maxAttempts,
		}); err != nil {
			t.Fatalf("insert %s: %v", kind, err)
		}
		return id
	}
	claim := func(kind string) queue.Claimed {
		t.Helper()
		claimed, ok, err := store.Claim(ctx, queue.ClaimRequest{Queue: queue.DefaultQueue, Kinds: []string{kind}, WorkerID: "w", Lease: time.Hour})
		if err != nil || !ok {
			t.Fatalf("claim %s: ok=%v err=%v", kind, ok, err)
		}
		return claimed
	}

	pending := insert("k.pending", 3)
	dead := insert("k.dead", 1)
	if err := store.Fail(ctx, claim("k.dead").Lease, queue.Failure{Message: "boom", Dead: true}); err != nil {
		t.Fatal(err)
	}
	done := insert("k.done", 3)
	if err := store.Complete(ctx, claim("k.done").Lease); err != nil {
		t.Fatal(err)
	}
	leased := insert("k.leased", 3)
	lease := claim("k.leased").Lease
	before, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}

	restore := sabotage(t)
	operations := map[string]func() error{
		"Insert": func() error {
			_, err := store.Insert(ctx, queue.NewJob{ID: uuid.MustNew(), Kind: "k.new", Queue: queue.DefaultQueue, Payload: json.RawMessage(`{}`), MaxAttempts: 1})
			return err
		},
		"Insert with a key": func() error {
			_, err := store.Insert(ctx, queue.NewJob{ID: uuid.MustNew(), Kind: "k.new", Queue: queue.DefaultQueue, Payload: json.RawMessage(`{}`), MaxAttempts: 1, Key: "dup"})
			return err
		},
		"Claim": func() error {
			_, _, err := store.Claim(ctx, queue.ClaimRequest{Queue: queue.DefaultQueue, Kinds: []string{"k.pending"}, WorkerID: "w", Lease: time.Hour})
			return err
		},
		"Renew":    func() error { return store.Renew(ctx, lease, time.Hour) },
		"Complete": func() error { return store.Complete(ctx, lease) },
		"Fail":     func() error { return store.Fail(ctx, lease, queue.Failure{Message: "x", RetryIn: time.Second}) },
		"Fail dead": func() error {
			return store.Fail(ctx, lease, queue.Failure{Message: "x", Dead: true})
		},
		"Release": func() error { return store.Release(ctx, lease) },
		"Recover": func() error { _, err := store.Recover(ctx); return err },
		"Stats":   func() error { _, err := store.Stats(ctx); return err },
		"Get":     func() error { _, err := store.Get(ctx, pending); return err },
		"ListDead": func() error {
			_, err := store.ListDead(ctx, queue.DeadQuery{Queue: queue.DefaultQueue})
			return err
		},
		"Retry":   func() error { return store.Retry(ctx, dead) },
		"Discard": func() error { return store.Discard(ctx, dead) },
		"Prune":   func() error { _, err := store.Prune(ctx, 0, 10); return err },
	}
	for name, operation := range operations {
		err := operation()
		switch {
		case err == nil:
			t.Errorf("%s succeeded while the database was unavailable", name)
		case errors.Is(err, queue.ErrLeaseLost), errors.Is(err, queue.ErrNotFound), errors.Is(err, queue.ErrKeyInUse):
			t.Errorf("%s reported %v for an unavailable database; a worker would act on a wrong answer", name, err)
		}
	}
	restore()

	// Nothing changed, and the lease is still good.
	after, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Queues) != len(after.Queues) {
		t.Fatalf("stats changed: %+v -> %+v", before, after)
	}
	for i := range before.Queues {
		b, a := before.Queues[i], after.Queues[i]
		if b.Queue != a.Queue || b.Ready != a.Ready || b.Scheduled != a.Scheduled || b.Running != a.Running || b.Dead != a.Dead || b.Succeeded != a.Succeeded {
			t.Errorf("stats changed: %+v -> %+v", b, a)
		}
	}
	for id, want := range map[uuid.UUID]queue.State{pending: queue.StatePending, dead: queue.StateDead, done: queue.StateSucceeded, leased: queue.StateRunning} {
		if info, err := store.Get(ctx, id); err != nil || info.State != want {
			t.Errorf("job %s = %+v, %v; want %s", id, info.State, err, want)
		}
	}
	if err := store.Complete(ctx, lease); err != nil {
		t.Errorf("the lease did not survive the outage: %v", err)
	}
}

// RunInvalidArguments checks that a store refuses nonsensical requests
// before touching a job: a claim without a queue, kinds or a lease, and a
// prune with a negative retention or no limit. A refused claim leases
// nothing and a refused prune deletes nothing.
func RunInvalidArguments(t *testing.T, newStore func(testing.TB) queue.Store) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)
	id := uuid.MustNew()
	if _, err := store.Insert(ctx, queue.NewJob{ID: id, Kind: "k", Queue: queue.DefaultQueue, Payload: json.RawMessage(`{}`), MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	valid := queue.ClaimRequest{Queue: queue.DefaultQueue, Kinds: []string{"k"}, WorkerID: "w", Lease: time.Minute}
	for name, request := range map[string]queue.ClaimRequest{
		"no queue": {Kinds: valid.Kinds, WorkerID: "w", Lease: valid.Lease},
		"no kinds": {Queue: valid.Queue, WorkerID: "w", Lease: valid.Lease},
		"no lease": {Queue: valid.Queue, Kinds: valid.Kinds, WorkerID: "w"},
		"negative": {Queue: valid.Queue, Kinds: valid.Kinds, WorkerID: "w", Lease: -time.Second},
	} {
		if _, ok, err := store.Claim(ctx, request); err == nil || ok {
			t.Errorf("claim with %s: ok=%v err=%v, want a refusal", name, ok, err)
		}
	}
	for name, args := range map[string]struct {
		retention time.Duration
		limit     int
	}{
		"negative retention": {-time.Second, 10},
		"zero limit":         {time.Hour, 0},
		"negative limit":     {time.Hour, -5},
	} {
		if n, err := store.Prune(ctx, args.retention, args.limit); err == nil || n != 0 {
			t.Errorf("prune with %s = %d, %v; want a refusal", name, n, err)
		}
	}
	// Nothing was leased or deleted: the valid claim still gets the job.
	claimed, ok, err := store.Claim(ctx, valid)
	if err != nil || !ok || claimed.Job.ID != id {
		t.Fatalf("claim after the refusals = %+v, %v, %v", claimed.Job.ID, ok, err)
	}
}
