// Package storetest is the conformance suite every queue.Store must pass. It
// drives a real database through the real Worker, so the guarantees it checks
// (no double execution, fenced acknowledgements, retries, dead jobs,
// graceful shutdown) are the ones applications rely on, whichever adapter
// they choose.
//
//	func TestStore(t *testing.T) {
//	    storetest.Run(t, func(t testing.TB) queue.Store { ... a store on a fresh, migrated database ... })
//	}
package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/queue"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// NewStore returns a store on a fresh, migrated database that the test
// cleans up.
type NewStore func(t testing.TB) queue.Store

// Message is the payload type of the suite's handlers.
type Message struct {
	ID     int    `json:"id"`
	Note   string `json:"note,omitempty"`
	Secret string `json:"secret,omitempty"`
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type env struct {
	t     *testing.T
	store queue.Store
	reg   *queue.Registry
	logs  *syncBuffer
}

func newEnv(t *testing.T, newStore NewStore) *env {
	t.Helper()
	return &env{t: t, store: newStore(t), reg: queue.NewRegistry(), logs: &syncBuffer{}}
}

func (e *env) register(kind string, handler func(context.Context, Message) error, opts ...queue.HandlerOption) {
	e.t.Helper()
	if err := queue.Register(e.reg, kind, handler, opts...); err != nil {
		e.t.Fatal(err)
	}
}

// worker builds a worker with test-speed timings; fields set in opts win.
func (e *env) worker(opts queue.WorkerOptions) *queue.Worker {
	e.t.Helper()
	return e.workerOn(e.store, opts)
}

func (e *env) workerOn(store queue.Store, opts queue.WorkerOptions) *queue.Worker {
	e.t.Helper()
	if opts.PollInterval == 0 {
		opts.PollInterval = 10 * time.Millisecond
	}
	if opts.MaxPollInterval == 0 {
		opts.MaxPollInterval = 30 * time.Millisecond
	}
	if opts.Lease == 0 {
		opts.Lease = 2 * time.Second
	}
	if opts.Backoff == (queue.Backoff{}) {
		opts.Backoff = queue.Backoff{Base: 20 * time.Millisecond, Max: 100 * time.Millisecond}
	}
	if opts.ReleaseTimeout == 0 {
		opts.ReleaseTimeout = time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	w, err := queue.NewWorker(store, e.reg, opts)
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

func (e *env) start(w *queue.Worker) {
	e.t.Helper()
	if err := w.Start(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = w.Shutdown(ctx)
	})
}

func (e *env) enqueue(kind string, message Message, opts ...queue.EnqueueOption) uuid.UUID {
	e.t.Helper()
	result, err := queue.Enqueue(context.Background(), e.store, e.reg, kind, message, opts...)
	if err != nil {
		e.t.Fatalf("enqueue %s: %v", kind, err)
	}
	return result.ID
}

func (e *env) info(id uuid.UUID) queue.Info {
	e.t.Helper()
	info, err := e.store.Get(context.Background(), id)
	if err != nil {
		e.t.Fatalf("get job %s: %v", id, err)
	}
	return info
}

func (e *env) waitState(id uuid.UUID, want queue.State) queue.Info {
	e.t.Helper()
	var info queue.Info
	waitFor(e.t, 20*time.Second, fmt.Sprintf("job %s to be %s", id, want), func() bool {
		var err error
		info, err = e.store.Get(context.Background(), id)
		return err == nil && info.State == want
	})
	return info
}

func waitFor(t testing.TB, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func shutdown(t testing.TB, w *queue.Worker, timeout time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return w.Shutdown(ctx)
}

// insertRaw stores a job with an arbitrary payload, bypassing Prepare.
func (e *env) insertRaw(kind, payload string, maxAttempts int) uuid.UUID {
	e.t.Helper()
	id := uuid.MustNew()
	_, err := e.store.Insert(context.Background(), queue.NewJob{
		ID: id, Kind: kind, Queue: queue.DefaultQueue, Payload: json.RawMessage(payload), MaxAttempts: maxAttempts,
	})
	if err != nil {
		e.t.Fatalf("insert raw job: %v", err)
	}
	return id
}

// Run executes the conformance suite.
func Run(t *testing.T, newStore NewStore) {
	t.Helper()
	cases := []struct {
		name string
		run  func(*testing.T, NewStore)
	}{
		{"TypedRoundTrip", typedRoundTrip},
		{"StrictDecodingKillsUndecodablePayloads", strictDecoding},
		{"InsertValidates", insertValidates},
		{"IdempotencyKey", idempotencyKey},
		{"ScheduledRunAt", scheduledRunAt},
		{"PriorityOrder", priorityOrder},
		{"ConcurrentWorkersNeverRunAJobTwice", concurrentWorkers},
		{"BatchedWorkersRunEachJobExactlyOnce", batchedWorkers},
		{"ConcurrentClaimBatchesAreDisjoint", disjointBatches},
		{"ClaimBatchOrderAndLimit", batchOrderAndLimit},
		{"LeaseExpiryFencesStaleWorker", leaseExpiryFencing},
		{"ExpiredFinalAttemptDies", expiredFinalAttempt},
		{"WorkerLosesLeaseWhenRenewalFails", workerLosesLease},
		{"RetriesWithBackoffThenDies", retriesThenDead},
		{"PermanentAndRetryAfter", permanentAndRetryAfter},
		{"DeadJobsRetryAndDiscard", deadRetryDiscard},
		{"PanicBecomesFailure", panicFailure},
		{"HandlerTimeout", handlerTimeout},
		{"GracefulShutdownWaitsForInFlight", shutdownWaits},
		{"ForcedShutdownReleasesWithoutPenalty", shutdownReleases},
		{"StopClaimingWhenStartContextEnds", startContextCancel},
		{"PerQueueConcurrencyBound", queueConcurrency},
		{"UnknownKindsAreNotClaimed", unknownKinds},
		{"StatsAndPrune", statsAndPrune},
		{"PayloadsNeverReachLogsOrErrors", payloadPrivacy},
		{"ReleaseDoesNotCountTheAttempt", releaseKeepsAttempts},
		{"RecoverRequeuesExpiredLeases", recoverRequeues},
		{"SaturatedWorkerStillRecoversExpiredLeases", saturatedWorkerRecovers},
		{"ForcedShutdownNeverRunsAJobTwiceAtOnce", forcedShutdownNoOverlap},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.run(t, newStore) })
	}
}

func typedRoundTrip(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	got := make(chan queue.Job, 1)
	received := make(chan Message, 1)
	e.register("mail.send", func(ctx context.Context, m Message) error {
		job, ok := queue.JobFromContext(ctx)
		if !ok {
			return errors.New("no job in context")
		}
		got <- job
		received <- m
		return nil
	}, queue.HandlerMaxAttempts(4), queue.HandlerPriority(7))
	id := e.enqueue("mail.send", Message{ID: 42, Note: "héllo <b>&</b>"}, queue.WithKey("k-1"))
	e.start(e.worker(queue.WorkerOptions{}))

	select {
	case m := <-received:
		if m.ID != 42 || m.Note != "héllo <b>&</b>" {
			t.Fatalf("payload round trip = %+v", m)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("handler did not run")
	}
	job := <-got
	if job.ID != id || job.Kind != "mail.send" || job.Queue != queue.DefaultQueue || job.Attempt != 1 || job.MaxAttempts != 4 || job.Priority != 7 || job.Key != "k-1" {
		t.Fatalf("job metadata = %+v", job)
	}
	info := e.waitState(id, queue.StateSucceeded)
	if info.FinishedAt == nil || info.WorkerID != "" || info.Job.Attempt != 1 {
		t.Fatalf("succeeded info = %+v", info)
	}
	if _, err := queue.Prepare(e.reg, "mail.send", 3); err == nil {
		t.Fatal("Prepare accepted a payload of the wrong type")
	}
	if _, err := queue.Prepare(e.reg, "nobody", Message{}); !errors.Is(err, queue.ErrUnknownKind) {
		t.Fatalf("Prepare unknown kind error = %v", err)
	}
}

func strictDecoding(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	var calls atomic.Int32
	e.register("strict", func(context.Context, Message) error { calls.Add(1); return nil })
	unknownField := e.insertRaw("strict", `{"id":1,"bogus":true}`, 5)
	trailing := e.insertRaw("strict", `{"id":"not a number"}`, 5)
	e.start(e.worker(queue.WorkerOptions{}))
	for _, id := range []uuid.UUID{unknownField, trailing} {
		info := e.waitState(id, queue.StateDead)
		if info.Job.Attempt != 1 {
			t.Fatalf("undecodable payload was attempted %d times, want exactly 1 (permanent failure)", info.Job.Attempt)
		}
		if info.LastError == "" {
			t.Fatal("dead job has no error text")
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("handler ran %d times for undecodable payloads", calls.Load())
	}
}

func insertValidates(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	good := queue.NewJob{ID: uuid.MustNew(), Kind: "k", Queue: "q", Payload: json.RawMessage(`{}`), MaxAttempts: 1}
	bad := map[string]func(queue.NewJob) queue.NewJob{
		"kind":     func(j queue.NewJob) queue.NewJob { j.Kind = "Bad Kind"; return j },
		"queue":    func(j queue.NewJob) queue.NewJob { j.Queue = ""; return j },
		"payload":  func(j queue.NewJob) queue.NewJob { j.Payload = json.RawMessage(`{`); return j },
		"attempts": func(j queue.NewJob) queue.NewJob { j.MaxAttempts = 0; return j },
		"id":       func(j queue.NewJob) queue.NewJob { j.ID = uuid.UUID{}; return j },
		"long key": func(j queue.NewJob) queue.NewJob { j.Key = string(bytes.Repeat([]byte("k"), 201)); return j },
		"huge": func(j queue.NewJob) queue.NewJob {
			j.Payload = bytes.Repeat([]byte(" "), queue.MaxPayloadBytes+1)
			return j
		},
		"control key": func(j queue.NewJob) queue.NewJob { j.Key = "a\x00b"; return j },
	}
	for name, mutate := range bad {
		if _, err := e.store.Insert(context.Background(), mutate(good)); err == nil {
			t.Errorf("Insert accepted invalid %s", name)
		}
	}
	if _, err := e.store.Insert(context.Background(), good); err != nil {
		t.Fatalf("Insert valid job: %v", err)
	}
	// Unicode keys and payloads survive.
	unicode := good
	unicode.ID, unicode.Key, unicode.Payload = uuid.MustNew(), "chave-ção-✓", json.RawMessage(`{"n":"日本語"}`)
	if _, err := e.store.Insert(context.Background(), unicode); err != nil {
		t.Fatalf("Insert unicode job: %v", err)
	}
	if info := e.info(unicode.ID); info.Job.Key != "chave-ção-✓" {
		t.Fatalf("unicode key = %q", info.Job.Key)
	}
}

func idempotencyKey(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	e.register("a.job", func(context.Context, Message) error { return nil })
	e.register("b.job", func(context.Context, Message) error { return nil })
	ctx := context.Background()
	first, err := queue.Enqueue(ctx, e.store, e.reg, "a.job", Message{ID: 1}, queue.WithKey("same"))
	if err != nil || first.Duplicate {
		t.Fatalf("first enqueue = %+v, %v", first, err)
	}
	second, err := queue.Enqueue(ctx, e.store, e.reg, "a.job", Message{ID: 2}, queue.WithKey("same"))
	if err != nil || !second.Duplicate || second.ID != first.ID {
		t.Fatalf("second enqueue = %+v, %v; want duplicate of %s", second, err, first.ID)
	}
	other, err := queue.Enqueue(ctx, e.store, e.reg, "b.job", Message{ID: 3}, queue.WithKey("same"))
	if err != nil || other.Duplicate || other.ID == first.ID {
		t.Fatalf("same key on another kind = %+v, %v; keys are per kind", other, err)
	}
	// Only one row exists for the duplicated key.
	stats, err := e.store.Stats(ctx)
	if err != nil || len(stats.Queues) != 1 || stats.Queues[0].Ready != 2 {
		t.Fatalf("stats after duplicate = %+v, %v; want 2 ready jobs", stats, err)
	}
	// Once the holder finishes, the key is free again.
	e.start(e.worker(queue.WorkerOptions{}))
	e.waitState(first.ID, queue.StateSucceeded)
	again, err := queue.Enqueue(ctx, e.store, e.reg, "a.job", Message{ID: 4}, queue.WithKey("same"))
	if err != nil || again.Duplicate || again.ID == first.ID {
		t.Fatalf("enqueue after completion = %+v, %v; want a new job", again, err)
	}
	// Concurrent enqueues of one key create exactly one job.
	var wg sync.WaitGroup
	var created atomic.Int32
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := queue.Enqueue(ctx, e.store, e.reg, "b.job", Message{}, queue.WithKey("race"), queue.RunAfter(time.Hour))
			if err != nil {
				t.Errorf("concurrent enqueue: %v", err)
				return
			}
			if !result.Duplicate {
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 1 {
		t.Fatalf("%d concurrent enqueues created a job, want exactly 1", created.Load())
	}
}

func scheduledRunAt(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	ran := make(chan time.Time, 2)
	e.register("later", func(context.Context, Message) error { ran <- time.Now(); return nil })
	scheduled := time.Now().Add(4 * time.Second)
	id := e.enqueue("later", Message{ID: 1}, queue.RunAt(scheduled))
	immediate := e.enqueue("later", Message{ID: 2})
	e.start(e.worker(queue.WorkerOptions{}))

	e.waitState(immediate, queue.StateSucceeded)
	<-ran
	if info := e.info(id); info.State != queue.StatePending {
		t.Fatalf("scheduled job is %s before its time", info.State)
	}
	stats, _ := e.store.Stats(context.Background())
	if stats.Queues[0].Scheduled != 1 {
		t.Fatalf("stats = %+v, want one scheduled job", stats)
	}
	e.waitState(id, queue.StateSucceeded)
	at := <-ran
	if at.Before(scheduled.Add(-50 * time.Millisecond)) {
		t.Fatalf("job ran %s before its scheduled time", scheduled.Sub(at))
	}
}

func priorityOrder(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	var mu sync.Mutex
	var order []int
	e.register("ordered", func(_ context.Context, m Message) error {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, m.ID)
		return nil
	})
	// Enqueued before the worker starts, so ordering is by priority, then due
	// time (enqueue order within a priority).
	e.enqueue("ordered", Message{ID: 5}, queue.WithPriority(5))
	e.enqueue("ordered", Message{ID: 1}, queue.WithPriority(1))
	e.enqueue("ordered", Message{ID: 3}, queue.WithPriority(3))
	time.Sleep(5 * time.Millisecond)
	e.enqueue("ordered", Message{ID: 31}, queue.WithPriority(3))
	e.enqueue("ordered", Message{ID: 0}, queue.WithPriority(-2))
	e.start(e.worker(queue.WorkerOptions{Queues: []queue.QueueConfig{{Name: queue.DefaultQueue, Concurrency: 1}}}))
	waitFor(t, 20*time.Second, "all jobs", func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 5 })
	mu.Lock()
	defer mu.Unlock()
	want := []int{0, 1, 3, 31, 5}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("execution order = %v, want %v", order, want)
		}
	}
}

