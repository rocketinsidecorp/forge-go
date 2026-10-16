package queue

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

// fakeStore is a scriptable Store for worker lifecycle tests: it hands out
// jobs on demand and lets a test slow down or hang individual operations. The
// database-backed behavior is covered by the storetest suite.
type fakeStore struct {
	Store
	claim    func(ClaimRequest) (Claimed, bool, error)
	renew    func(context.Context, Lease) error
	complete func(context.Context, Lease) error
	fail     func(context.Context, Lease, Failure) error
	release  func(context.Context, Lease) error

	claims, renews, completes, releases atomic.Int32
}

func (s *fakeStore) Claim(_ context.Context, r ClaimRequest) (Claimed, bool, error) {
	s.claims.Add(1)
	if s.claim == nil {
		return Claimed{}, false, nil
	}
	return s.claim(r)
}

func (s *fakeStore) Renew(ctx context.Context, l Lease, _ time.Duration) error {
	s.renews.Add(1)
	if s.renew == nil {
		return nil
	}
	return s.renew(ctx, l)
}

func (s *fakeStore) Complete(ctx context.Context, l Lease) error {
	s.completes.Add(1)
	if s.complete == nil {
		return nil
	}
	return s.complete(ctx, l)
}

func (s *fakeStore) Fail(ctx context.Context, l Lease, f Failure) error {
	if s.fail == nil {
		return nil
	}
	return s.fail(ctx, l, f)
}

func (s *fakeStore) Release(ctx context.Context, l Lease) error {
	s.releases.Add(1)
	if s.release == nil {
		return nil
	}
	return s.release(ctx, l)
}

func (s *fakeStore) Recover(context.Context) (int64, error) { return 0, nil }

func (s *fakeStore) Prune(context.Context, time.Duration, int) (int64, error) { return 0, nil }

// oneJob returns a claim function that hands out a single job of kind "k".
func oneJob() func(ClaimRequest) (Claimed, bool, error) {
	var once sync.Once
	return func(ClaimRequest) (Claimed, bool, error) {
		var claimed Claimed
		given := false
		once.Do(func() {
			id := uuid.MustNew()
			claimed = Claimed{
				Job:     Job{ID: id, Kind: "k", Queue: DefaultQueue, Attempt: 1, MaxAttempts: 3},
				Payload: json.RawMessage(`{}`),
				Lease:   Lease{JobID: id, Token: uuid.MustNew()},
			}
			given = true
		})
		return claimed, given, nil
	}
}

func registerK(t *testing.T, handler func(context.Context) error) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := Register(reg, "k", func(ctx context.Context, _ struct{}) error { return handler(ctx) }); err != nil {
		t.Fatal(err)
	}
	return reg
}

