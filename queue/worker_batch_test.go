package queue

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

// batchStore is a fakeStore that also claims in batches from a fixed supply
// of jobs, recording the limits it was asked for.
type batchStore struct {
	*fakeStore
	mu      sync.Mutex
	supply  int
	extra   int // jobs returned beyond the limit, to simulate a broken store
	limits  []int
	batches atomic.Int32
}

func newJob() Claimed {
	id := uuid.MustNew()
	return Claimed{
		Job:     Job{ID: id, Kind: "k", Queue: DefaultQueue, Attempt: 1, MaxAttempts: 3},
		Payload: json.RawMessage(`{}`),
		Lease:   Lease{JobID: id, Token: uuid.MustNew()},
	}
}

func (s *batchStore) ClaimBatch(_ context.Context, _ ClaimRequest, limit int) ([]Claimed, error) {
	s.batches.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limits = append(s.limits, limit)
	n := min(limit+s.extra, s.supply)
	s.supply -= n
	claimed := make([]Claimed, n)
	for i := range claimed {
		claimed[i] = newJob()
	}
	return claimed, nil
}

func (s *batchStore) requested() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.limits...)
}

func TestWorkerValidatesBatchSize(t *testing.T) {
	t.Parallel()
	reg := registerK(t, func(context.Context) error { return nil })
	for _, bad := range []int{-1, MaxClaimBatch + 1} {
		if _, err := NewWorker(&fakeStore{}, reg, WorkerOptions{BatchSize: bad}); err == nil {
			t.Errorf("BatchSize %d accepted", bad)
		}
	}
	w, err := NewWorker(&fakeStore{}, reg, WorkerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if w.opts.BatchSize != defaultBatchSize {
		t.Fatalf("default BatchSize = %d, want %d", w.opts.BatchSize, defaultBatchSize)
	}
}

// A batch never asks for more jobs than the queue has free slots, so every
// claimed job starts at once.
func TestBatchClaimsNeverExceedFreeSlots(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var running, peak atomic.Int32
		reg := registerK(t, func(context.Context) error {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			<-release
			running.Add(-1)
			return nil
		})
		store := &batchStore{fakeStore: &fakeStore{}, supply: 50}
		w := newTestWorker(t, store, reg, WorkerOptions{BatchSize: 10, Queues: []QueueConfig{{Name: DefaultQueue, Concurrency: 4}}})
		waitUntil(t, "four running jobs", 5*time.Second, func() bool { return running.Load() == 4 })
		synctest.Wait() // a saturated poller must not claim: checked once every goroutine is idle
		if limits := store.requested(); len(limits) != 1 || limits[0] != 4 {
			t.Fatalf("claims while saturated asked for %v, want one batch of 4", limits)
		}
		close(release)
		waitUntil(t, "the supply to drain", 5*time.Second, func() bool {
			store.mu.Lock()
			defer store.mu.Unlock()
			return store.supply == 0
		})
		if err := w.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, limit := range store.requested() {
			if limit < 1 || limit > 4 {
				t.Fatalf("a batch asked for %d jobs with 4 slots", limit)
			}
		}
		if p := peak.Load(); p > 4 {
			t.Fatalf("%d jobs ran at once with concurrency 4", p)
		}
	})
}

// BatchSize 1, or a store without ClaimBatch, claims one job per round trip.
func TestBatchSizeOneAndPlainStoresUseClaim(t *testing.T) {
	t.Parallel()
	reg := registerK(t, func(context.Context) error { return nil })
	batched := &batchStore{fakeStore: &fakeStore{claim: oneJob()}, supply: 5}
	w := newTestWorker(t, batched, reg, WorkerOptions{BatchSize: 1})
	waitUntil(t, "a single claim", 5*time.Second, func() bool { return batched.completes.Load() == 1 })
	if err := w.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if batched.batches.Load() != 0 {
		t.Fatal("BatchSize 1 used ClaimBatch")
	}

	plain := &fakeStore{claim: oneJob()}
	w = newTestWorker(t, plain, reg, WorkerOptions{BatchSize: 10})
	waitUntil(t, "a single claim", 5*time.Second, func() bool { return plain.completes.Load() == 1 })
	if err := w.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A store that returns more jobs than asked has them released, not run
// beyond the worker's concurrency.
func TestJobsBeyondTheBatchLimitAreReleased(t *testing.T) {
	t.Parallel()
	logger, messages := newRecordingLogger()
	reg := registerK(t, func(context.Context) error { return nil })
	store := &batchStore{fakeStore: &fakeStore{}, supply: 4, extra: 2}
	w := newTestWorker(t, store, reg, WorkerOptions{Logger: logger, Queues: []QueueConfig{{Name: DefaultQueue, Concurrency: 2}}})
	waitUntil(t, "the extra jobs to be released", 5*time.Second, func() bool { return store.releases.Load() >= 2 })
	if err := w.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.completes.Load() != 2 {
		t.Fatalf("%d jobs completed, want the two requested", store.completes.Load())
	}
	awaitMessage(t, messages, "store returned more jobs than requested")
}
