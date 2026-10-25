package queue

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// recordingHandler sends every log message to a channel, so a test waits for
// the line it expects instead of sleeping.
type recordingHandler struct {
	messages chan string
}

func newRecordingLogger() (*slog.Logger, chan string) {
	h := &recordingHandler{messages: make(chan string, 256)}
	return slog.New(h), h.messages
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	select {
	case h.messages <- record.Message:
	default:
	}
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func awaitMessage(t *testing.T, messages <-chan string, want string) {
	t.Helper()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case message := <-messages:
			if strings.Contains(message, want) {
				return
			}
		case <-timeout:
			t.Fatalf("no %q log line", want)
		}
	}
}

// maintenanceStore adds scriptable Recover and Prune to fakeStore.
type maintenanceStore struct {
	*fakeStore
	recover func() (int64, error)
	prune   func() (int64, error)
}

func (s *maintenanceStore) Recover(context.Context) (int64, error) {
	if s.recover == nil {
		return 0, nil
	}
	return s.recover()
}

func (s *maintenanceStore) Prune(context.Context, time.Duration, int) (int64, error) {
	if s.prune == nil {
		return 0, nil
	}
	return s.prune()
}

func TestNewWorkerRequiresItsDependenciesAndBoundedOptions(t *testing.T) {
	t.Parallel()
	reg := registerK(t, func(context.Context) error { return nil })
	if _, err := NewWorker(nil, reg, WorkerOptions{}); err == nil {
		t.Error("a nil store was accepted")
	}
	if _, err := NewWorker(&fakeStore{}, nil, WorkerOptions{}); err == nil {
		t.Error("a nil registry was accepted")
	}
	if _, err := NewWorker(&fakeStore{}, reg, WorkerOptions{RetainSucceeded: 11 * 365 * 24 * time.Hour}); err == nil || !strings.Contains(err.Error(), "ten years") {
		t.Errorf("retention: %v", err)
	}
	w, err := NewWorker(&fakeStore{}, reg, WorkerOptions{WorkerID: "worker-1"})
	if err != nil || w.WorkerID() != "worker-1" {
		t.Fatalf("worker=%v err=%v", w, err)
	}
	generated, err := NewWorker(&fakeStore{}, reg, WorkerOptions{})
	if err != nil || generated.WorkerID() == "" || len(generated.WorkerID()) > maxWorkerIDLength {
		t.Fatalf("generated ID %q err=%v", generated.WorkerID(), err)
	}
	var nilCtx context.Context
	if err := generated.Start(nilCtx); err == nil {
		t.Error("a nil context was accepted")
	}
	if err := (&Worker{}).Shutdown(context.Background()); err != nil {
		t.Errorf("shutdown of a worker that never started: %v", err)
	}
}