func newTestWorker(t *testing.T, store Store, reg *Registry, opts WorkerOptions) *Worker {
	t.Helper()
	opts.PollInterval, opts.MaxPollInterval = 5*time.Millisecond, 10*time.Millisecond
	if opts.RetainSucceeded == 0 {
		opts.RetainSucceeded = -1
	}
	w, err := NewWorker(store, reg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return w
}

func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A renewal that hangs (a database behind a partition) must not keep the
// handler running past the moment the lease expires in the store.
func TestHandlerIsCanceledBeforeTheLeaseExpiresWhenRenewalHangs(t *testing.T) {
	const lease = time.Second
	canceledAt := make(chan time.Time, 1)
	reg := registerK(t, func(ctx context.Context) error {
		<-ctx.Done()
		canceledAt <- time.Now()
		return ctx.Err()
	})
	store := &fakeStore{claim: oneJob(), renew: func(ctx context.Context, _ Lease) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	began := time.Now()
	w := newTestWorker(t, store, reg, WorkerOptions{Lease: lease})
	defer shutdownQuietly(w)

	select {
	case at := <-canceledAt:
		if after := at.Sub(began); after >= lease {
			t.Fatalf("handler canceled %s after the claim, at or after the %s lease expired in the store", after, lease)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler was never canceled")
	}
}

func shutdownQuietly(w *Worker) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = w.Shutdown(ctx)
}

// Claiming must stop when Shutdown begins, even when a slot is free.
func TestWorkerDoesNotClaimAfterShutdownBegins(t *testing.T) {
	reg := registerK(t, func(context.Context) error { return nil })
	late := 0
	for range 100 {
		var stopping atomic.Bool
		var afterStop atomic.Int32
		inClaim := make(chan struct{})
		var once sync.Once
		store := &fakeStore{claim: func(ClaimRequest) (Claimed, bool, error) {
			if stopping.Load() {
				afterStop.Add(1)
			}
			once.Do(func() { close(inClaim) })
			time.Sleep(10 * time.Millisecond)
			id := uuid.MustNew()
			return Claimed{Job: Job{ID: id, Kind: "k", Queue: DefaultQueue, Attempt: 1, MaxAttempts: 3}, Payload: json.RawMessage(`{}`), Lease: Lease{JobID: id, Token: uuid.MustNew()}}, true, nil
		}}
		w := newTestWorker(t, store, reg, WorkerOptions{Queues: []QueueConfig{{Name: DefaultQueue, Concurrency: 5}}})
		<-inClaim
		stopping.Store(true)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := w.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
		cancel()
		late += int(afterStop.Load())
	}
	if late > 0 {
		t.Fatalf("%d claims started after Shutdown began", late)
	}
}

// A forced shutdown must not release a job whose handler is still running:
// another worker would run it concurrently. The lease is left to expire, and
// it must no longer be renewed.
func TestForcedShutdownLeavesRunningHandlersToLeaseExpiry(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	reg := registerK(t, func(context.Context) error {
		close(started)
		<-release // an uncooperative handler: ignores its context
		return nil
	})
	store := &fakeStore{claim: oneJob()}
	w := newTestWorker(t, store, reg, WorkerOptions{Lease: 150 * time.Millisecond, ReleaseTimeout: 100 * time.Millisecond})
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := w.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("Shutdown = %v, want a deadline error reporting handlers still running", err)
	}
	if n := store.releases.Load(); n != 0 {
		t.Fatalf("Shutdown released %d job(s) whose handler was still running", n)
	}
	renews := store.renews.Load()
	time.Sleep(300 * time.Millisecond)
	if after := store.renews.Load(); after != renews {
		t.Fatalf("lease renewed %d more time(s) after a forced shutdown; it must be left to expire", after-renews)
	}
	close(release)
	waitUntil(t, "the abandoned handler acknowledgement", 5*time.Second, func() bool { return store.completes.Load() == 1 })
}

// Acknowledgements that were already in flight when a forced shutdown began
// must finish before Shutdown returns (the caller closes the pool next).
func TestForcedShutdownWaitsForInFlightAcknowledgements(t *testing.T) {
	var completeDone atomic.Bool
	reg := registerK(t, func(ctx context.Context) error {
		time.Sleep(250 * time.Millisecond) // ignores ctx, then succeeds
		return nil
	})
	store := &fakeStore{claim: oneJob(), complete: func(context.Context, Lease) error {
		time.Sleep(200 * time.Millisecond)
		completeDone.Store(true)
		return nil
	}}
	w := newTestWorker(t, store, reg, WorkerOptions{Lease: 10 * time.Second, ReleaseTimeout: 300 * time.Millisecond})
	waitUntil(t, "claim", 5*time.Second, func() bool { return store.claims.Load() > 0 })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := w.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want the deadline error", err)
	}
	if !completeDone.Load() {
		t.Fatal("Shutdown returned while a Complete write was still in flight")
	}
}

type panicError2 struct{ msg *string }

func (e *panicError2) Error() string { return *e.msg }

func TestFailureWithPanickingErrorMethodDoesNotCrashTheWorker(t *testing.T) {
	var failed atomic.Bool
	reg := registerK(t, func(context.Context) error {
		var e *panicError2
		return error(e)
	})
	store := &fakeStore{claim: oneJob(), fail: func(_ context.Context, _ Lease, f Failure) error {
		if !strings.Contains(f.Message, "error text unavailable") {
			t.Errorf("stored message = %q", f.Message)
		}
		failed.Store(true)
		return nil
	}}
	w := newTestWorker(t, store, reg, WorkerOptions{})
	defer shutdownQuietly(w)
	waitUntil(t, "Fail to be stored", 5*time.Second, failed.Load)
}