func concurrentWorkers(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	const jobs = 240
	var mu sync.Mutex
	runs := make(map[int]int, jobs)
	var finished atomic.Int32
	e.register("stress", func(_ context.Context, m Message) error {
		mu.Lock()
		runs[m.ID]++
		mu.Unlock()
		time.Sleep(time.Millisecond)
		finished.Add(1)
		return nil
	})
	ids := make([]uuid.UUID, 0, jobs)
	for i := range jobs {
		ids = append(ids, e.enqueue("stress", Message{ID: i}))
	}
	for range 4 {
		e.start(e.worker(queue.WorkerOptions{Lease: 30 * time.Second, Queues: []queue.QueueConfig{{Name: queue.DefaultQueue, Concurrency: 4}}}))
	}
	waitFor(t, 60*time.Second, "all stress jobs", func() bool { return finished.Load() >= jobs })
	for _, id := range ids {
		e.waitState(id, queue.StateSucceeded)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(runs) != jobs {
		t.Fatalf("%d distinct jobs ran, want %d", len(runs), jobs)
	}
	for id, n := range runs {
		if n != 1 {
			t.Fatalf("job %d ran %d times", id, n)
		}
	}
	for _, id := range ids {
		if info := e.info(id); info.Job.Attempt != 1 {
			t.Fatalf("job %s attempt = %d, want 1", id, info.Job.Attempt)
		}
	}
}

// batchedWorkers races workers that claim several jobs per round trip: every
// job runs exactly once (the leases are long enough never to expire), on its
// first attempt, and no worker runs more jobs at once than its concurrency.
func batchedWorkers(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	const (
		jobs        = 400
		workers     = 4
		concurrency = 8
	)
	var mu sync.Mutex
	runs := make(map[int]int, jobs)
	var finished, active, peak atomic.Int32
	e.register("batched", func(_ context.Context, m Message) error {
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		mu.Lock()
		runs[m.ID]++
		mu.Unlock()
		time.Sleep(time.Millisecond)
		active.Add(-1)
		finished.Add(1)
		return nil
	})
	ids := make([]uuid.UUID, 0, jobs)
	for i := range jobs {
		ids = append(ids, e.enqueue("batched", Message{ID: i}))
	}
	for range workers {
		e.start(e.worker(queue.WorkerOptions{
			Lease:     30 * time.Second,
			BatchSize: concurrency,
			Queues:    []queue.QueueConfig{{Name: queue.DefaultQueue, Concurrency: concurrency}},
		}))
	}
	waitFor(t, 60*time.Second, "all batched jobs", func() bool { return finished.Load() >= jobs })
	for _, id := range ids {
		if info := e.waitState(id, queue.StateSucceeded); info.Job.Attempt != 1 {
			t.Fatalf("job %s attempt = %d, want 1", id, info.Job.Attempt)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(runs) != jobs {
		t.Fatalf("%d distinct jobs ran, want %d", len(runs), jobs)
	}
	for id, n := range runs {
		if n != 1 {
			t.Fatalf("job %d ran %d times", id, n)
		}
	}
	if p := peak.Load(); p > workers*concurrency {
		t.Fatalf("%d handlers ran at once, above %d workers x %d slots", p, workers, concurrency)
	}
}

// disjointBatches calls ClaimBatch from many goroutines at once: no job is
// leased twice, every job is leased, and each lease fences its own job.
func disjointBatches(t *testing.T, newStore NewStore) {
	store := newStore(t)
	batcher, ok := store.(queue.BatchClaimer)
	if !ok {
		t.Skip("the store does not implement queue.BatchClaimer")
	}
	const (
		jobs     = 300
		claimers = 8
		limit    = 7
	)
	inserted := make(map[uuid.UUID]bool, jobs)
	for range jobs {
		inserted[insertK(t, store, 3)] = true
	}
	var (
		mu      sync.Mutex
		claimed = make(map[uuid.UUID]int, jobs)
		wg      sync.WaitGroup
		failure atomic.Value
	)
	request := queue.ClaimRequest{Queue: queue.DefaultQueue, Kinds: []string{"k"}, Lease: time.Minute}
	for i := range claimers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := request
			r.WorkerID = fmt.Sprintf("claimer-%d", i)
			for {
				batch, err := batcher.ClaimBatch(context.Background(), r, limit)
				if err != nil {
					failure.Store(err)
					return
				}
				if len(batch) == 0 {
					return
				}
				if len(batch) > limit {
					failure.Store(fmt.Errorf("ClaimBatch returned %d jobs, limit %d", len(batch), limit))
					return
				}
				for _, c := range batch {
					if c.Job.Attempt != 1 || c.Lease.JobID != c.Job.ID || string(c.Payload) == "" {
						failure.Store(fmt.Errorf("bad claim %+v", c))
						return
					}
					mu.Lock()
					claimed[c.Job.ID]++
					mu.Unlock()
					if err := store.Complete(context.Background(), c.Lease); err != nil {
						failure.Store(fmt.Errorf("complete %s: %w", c.Job.ID, err))
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	if err, _ := failure.Load().(error); err != nil {
		t.Fatal(err)
	}
	if len(claimed) != jobs {
		t.Fatalf("%d distinct jobs claimed, want %d", len(claimed), jobs)
	}
	for id, n := range claimed {
		if !inserted[id] || n != 1 {
			t.Fatalf("job %s claimed %d times (inserted: %v)", id, n, inserted[id])
		}
	}
}

// batchOrderAndLimit checks that a batch takes the first jobs in claim order
// (priority, then due time) up to its limit, and that limits are validated.
func batchOrderAndLimit(t *testing.T, newStore NewStore) {
	store := newStore(t)
	batcher, ok := store.(queue.BatchClaimer)
	if !ok {
		t.Skip("the store does not implement queue.BatchClaimer")
	}
	ctx := context.Background()
	byPriority := map[int]uuid.UUID{}
	for _, priority := range []int{5, -1, 3, 0, 9} {
		id := uuid.MustNew()
		if _, err := store.Insert(ctx, queue.NewJob{ID: id, Kind: "k", Queue: queue.DefaultQueue, Payload: json.RawMessage(`{"id":1}`), MaxAttempts: 3, Priority: priority}); err != nil {
			t.Fatal(err)
		}
		byPriority[priority] = id
	}
	request := queue.ClaimRequest{Queue: queue.DefaultQueue, Kinds: []string{"k"}, WorkerID: "w", Lease: time.Minute}
	for _, bad := range []int{0, -1, queue.MaxClaimBatch + 1} {
		if _, err := batcher.ClaimBatch(ctx, request, bad); err == nil {
			t.Errorf("ClaimBatch limit %d accepted", bad)
		}
	}
	first, err := batcher.ClaimBatch(ctx, request, 3)
	if err != nil || len(first) != 3 {
		t.Fatalf("ClaimBatch(3) = %d jobs, %v; want 3", len(first), err)
	}
	got := map[uuid.UUID]bool{}
	for _, c := range first {
		got[c.Job.ID] = true
		if info, err := store.Get(ctx, c.Job.ID); err != nil || info.State != queue.StateRunning || info.WorkerID != "w" || info.LeaseUntil == nil {
			t.Fatalf("claimed job %s: %+v, %v; want running under w", c.Job.ID, info, err)
		}
	}
	for _, priority := range []int{-1, 0, 3} {
		if !got[byPriority[priority]] {
			t.Fatalf("first batch misses the job of priority %d", priority)
		}
	}
	rest, err := batcher.ClaimBatch(ctx, request, queue.MaxClaimBatch)
	if err != nil || len(rest) != 2 {
		t.Fatalf("ClaimBatch(max) = %d jobs, %v; want the 2 left", len(rest), err)
	}
	if none, err := batcher.ClaimBatch(ctx, request, 5); err != nil || len(none) != 0 {
		t.Fatalf("ClaimBatch on an empty queue = %d jobs, %v", len(none), err)
	}
	// Each lease of the batch fences its own job only.
	if err := store.Complete(ctx, queue.Lease{JobID: rest[0].Job.ID, Token: first[0].Lease.Token}); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("Complete with another claim's token = %v, want ErrLeaseLost", err)
	}
	for _, c := range append(first, rest...) {
		if err := store.Complete(ctx, c.Lease); err != nil {
			t.Fatalf("Complete %s: %v", c.Job.ID, err)
		}
	}
}

func claimOne(t *testing.T, store queue.Store, worker string, lease time.Duration) queue.Claimed {
	t.Helper()
	claimed, ok, err := store.Claim(context.Background(), queue.ClaimRequest{Queue: queue.DefaultQueue, Kinds: []string{"k"}, WorkerID: worker, Lease: lease})
	if err != nil || !ok {
		t.Fatalf("Claim by %s = %v, %v; want a job", worker, ok, err)
	}
	return claimed
}

// recoverAll is what a worker's recovery loop does: Claim leaves expired
// leases alone.
func recoverAll(t *testing.T, store queue.Store) {
	t.Helper()
	if _, err := store.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
}

func insertK(t *testing.T, store queue.Store, maxAttempts int) uuid.UUID {
	t.Helper()
	id := uuid.MustNew()
	if _, err := store.Insert(context.Background(), queue.NewJob{ID: id, Kind: "k", Queue: queue.DefaultQueue, Payload: json.RawMessage(`{"id":1}`), MaxAttempts: maxAttempts}); err != nil {
		t.Fatal(err)
	}
	return id
}

func leaseExpiryFencing(t *testing.T, newStore NewStore) {
	store := newStore(t)
	ctx := context.Background()
	id := insertK(t, store, 5)

	stale := claimOne(t, store, "worker-a", 2*time.Second)
	if stale.Job.Attempt != 1 {
		t.Fatalf("first claim attempt = %d", stale.Job.Attempt)
	}
	if _, ok, err := store.Claim(ctx, queue.ClaimRequest{Queue: queue.DefaultQueue, Kinds: []string{"k"}, WorkerID: "worker-b", Lease: time.Minute}); err != nil || ok {
		t.Fatalf("Claim while leased = %v, %v; want none", ok, err)
	}
	// A live lease is renewable.
	if err := store.Renew(ctx, stale.Lease, 2*time.Second); err != nil {
		t.Fatalf("Renew live lease: %v", err)
	}
	time.Sleep(2600 * time.Millisecond) // the lease expires
	recoverAll(t, store)

	fresh := claimOne(t, store, "worker-b", time.Minute)
	if fresh.Job.ID != id || fresh.Job.Attempt != 2 {
		t.Fatalf("reclaimed job = %+v; want the same job on attempt 2", fresh.Job)
	}
	if fresh.Lease.Token == stale.Lease.Token {
		t.Fatal("reclaim reused the lease token")
	}
	// The stale worker is fenced out of every write.
	if err := store.Renew(ctx, stale.Lease, time.Minute); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("stale Renew = %v, want ErrLeaseLost", err)
	}
	if err := store.Complete(ctx, stale.Lease); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("stale Complete = %v, want ErrLeaseLost", err)
	}
	if err := store.Fail(ctx, stale.Lease, queue.Failure{Message: "late", Dead: true}); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("stale Fail = %v, want ErrLeaseLost", err)
	}
	if err := store.Release(ctx, stale.Lease); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("stale Release = %v, want ErrLeaseLost", err)
	}
	info, err := store.Get(ctx, id)
	if err != nil || info.State != queue.StateRunning || info.WorkerID != "worker-b" {
		t.Fatalf("after stale writes: %+v, %v; want running under worker-b", info, err)
	}
	if err := store.Complete(ctx, fresh.Lease); err != nil {
		t.Fatalf("current Complete: %v", err)
	}
	if err := store.Complete(ctx, fresh.Lease); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("second Complete = %v, want ErrLeaseLost", err)
	}
	if info, _ := store.Get(ctx, id); info.State != queue.StateSucceeded {
		t.Fatalf("final state = %s", info.State)
	}
}

