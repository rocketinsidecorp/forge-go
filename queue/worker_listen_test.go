package queue

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// listeningStore is a fakeStore that is a Listener scripted by listen.
type listeningStore struct {
	*fakeStore
	listen func(ctx context.Context, listening func(), wake func(string)) error
	calls  atomic.Int32
}

func (s *listeningStore) Listen(ctx context.Context, listening func(), wake func(string)) error {
	s.calls.Add(1)
	return s.listen(ctx, listening, wake)
}

func startSlowPoller(t *testing.T, store Store, reg *Registry) *Worker {
	t.Helper()
	w, err := NewWorker(store, reg, WorkerOptions{PollInterval: time.Hour, MaxPollInterval: time.Hour, RetainSucceeded: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return w
}

// An announcement of one of the worker's queues ends its idle wait; other
// names are ignored.
func TestAnnouncementWakesTheIdlePoller(t *testing.T) {
	t.Parallel()
	announce := make(chan string)
	var jobs atomic.Int32
	store := &listeningStore{fakeStore: &fakeStore{claim: func(ClaimRequest) (Claimed, bool, error) {
		if jobs.Load() > 0 {
			jobs.Add(-1)
			return newJob(), true, nil
		}
		return Claimed{}, false, nil
	}}}
	store.listen = func(ctx context.Context, listening func(), wake func(string)) error {
		listening()
		for {
			select {
			case name := <-announce:
				wake(name)
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	reg := registerK(t, func(context.Context) error { return nil })
	w := startSlowPoller(t, store, reg)
	waitUntil(t, "the subscription", 5*time.Second, w.Listening)
	waitUntil(t, "the first claims", 5*time.Second, func() bool { return store.claims.Load() >= 1 })
	time.Sleep(20 * time.Millisecond)
	before := store.claims.Load()

	announce <- "some-other-queue"
	announce <- "not a queue name"
	time.Sleep(50 * time.Millisecond)
	if store.claims.Load() != before {
		t.Fatal("an announcement of another queue woke the poller")
	}
	jobs.Store(1)
	announce <- DefaultQueue
	waitUntil(t, "the announced job to run", 5*time.Second, func() bool { return store.completes.Load() == 1 })
	if err := w.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A listener that keeps failing is retried with backoff while polling goes
// on; one the store does not support is not retried at all.
func TestListenerReconnectsWithBackoffAndStopsWhenUnsupported(t *testing.T) {
	t.Parallel()
	reg := registerK(t, func(context.Context) error { return nil })

	failing := &listeningStore{fakeStore: &fakeStore{}}
	failing.listen = func(context.Context, func(), func(string)) error { return errors.New("connection refused") }
	w, err := NewWorker(failing, reg, WorkerOptions{PollInterval: 5 * time.Millisecond, MaxPollInterval: 10 * time.Millisecond, RetainSucceeded: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "a reconnect attempt", 5*time.Second, func() bool { return failing.calls.Load() >= 2 })
	if calls := failing.calls.Load(); calls > 3 {
		t.Fatalf("%d listen attempts in about a second: no backoff", calls)
	}
	if failing.claims.Load() < 10 {
		t.Fatalf("polling did not go on while the listener failed (%d claims)", failing.claims.Load())
	}
	if w.Listening() {
		t.Fatal("Listening() with a failing listener")
	}
	if err := w.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	unsupported := &listeningStore{fakeStore: &fakeStore{}}
	unsupported.listen = func(context.Context, func(), func(string)) error { return ErrListenUnsupported }
	w = newTestWorker(t, unsupported, reg, WorkerOptions{})
	time.Sleep(1500 * time.Millisecond)
	if calls := unsupported.calls.Load(); calls != 1 {
		t.Fatalf("an unsupported listener was called %d times", calls)
	}
	if err := w.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Shutdown cancels the listener and waits for it, promptly.
func TestShutdownStopsAnActiveListener(t *testing.T) {
	t.Parallel()
	returned := make(chan struct{})
	store := &listeningStore{fakeStore: &fakeStore{}}
	store.listen = func(ctx context.Context, listening func(), _ func(string)) error {
		listening()
		<-ctx.Done()
		close(returned)
		return ctx.Err()
	}
	w := startSlowPoller(t, store, registerK(t, func(context.Context) error { return nil }))
	waitUntil(t, "the subscription", 5*time.Second, w.Listening)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	began := time.Now()
	if err := w.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > time.Second {
		t.Fatalf("Shutdown took %s", took)
	}
	select {
	case <-returned:
	default:
		t.Fatal("Shutdown returned before the listener did")
	}
	if w.Listening() {
		t.Fatal("Listening() after Shutdown")
	}
}