func TestWorkerKeepsPollingWhenClaimsFail(t *testing.T) {
	t.Parallel()
	logger, messages := newRecordingLogger()
	failures := make(chan struct{}, 8)
	claims := 0
	job := oneJob()
	store := &fakeStore{claim: func(r ClaimRequest) (Claimed, bool, error) {
		claims++
		if claims <= 2 {
			failures <- struct{}{}
			return Claimed{}, false, errors.New("database unavailable")
		}
		return job(r)
	}}
	finished := make(chan Result, 1)
	reg := registerK(t, func(context.Context) error { return nil })
	w := newTestWorker(t, store, reg, WorkerOptions{Logger: logger, Hooks: Hooks{Finished: func(r Result) { finished <- r }}})
	defer shutdownQuietly(w)
	awaitMessage(t, messages, "claim jobs failed")
	select {
	case result := <-finished:
		if result.Outcome != OutcomeSucceeded {
			t.Errorf("outcome = %s", result.Outcome)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the worker never recovered from the claim failures")
	}
}

func TestWorkerReportsMaintenanceFailuresAndKeepsRunning(t *testing.T) {
	t.Parallel()
	logger, messages := newRecordingLogger()
	store := &maintenanceStore{
		fakeStore: &fakeStore{},
		recover:   func() (int64, error) { return 0, errors.New("recover failed") },
		prune:     func() (int64, error) { return 0, errors.New("prune failed") },
	}
	reg := registerK(t, func(context.Context) error { return nil })
	opts := WorkerOptions{Logger: logger, Lease: minLease, MaintenanceInterval: time.Millisecond, RetainSucceeded: time.Hour}
	opts.PollInterval, opts.MaxPollInterval = 5*time.Millisecond, 10*time.Millisecond
	w, err := NewWorker(store, reg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer shutdownQuietly(w)
	awaitMessage(t, messages, "recover expired job leases failed")
	awaitMessage(t, messages, "prune finished jobs failed")
}

func TestWorkerRecoversAndPrunesFullBatchesInRounds(t *testing.T) {
	t.Parallel()
	logger, messages := newRecordingLogger()
	rounds := make(chan struct{}, 8)
	store := &maintenanceStore{
		fakeStore: &fakeStore{},
		recover: func() (int64, error) {
			select {
			case rounds <- struct{}{}:
			default:
			}
			return recoverBatch, nil // a full batch asks for another round
		},
	}
	reg := registerK(t, func(context.Context) error { return nil })
	opts := WorkerOptions{Logger: logger, Lease: minLease, RetainSucceeded: -1}
	opts.PollInterval, opts.MaxPollInterval = 5*time.Millisecond, 10*time.Millisecond
	w, err := NewWorker(store, reg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer shutdownQuietly(w)
	awaitMessage(t, messages, "recovered jobs whose lease expired")
	<-rounds
	<-rounds // more than one round in one sweep
}

func TestWorkerKillsJobsOfUnregisteredKindsAndSurvivesHookPanics(t *testing.T) {
	t.Parallel()
	logger, messages := newRecordingLogger()
	failed := make(chan Failure, 1)
	id := oneJob()
	store := &fakeStore{
		claim: func(r ClaimRequest) (Claimed, bool, error) {
			claimed, ok, err := id(r)
			claimed.Job.Kind = "ghost" // a kind this worker has no handler for
			return claimed, ok, err
		},
		fail: func(_ context.Context, _ Lease, f Failure) error { failed <- f; return nil },
	}
	reg := registerK(t, func(context.Context) error { return nil })
	w := newTestWorker(t, store, reg, WorkerOptions{
		Logger: logger,
		Hooks:  Hooks{Finished: func(Result) { panic("hook bug") }},
	})
	defer shutdownQuietly(w)
	select {
	case failure := <-failed:
		if !failure.Dead || !strings.Contains(failure.Message, `"ghost"`) {
			t.Errorf("failure = %+v, want a dead job naming the unknown kind", failure)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the unknown kind was never failed")
	}
	awaitMessage(t, messages, "job hook panicked")
}

func TestWorkerSurvivesAStartedHookPanicAndRunsTheJob(t *testing.T) {
	t.Parallel()
	logger, messages := newRecordingLogger()
	ran := make(chan struct{}, 1)
	reg := registerK(t, func(context.Context) error { ran <- struct{}{}; return nil })
	w := newTestWorker(t, &fakeStore{claim: oneJob()}, reg, WorkerOptions{
		Logger: logger,
		Hooks:  Hooks{Started: func(Job) { panic("started hook bug") }},
	})
	defer shutdownQuietly(w)
	awaitMessage(t, messages, "job hook panicked")
	select {
	case <-ran:
	case <-time.After(20 * time.Second):
		t.Fatal("a panicking hook stopped the job from running")
	}
}

func TestWorkerLeavesAJobLeasedWhenItsOutcomeCannotBeStored(t *testing.T) {
	t.Parallel()
	logger, messages := newRecordingLogger()
	attempts := make(chan struct{}, 64)
	store := &fakeStore{claim: oneJob(), complete: func(context.Context, Lease) error {
		select {
		case attempts <- struct{}{}:
		default:
		}
		return errors.New("database unavailable")
	}}
	reg := registerK(t, func(context.Context) error { return nil })
	finished := make(chan Result, 1)
	w := newTestWorker(t, store, reg, WorkerOptions{Logger: logger, Lease: 300 * time.Millisecond, Hooks: Hooks{Finished: func(r Result) { finished <- r }}})
	defer shutdownQuietly(w)
	awaitMessage(t, messages, "store job outcome failed")
	if len(attempts) < 2 {
		t.Errorf("the outcome was written %d time(s); a transient failure must be retried", len(attempts))
	}
	select {
	case result := <-finished:
		t.Errorf("a job whose outcome was never stored was reported finished: %+v", result)
	default:
	}
}

func TestWorkerDiscardsTheResultOfAJobWhoseLeaseWasLost(t *testing.T) {
	t.Parallel()
	logger, messages := newRecordingLogger()
	started := make(chan struct{})
	reg := registerK(t, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	store := &fakeStore{claim: oneJob(), renew: func(context.Context, Lease) error { return ErrLeaseLost }}
	finished := make(chan Result, 1)
	w := newTestWorker(t, store, reg, WorkerOptions{Logger: logger, Lease: 30 * time.Millisecond, Hooks: Hooks{Finished: func(r Result) { finished <- r }}})
	defer shutdownQuietly(w)
	<-started
	awaitMessage(t, messages, "job lease lost; result discarded")
	select {
	case result := <-finished:
		if result.Outcome != OutcomeLeaseLost {
			t.Errorf("outcome = %s", result.Outcome)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no result for the lost lease")
	}
	if store.completes.Load() != 0 {
		t.Error("the result of a job without a lease was written")
	}
}

func TestWorkerCancelsAHandlerWhoseLeaseCannotBeRenewedInTime(t *testing.T) {
	t.Parallel()
	logger, messages := newRecordingLogger()
	canceled := make(chan error, 1)
	reg := registerK(t, func(ctx context.Context) error {
		<-ctx.Done()
		canceled <- context.Cause(ctx)
		return ctx.Err()
	})
	// Renewals fail with an infrastructure error, so the lease runs out.
	store := &fakeStore{claim: oneJob(), renew: func(context.Context, Lease) error { return errors.New("database unavailable") }}
	w := newTestWorker(t, store, reg, WorkerOptions{Logger: logger, Lease: 60 * time.Millisecond})
	defer shutdownQuietly(w)
	awaitMessage(t, messages, "renew job lease failed")
	select {
	case cause := <-canceled:
		if !errors.Is(cause, ErrLeaseLost) {
			t.Errorf("cause = %v, want ErrLeaseLost", cause)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the handler kept running past its lease")
	}
}