func expiredFinalAttempt(t *testing.T, newStore NewStore) {
	store := newStore(t)
	ctx := context.Background()
	id := insertK(t, store, 1)
	claimOne(t, store, "worker-a", 100*time.Millisecond)
	time.Sleep(600 * time.Millisecond)
	recoverAll(t, store)
	if _, ok, err := store.Claim(ctx, queue.ClaimRequest{Queue: queue.DefaultQueue, Kinds: []string{"k"}, WorkerID: "worker-b", Lease: time.Minute}); err != nil || ok {
		t.Fatalf("Claim after final-attempt expiry = %v, %v; want none", ok, err)
	}
	info, err := store.Get(ctx, id)
	if err != nil || info.State != queue.StateDead || info.FinishedAt == nil {
		t.Fatalf("after expiry on the last attempt: %+v, %v; want dead", info, err)
	}
	if info.LastError == "" {
		t.Fatal("dead job has no error text")
	}
}

// flakyRenewStore fails every Renew while broken is set, simulating a worker
// that cannot reach the database to renew its lease.
type flakyRenewStore struct {
	queue.Store
	broken atomic.Bool
}

func (s *flakyRenewStore) Renew(ctx context.Context, lease queue.Lease, extend time.Duration) error {
	if s.broken.Load() {
		return errors.New("simulated renew failure")
	}
	return s.Store.Renew(ctx, lease, extend)
}

func workerLosesLease(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	flaky := &flakyRenewStore{Store: e.store}
	flaky.broken.Store(true)

	started := make(chan struct{})
	canceled := make(chan error, 1)
	release := make(chan struct{})
	var runs atomic.Int32
	e.register("slow", func(ctx context.Context, m Message) error {
		if runs.Add(1) == 1 { // the first worker, which cannot renew
			close(started)
			<-ctx.Done()
			canceled <- context.Cause(ctx)
			<-release // an uncooperative handler: keeps running after cancellation
			return nil
		}
		return nil // the worker that reclaims the job
	})
	id := e.enqueue("slow", Message{ID: 1})

	var outcomesMu sync.Mutex
	var outcomes []queue.Outcome
	stale := e.workerOn(flaky, queue.WorkerOptions{
		Lease:  300 * time.Millisecond,
		Queues: []queue.QueueConfig{{Name: queue.DefaultQueue, Concurrency: 1}}, // the stuck handler holds the only slot
		Hooks: queue.Hooks{Finished: func(r queue.Result) {
			outcomesMu.Lock()
			outcomes = append(outcomes, r.Outcome)
			outcomesMu.Unlock()
		}},
	})
	e.start(stale)
	<-started

	// The stale worker's heartbeat gives up once a full lease passed without
	// a successful renewal and cancels the handler.
	select {
	case cause := <-canceled:
		if !errors.Is(cause, queue.ErrLeaseLost) {
			t.Fatalf("handler canceled with %v, want ErrLeaseLost", cause)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler was not canceled after the lease went unrenewed")
	}

	// A healthy worker reclaims and finishes the job.
	e.start(e.worker(queue.WorkerOptions{Lease: 20 * time.Second}))
	info := e.waitState(id, queue.StateSucceeded)
	if info.Job.Attempt != 2 {
		t.Fatalf("job finished on attempt %d, want 2", info.Job.Attempt)
	}

	// The stale handler finally returns; its result must be discarded.
	close(release)
	waitFor(t, 10*time.Second, "stale worker to report", func() bool {
		outcomesMu.Lock()
		defer outcomesMu.Unlock()
		return len(outcomes) == 1
	})
	outcomesMu.Lock()
	defer outcomesMu.Unlock()
	if outcomes[0] != queue.OutcomeLeaseLost {
		t.Fatalf("stale worker outcome = %s, want lease_lost", outcomes[0])
	}
	if after := e.info(id); after.State != queue.StateSucceeded || after.Job.Attempt != 2 {
		t.Fatalf("stale worker changed the job: %+v", after)
	}
}

func retriesThenDead(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	var mu sync.Mutex
	var attempts []time.Time
	e.register("flaky", func(_ context.Context, m Message) error {
		mu.Lock()
		attempts = append(attempts, time.Now())
		mu.Unlock()
		return fmt.Errorf("upstream rejected %s", m.Secret)
	}, queue.HandlerMaxAttempts(3), queue.HandlerBackoff(queue.Backoff{Base: 100 * time.Millisecond, Max: 400 * time.Millisecond}))
	var outcomesMu sync.Mutex
	var outcomes []queue.Outcome
	id := e.enqueue("flaky", Message{ID: 1, Secret: "sk_live_topsecretvalue"}) // #nosec G101 -- fixture value, not a credential
	e.start(e.worker(queue.WorkerOptions{Hooks: queue.Hooks{Finished: func(r queue.Result) {
		outcomesMu.Lock()
		outcomes = append(outcomes, r.Outcome)
		outcomesMu.Unlock()
	}}}))

	info := e.waitState(id, queue.StateDead)
	if info.Job.Attempt != 3 {
		t.Fatalf("dead after %d attempts, want 3", info.Job.Attempt)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 3 {
		t.Fatalf("handler ran %d times, want 3", len(attempts))
	}
	// Equal jitter: the first retry waits 50-100ms, the second 100-200ms.
	if gap := attempts[1].Sub(attempts[0]); gap < 45*time.Millisecond {
		t.Errorf("first retry gap = %s, want at least half the 100ms base", gap)
	}
	if gap := attempts[2].Sub(attempts[1]); gap < 95*time.Millisecond {
		t.Errorf("second retry gap = %s, want at least half the 200ms ceiling", gap)
	}
	waitFor(t, 5*time.Second, "hooks", func() bool { outcomesMu.Lock(); defer outcomesMu.Unlock(); return len(outcomes) == 3 })
	outcomesMu.Lock()
	defer outcomesMu.Unlock()
	if outcomes[0] != queue.OutcomeRetry || outcomes[1] != queue.OutcomeRetry || outcomes[2] != queue.OutcomeDead {
		t.Fatalf("outcomes = %v", outcomes)
	}
	if info.LastError == "" || bytes.Contains([]byte(info.LastError), []byte("sk_live_topsecretvalue")) {
		t.Fatalf("stored error %q is empty or leaks the payload", info.LastError)
	}
}

func permanentAndRetryAfter(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	var mu sync.Mutex
	var retries []time.Time
	e.register("perm", func(context.Context, Message) error { return queue.Permanent(errors.New("invalid input")) })
	e.register("after", func(context.Context, Message) error {
		mu.Lock()
		defer mu.Unlock()
		retries = append(retries, time.Now())
		if len(retries) == 1 {
			return queue.RetryAfter(400*time.Millisecond, errors.New("rate limited"))
		}
		return nil
	})
	perm := e.enqueue("perm", Message{ID: 1})
	after := e.enqueue("after", Message{ID: 2})
	e.start(e.worker(queue.WorkerOptions{}))

	info := e.waitState(perm, queue.StateDead)
	if info.Job.Attempt != 1 {
		t.Fatalf("permanent failure was attempted %d times", info.Job.Attempt)
	}
	e.waitState(after, queue.StateSucceeded)
	mu.Lock()
	defer mu.Unlock()
	if gap := retries[1].Sub(retries[0]); gap < 380*time.Millisecond {
		t.Fatalf("RetryAfter(400ms) retried after %s", gap)
	}
}

func deadRetryDiscard(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	ctx := context.Background()
	var healthy atomic.Bool
	e.register("fragile", func(context.Context, Message) error {
		if healthy.Load() {
			return nil
		}
		return errors.New("downstream is down")
	}, queue.HandlerMaxAttempts(1))
	e.register("other", func(context.Context, Message) error { return queue.Permanent(errors.New("nope")) })

	a := e.enqueue("fragile", Message{ID: 1})
	b := e.enqueue("fragile", Message{ID: 2})
	c := e.enqueue("fragile", Message{ID: 3}, queue.WithKey("dup"))
	o := e.enqueue("other", Message{ID: 4}, queue.WithQueue("special"))
	w := e.worker(queue.WorkerOptions{Queues: []queue.QueueConfig{{Name: queue.DefaultQueue, Concurrency: 1}, {Name: "special", Concurrency: 1}}})
	e.start(w)
	for _, id := range []uuid.UUID{a, b, c, o} {
		e.waitState(id, queue.StateDead)
	}

	// Listing: filters and keyset pagination.
	all, err := e.store.ListDead(ctx, queue.DeadQuery{Limit: 2})
	if err != nil || len(all.Items) != 2 || all.Next == nil {
		t.Fatalf("first page = %+v, %v", all, err)
	}
	rest, err := e.store.ListDead(ctx, queue.DeadQuery{Limit: 2, After: all.Next})
	if err != nil || len(rest.Items) != 2 || rest.Next != nil {
		t.Fatalf("second page = %+v, %v", rest, err)
	}
	seen := map[uuid.UUID]bool{}
	for _, item := range append(all.Items, rest.Items...) {
		seen[item.Job.ID] = true
		if item.State != queue.StateDead || item.FinishedAt == nil || item.LastError == "" {
			t.Fatalf("dead listing item = %+v", item)
		}
	}
	if len(seen) != 4 {
		t.Fatalf("pagination returned %d distinct jobs, want 4", len(seen))
	}
	byQueue, _ := e.store.ListDead(ctx, queue.DeadQuery{Queue: "special"})
	byKind, _ := e.store.ListDead(ctx, queue.DeadQuery{Kind: "fragile"})
	if len(byQueue.Items) != 1 || byQueue.Items[0].Job.ID != o || len(byKind.Items) != 3 {
		t.Fatalf("filters: queue=%d kind=%d", len(byQueue.Items), len(byKind.Items))
	}
	if _, err := e.store.ListDead(ctx, queue.DeadQuery{Limit: queue.MaxPageSize + 1}); err == nil {
		t.Fatal("ListDead accepted an oversized page")
	}

	// Retry: back to pending with attempts reset, then it succeeds.
	healthy.Store(true)
	if err := e.store.Retry(ctx, a); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	info := e.waitState(a, queue.StateSucceeded)
	if info.Job.Attempt != 1 {
		t.Fatalf("retried job attempt = %d, want 1 (counter reset)", info.Job.Attempt)
	}
	if err := e.store.Retry(ctx, a); !errors.Is(err, queue.ErrNotFound) {
		t.Fatalf("Retry of a succeeded job = %v, want ErrNotFound", err)
	}
	if err := e.store.Retry(ctx, uuid.MustNew()); !errors.Is(err, queue.ErrNotFound) {
		t.Fatalf("Retry of an unknown job = %v, want ErrNotFound", err)
	}

	// Discard deletes dead jobs and nothing else.
	if err := e.store.Discard(ctx, b); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, err := e.store.Get(ctx, b); !errors.Is(err, queue.ErrNotFound) {
		t.Fatalf("Get after Discard = %v, want ErrNotFound", err)
	}
	if err := e.store.Discard(ctx, b); !errors.Is(err, queue.ErrNotFound) {
		t.Fatalf("second Discard = %v, want ErrNotFound", err)
	}
	if err := e.store.Discard(ctx, a); !errors.Is(err, queue.ErrNotFound) {
		t.Fatalf("Discard of a succeeded job = %v, want ErrNotFound", err)
	}

	// A dead job whose key was taken by a newer active job cannot be retried.
	healthy.Store(false)
	e.enqueue("fragile", Message{ID: 5}, queue.WithKey("dup"), queue.RunAfter(time.Hour))
	if err := e.store.Retry(ctx, c); !errors.Is(err, queue.ErrKeyInUse) {
		t.Fatalf("Retry with the key held = %v, want ErrKeyInUse", err)
	}
	if info := e.info(c); info.State != queue.StateDead {
		t.Fatalf("failed Retry changed the job to %s", info.State)
	}
}

func panicFailure(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	var calls atomic.Int32
	e.register("boom", func(_ context.Context, m Message) error {
		if calls.Add(1) == 1 {
			panic("exploded with " + m.Secret)
		}
		return nil
	}, queue.HandlerMaxAttempts(2))
	e.register("boom.once", func(_ context.Context, m Message) error {
		panic(fmt.Errorf("fatal %s", m.Secret))
	}, queue.HandlerMaxAttempts(1))

	retried := e.enqueue("boom", Message{ID: 1, Secret: "panic-secret-123"})
	dead := e.enqueue("boom.once", Message{ID: 2, Secret: "panic-secret-123"})
	w := e.worker(queue.WorkerOptions{})
	e.start(w)

	info := e.waitState(retried, queue.StateSucceeded)
	if info.Job.Attempt != 2 {
		t.Fatalf("job survived the panic on attempt %d, want 2", info.Job.Attempt)
	}
	info = e.waitState(dead, queue.StateDead)
	if !bytes.Contains([]byte(info.LastError), []byte("panicked")) {
		t.Fatalf("error text %q does not record the panic", info.LastError)
	}
	if bytes.Contains([]byte(info.LastError), []byte("panic-secret-123")) {
		t.Fatalf("panic value leaked into the stored error: %q", info.LastError)
	}
}

func handlerTimeout(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	var calls atomic.Int32
	e.register("hang", func(ctx context.Context, _ Message) error {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}, queue.HandlerTimeout(150*time.Millisecond))
	id := e.enqueue("hang", Message{ID: 1})
	e.start(e.worker(queue.WorkerOptions{}))
	info := e.waitState(id, queue.StateSucceeded)
	if info.Job.Attempt != 2 || !bytes.Contains([]byte(info.LastError), []byte("timeout")) {
		t.Fatalf("timed-out job = attempt %d, last error %q; want a retry that recorded the timeout", info.Job.Attempt, info.LastError)
	}
}

func shutdownWaits(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	started := make(chan struct{})
	e.register("slow", func(context.Context, Message) error {
		close(started)
		time.Sleep(400 * time.Millisecond)
		return nil
	})
	first := e.enqueue("slow", Message{ID: 1})
	w := e.worker(queue.WorkerOptions{})
	e.start(w)
	<-started

	begun := time.Now()
	if err := shutdown(t, w, 10*time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if time.Since(begun) < 300*time.Millisecond {
		t.Fatalf("Shutdown returned after %s, before the in-flight job finished", time.Since(begun))
	}
	if info := e.info(first); info.State != queue.StateSucceeded {
		t.Fatalf("in-flight job state after Shutdown = %s", info.State)
	}
	if w.InFlight() != 0 {
		t.Fatalf("%d jobs in flight after Shutdown", w.InFlight())
	}
	// A stopped worker claims nothing, and Shutdown is idempotent.
	late := e.enqueue("slow", Message{ID: 2})
	time.Sleep(150 * time.Millisecond)
	if info := e.info(late); info.State != queue.StatePending || info.Job.Attempt != 0 {
		t.Fatalf("stopped worker touched a new job: %+v", info)
	}
	if err := shutdown(t, w, time.Second); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if err := w.Start(context.Background()); !errors.Is(err, queue.ErrStarted) {
		t.Fatalf("Start after Shutdown = %v, want ErrStarted", err)
	}
}

func shutdownReleases(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	started := make(chan struct{})
	var calls atomic.Int32
	e.register("long", func(ctx context.Context, _ Message) error {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	})
	id := e.enqueue("long", Message{ID: 1})
	var outcomesMu sync.Mutex
	var outcomes []queue.Outcome
	w := e.worker(queue.WorkerOptions{Hooks: queue.Hooks{Finished: func(r queue.Result) {
		outcomesMu.Lock()
		outcomes = append(outcomes, r.Outcome)
		outcomesMu.Unlock()
	}}})
	e.start(w)
	<-started

	err := shutdown(t, w, 150*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("forced Shutdown = %v, want an error wrapping the deadline", err)
	}
	// Released at once, without consuming the attempt.
	info := e.info(id)
	if info.State != queue.StatePending || info.Job.Attempt != 0 {
		t.Fatalf("after forced Shutdown: state=%s attempt=%d; want pending with the attempt returned", info.State, info.Job.Attempt)
	}
	outcomesMu.Lock()
	if len(outcomes) != 1 || outcomes[0] != queue.OutcomeReleased {
		t.Fatalf("outcomes = %v, want one released", outcomes)
	}
	outcomesMu.Unlock()

	// Another worker takes it over immediately, as attempt 1.
	e.start(e.worker(queue.WorkerOptions{}))
	done := e.waitState(id, queue.StateSucceeded)
	if done.Job.Attempt != 1 {
		t.Fatalf("takeover ran as attempt %d, want 1", done.Job.Attempt)
	}
}

func startContextCancel(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	started := make(chan struct{})
	e.register("drain", func(context.Context, Message) error {
		close(started)
		time.Sleep(300 * time.Millisecond)
		return nil
	})
	first := e.enqueue("drain", Message{ID: 1})
	w := e.worker(queue.WorkerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel() // SIGTERM: stop claiming, but do not interrupt the running job
	second := e.enqueue("drain", Message{ID: 2})
	if err := shutdown(t, w, 10*time.Second); err != nil {
		t.Fatalf("Shutdown after context cancel: %v", err)
	}
	if info := e.info(first); info.State != queue.StateSucceeded {
		t.Fatalf("running job was interrupted: %+v", info)
	}
	if info := e.info(second); info.State != queue.StatePending {
		t.Fatalf("worker claimed a job after its context ended: %+v", info)
	}
}

func queueConcurrency(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	type gauge struct{ now, max atomic.Int32 }
	gauges := map[string]*gauge{"alpha": {}, "beta": {}}
	handler := func(name string, bound int32) func(context.Context, Message) error {
		g := gauges[name]
		return func(context.Context, Message) error {
			n := g.now.Add(1)
			for {
				seen := g.max.Load()
				if n <= seen || g.max.CompareAndSwap(seen, n) {
					break
				}
			}
			// Hold the slot until the queue is saturated, so the peak reaches
			// the bound however slow the machine or the database is.
			for deadline := time.Now().Add(10 * time.Second); g.now.Load() < bound && time.Now().Before(deadline); {
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(20 * time.Millisecond)
			g.now.Add(-1)
			return nil
		}
	}
	e.register("on.alpha", handler("alpha", 3), queue.HandlerQueue("alpha"))
	e.register("on.beta", handler("beta", 1), queue.HandlerQueue("beta"))
	var ids []uuid.UUID
	for i := range 6 {
		ids = append(ids, e.enqueue("on.alpha", Message{ID: i}), e.enqueue("on.beta", Message{ID: i}))
	}
	e.start(e.worker(queue.WorkerOptions{Queues: []queue.QueueConfig{{Name: "alpha", Concurrency: 3}, {Name: "beta", Concurrency: 1}}}))
	for _, id := range ids {
		e.waitState(id, queue.StateSucceeded)
	}
	if got := gauges["alpha"].max.Load(); got != 3 {
		t.Errorf("alpha peak concurrency = %d, want exactly its bound of 3", got)
	}
	if got := gauges["beta"].max.Load(); got != 1 {
		t.Errorf("beta peak concurrency = %d, want exactly its bound of 1", got)
	}
}

func unknownKinds(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	e.register("known", func(context.Context, Message) error { return nil })
	stranger := e.insertRaw("from.newer.deploy", `{"id":1}`, 3)
	known := e.enqueue("known", Message{ID: 1})
	e.start(e.worker(queue.WorkerOptions{}))
	e.waitState(known, queue.StateSucceeded)
	time.Sleep(100 * time.Millisecond)
	if info := e.info(stranger); info.State != queue.StatePending || info.Job.Attempt != 0 {
		t.Fatalf("a worker without the handler touched the job: %+v", info)
	}
}

func statsAndPrune(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	ctx := context.Background()
	e.register("ok", func(context.Context, Message) error { return nil })
	e.register("bad", func(context.Context, Message) error { return queue.Permanent(errors.New("no")) })
	e.register("later", func(context.Context, Message) error { return nil })
	e.enqueue("ok", Message{ID: 1})
	deadID := e.enqueue("bad", Message{ID: 2})
	e.enqueue("later", Message{ID: 3}, queue.RunAfter(time.Hour))
	w := e.worker(queue.WorkerOptions{})
	e.start(w)
	e.waitState(deadID, queue.StateDead)
	waitFor(t, 10*time.Second, "succeeded job", func() bool {
		s, err := e.store.Stats(ctx)
		return err == nil && len(s.Queues) == 1 && s.Queues[0].Succeeded == 1
	})
	stats, err := e.store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	q := stats.Queues[0]
	if q.Queue != queue.DefaultQueue || q.Succeeded != 1 || q.Dead != 1 || q.Scheduled != 1 || q.Ready != 0 || q.Running != 0 || q.OldestReadyAt != nil {
		t.Fatalf("stats = %+v", q)
	}
	// Ready jobs of an unstarted kind show their wait.
	e.insertRaw("nobody.handles.this", `{}`, 1)
	stats, _ = e.store.Stats(ctx)
	if q := stats.Queues[0]; q.Ready != 1 || q.OldestReadyAt == nil {
		t.Fatalf("stats with a waiting job = %+v", q)
	}

	// Prune removes only succeeded jobs older than the retention.
	if n, err := e.store.Prune(ctx, time.Hour, 100); err != nil || n != 0 {
		t.Fatalf("Prune(1h) = %d, %v; want nothing yet", n, err)
	}
	time.Sleep(20 * time.Millisecond)
	if n, err := e.store.Prune(ctx, 10*time.Millisecond, 100); err != nil || n != 1 {
		t.Fatalf("Prune(10ms) = %d, %v; want the one succeeded job", n, err)
	}
	stats, _ = e.store.Stats(ctx)
	if q := stats.Queues[0]; q.Succeeded != 0 || q.Dead != 1 || q.Scheduled != 1 {
		t.Fatalf("stats after prune = %+v; only succeeded jobs may be pruned", q)
	}
}

func payloadPrivacy(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	const secret = "4111-1111-1111-1111"
	e.register("charge", func(_ context.Context, m Message) error {
		return fmt.Errorf("gateway said: card %s declined; url=postgres://admin:hunter2@db.internal/x password=hunter2", m.Secret)
	}, queue.HandlerMaxAttempts(2))
	id := e.enqueue("charge", Message{ID: 1, Secret: secret, Note: "ordinary note"})
	e.start(e.worker(queue.WorkerOptions{}))
	info := e.waitState(id, queue.StateDead)
	for _, forbidden := range []string{secret, "hunter2"} {
		if bytes.Contains([]byte(info.LastError), []byte(forbidden)) {
			t.Errorf("stored error leaks %q: %q", forbidden, info.LastError)
		}
		if bytes.Contains([]byte(e.logs.String()), []byte(forbidden)) {
			t.Errorf("log output leaks %q", forbidden)
		}
	}
	if bytes.Contains([]byte(e.logs.String()), []byte("ordinary note")) {
		t.Error("log output contains payload text")
	}
	if len(info.LastError) > queue.MaxErrorBytes {
		t.Errorf("stored error has %d bytes, limit %d", len(info.LastError), queue.MaxErrorBytes)
	}
}

func releaseKeepsAttempts(t *testing.T, newStore NewStore) {
	store := newStore(t)
	ctx := context.Background()
	id := insertK(t, store, 3)
	claimed := claimOne(t, store, "worker-a", time.Minute)
	if info, _ := store.Get(ctx, id); info.State != queue.StateRunning || info.LeaseUntil == nil || info.WorkerID != "worker-a" {
		t.Fatalf("running info = %+v", info)
	}
	if err := store.Release(ctx, claimed.Lease); err != nil {
		t.Fatal(err)
	}
	info, _ := store.Get(ctx, id)
	if info.State != queue.StatePending || info.Job.Attempt != 0 || info.LeaseUntil != nil || info.WorkerID != "" {
		t.Fatalf("after Release: %+v", info)
	}
	again := claimOne(t, store, "worker-b", time.Minute)
	if again.Job.Attempt != 1 {
		t.Fatalf("attempt after release = %d, want 1", again.Job.Attempt)
	}
}

func recoverRequeues(t *testing.T, newStore NewStore) {
	store := newStore(t)
	ctx := context.Background()
	expiring := insertK(t, store, 3)
	exhausted := insertK(t, store, 1)
	live := insertK(t, store, 3)
	first := claimOne(t, store, "a", 1500*time.Millisecond)
	second := claimOne(t, store, "a", 1500*time.Millisecond)
	third := claimOne(t, store, "b", time.Hour)
	time.Sleep(2500 * time.Millisecond)

	n, err := store.Recover(ctx)
	if err != nil || n != 2 {
		t.Fatalf("Recover = %d, %v; want the two expired jobs", n, err)
	}
	states := map[uuid.UUID]queue.State{}
	for _, id := range []uuid.UUID{expiring, exhausted, live} {
		states[id] = e2eState(t, store, id)
	}
	for _, id := range []uuid.UUID{first.Job.ID, second.Job.ID} {
		// Which of the two had one attempt left depends on insertion order.
		if states[id] != queue.StatePending && states[id] != queue.StateDead {
			t.Errorf("expired job %s is %s", id, states[id])
		}
	}
	if states[third.Job.ID] != queue.StateRunning {
		t.Errorf("live job is %s, want running", states[third.Job.ID])
	}
	if n, err := store.Recover(ctx); err != nil || n != 0 {
		t.Fatalf("second Recover = %d, %v; want nothing left", n, err)
	}
}

func e2eState(t *testing.T, store queue.Store, id uuid.UUID) queue.State {
	t.Helper()
	info, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return info.State
}

// A worker whose slots are all busy never claims, so only its own recovery
// loop can requeue the jobs of a crashed peer.
func saturatedWorkerRecovers(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	release := make(chan struct{})
	started := make(chan struct{})
	e.register("busy", func(ctx context.Context, _ Message) error {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	e.register("orphan", func(context.Context, Message) error { return nil })
	t.Cleanup(func() { close(release) })
	e.enqueue("busy", Message{ID: 1}, queue.WithQueue("busyq"))
	e.start(e.worker(queue.WorkerOptions{
		Lease:  400 * time.Millisecond,
		Queues: []queue.QueueConfig{{Name: "busyq", Concurrency: 1}},
	}))
	<-started

	// A job claimed by a worker that then crashed (never renewed).
	orphan := e.enqueue("orphan", Message{ID: 2}, queue.WithQueue("elsewhere"))
	claimed, ok, err := e.store.Claim(context.Background(), queue.ClaimRequest{Queue: "elsewhere", Kinds: []string{"orphan"}, WorkerID: "crashed", Lease: 150 * time.Millisecond})
	if err != nil || !ok || claimed.Job.ID != orphan {
		t.Fatalf("setup claim: %v %v", ok, err)
	}
	e.waitState(orphan, queue.StatePending)
}

// Forced shutdown with a handler that ignores cancellation: the job must not
// be handed to another worker while that handler still runs.
func forcedShutdownNoOverlap(t *testing.T, newStore NewStore) {
	e := newEnv(t, newStore)
	var active, overlap, runs atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	e.register("stubborn", func(_ context.Context, _ Message) error {
		if n := active.Add(1); n > 1 {
			overlap.Add(1)
		}
		defer active.Add(-1)
		if runs.Add(1) == 1 {
			close(started)
			<-release // ignores cancellation
		}
		return nil
	})
	id := e.enqueue("stubborn", Message{ID: 1})
	first := e.worker(queue.WorkerOptions{Lease: 1500 * time.Millisecond, ReleaseTimeout: 100 * time.Millisecond})
	e.start(first)
	<-started

	if err := shutdown(t, first, 100*time.Millisecond); err == nil {
		t.Fatal("forced Shutdown returned nil")
	}
	// Another worker comes up at once. The job must stay leased to the stuck
	// handler: no overlapping run while it executes.
	e.start(e.worker(queue.WorkerOptions{Lease: 1500 * time.Millisecond}))
	time.Sleep(600 * time.Millisecond)
	if info := e.info(id); info.State != queue.StateRunning {
		t.Fatalf("job is %s while its handler still runs; it must stay leased", info.State)
	}
	if overlap.Load() != 0 || runs.Load() != 1 {
		t.Fatalf("job ran %d time(s), %d overlapping, while the first handler was stuck", runs.Load(), overlap.Load())
	}
	close(release)
	e.waitState(id, queue.StateSucceeded)
}
