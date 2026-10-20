package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

func knownRecoveryStore() *recoveryStoreStub {
	return &recoveryStoreStub{
		found:    true,
		identity: RecoveryIdentity{SubjectID: uuid.MustNew(), Email: "alice@example.com"},
	}
}

type dispatcherStub struct {
	mu   sync.Mutex
	jobs []RecoveryJob
	err  error
}

func (d *dispatcherStub) DispatchRecovery(_ context.Context, job RecoveryJob) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.jobs = append(d.jobs, job)
	return d.err
}

func TestRecoveryRequestNeverAnswersFasterThanTheResponseTime(t *testing.T) {
	limiter, _ := newTestThrottler(t, LoginThrottleConfig{Window: time.Minute, PairLimit: 1, AccountLimit: 5, SourceLimit: 50, MaxEntries: 100})
	service, err := NewRecoveryService(&recoveryStoreStub{}, &recoverySenderStub{}, WithRecoveryThrottler(limiter), WithRecoveryDispatcher(&dispatcherStub{}))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]RecoveryRequest{
		"known or unknown":   {Email: "alice@example.com", TenantSlug: "acme", Source: "192.0.2.1"},
		"throttled":          {Email: "alice@example.com", TenantSlug: "acme", Source: "192.0.2.1"},
		"malformed login":    {Email: "not-an-email", TenantSlug: "acme", Source: "192.0.2.2"},
		"non-ASCII login":    {Email: "alice@\u0130xample.com", TenantSlug: "acme", Source: "192.0.2.3"},
		"malformed tenant":   {Email: "alice@example.com", TenantSlug: "../x", Source: "192.0.2.4"},
		"empty everything":   {Source: "192.0.2.5"},
		"throttled (second)": {Email: "alice@example.com", TenantSlug: "acme", Source: "192.0.2.1"},
	}
	for name, request := range cases {
		started := time.Now()
		_ = service.Request(context.Background(), request)
		if elapsed := time.Since(started); elapsed < DefaultRecoveryResponseTime {
			t.Errorf("%s answered in %v, before the %v floor", name, elapsed, DefaultRecoveryResponseTime)
		}
	}
}

func TestRecoveryRequestStopsWaitingWhenTheClientIsGone(t *testing.T) {
	service, err := NewRecoveryService(&recoveryStoreStub{}, &recoverySenderStub{}, WithRecoveryResponseTime(5*time.Second), WithRecoveryDispatcher(&dispatcherStub{}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_ = service.Request(ctx, RecoveryRequest{Email: "a@example.com", TenantSlug: "acme"})
	if time.Since(started) > time.Second {
		t.Fatal("a cancelled request kept waiting for the response time")
	}
}

func TestRecoveryResponseTimeIsValidated(t *testing.T) {
	for _, bad := range []time.Duration{-1, 6 * time.Second} {
		if _, err := NewRecoveryService(&recoveryStoreStub{}, &recoverySenderStub{}, WithRecoveryResponseTime(bad)); err == nil {
			t.Errorf("response time %v accepted", bad)
		}
	}
}

func TestRecoveryRequestWithADispatcherDoesNoAccountWork(t *testing.T) {
	store := knownRecoveryStore()
	sender := &recoverySenderStub{}
	dispatcher := &dispatcherStub{}
	service, err := NewRecoveryService(store, sender, WithRecoveryResponseTime(0), WithRecoveryDispatcher(dispatcher))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Request(context.Background(), RecoveryRequest{Email: " Alice@Example.com ", TenantSlug: "ACME", Source: "192.0.2.1"}); err != nil {
		t.Fatal(err)
	}
	// Nothing that depends on the account ran on the request path.
	if store.lookupEmail != "" || store.createdDigest != (Digest{}) || sender.calls != 0 {
		t.Fatalf("account work ran inline: lookup=%q created=%x sends=%d", store.lookupEmail, store.createdDigest, sender.calls)
	}
	if len(dispatcher.jobs) != 1 || dispatcher.jobs[0].Email != "alice@example.com" || dispatcher.jobs[0].TenantSlug != "acme" || dispatcher.jobs[0].Key.Account == ([32]byte{}) {
		t.Fatalf("jobs=%+v", dispatcher.jobs)
	}

	// The same job, processed later, does the work.
	if err := service.Process(context.Background(), dispatcher.jobs[0]); err != nil {
		t.Fatal(err)
	}
	if store.lookupEmail != "alice@example.com" || sender.calls != 1 || sender.message.Email != "alice@example.com" {
		t.Fatalf("process: lookup=%q sends=%d to=%q", store.lookupEmail, sender.calls, sender.message.Email)
	}
}

func TestRecoveryRequestDispatchesOnlyWellFormedIdentities(t *testing.T) {
	dispatcher := &dispatcherStub{}
	service, err := NewRecoveryService(&recoveryStoreStub{}, &recoverySenderStub{}, WithRecoveryResponseTime(0), WithRecoveryDispatcher(dispatcher))
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []RecoveryRequest{
		{Email: "not-an-email", TenantSlug: "acme"},
		{Email: "user@\u0130cloud.com", TenantSlug: "acme"},
		{Email: "user@example.com", TenantSlug: "../global"},
		{Email: "user@example.com", TenantSlug: "AC\u212aME"},
	} {
		if err := service.Request(context.Background(), request); err != nil {
			t.Fatalf("%+v: %v", request, err)
		}
	}
	if len(dispatcher.jobs) != 0 {
		t.Fatalf("jobs for identities that name no account: %+v", dispatcher.jobs)
	}
}

func TestRecoveryRequestDropsAndLogsWhenTheDispatcherIsFull(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	for _, dropped := range []error{ErrRecoveryQueueFull, ErrRecoveryQueueClosed} {
		logs.Reset()
		service, err := NewRecoveryService(&recoveryStoreStub{}, &recoverySenderStub{},
			WithRecoveryResponseTime(0), WithRecoveryLogger(logger), WithRecoveryDispatcher(&dispatcherStub{err: dropped}))
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Request(context.Background(), RecoveryRequest{Email: "victim@example.com", TenantSlug: "acme", Source: "192.0.2.77"}); err != nil {
			t.Fatalf("%v: a dropped request must still answer like any other: %v", dropped, err)
		}
		if !strings.Contains(logs.String(), "password recovery request dropped") {
			t.Fatalf("%v: drop not logged: %q", dropped, logs.String())
		}
		for _, secret := range []string{"victim", "example.com", "acme", "192.0.2.77"} {
			if strings.Contains(logs.String(), secret) {
				t.Fatalf("log leaks %q: %s", secret, logs.String())
			}
		}
	}

	failing := errors.New("database down")
	service, err := NewRecoveryService(&recoveryStoreStub{}, &recoverySenderStub{}, WithRecoveryResponseTime(0), WithRecoveryDispatcher(&dispatcherStub{err: failing}))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Request(context.Background(), RecoveryRequest{Email: "a@example.com", TenantSlug: "acme"}); !errors.Is(err, failing) {
		t.Fatalf("an enqueue failure must be reported: %v", err)
	}
}

// blockingSender holds every delivery until released.
type blockingSender struct {
	entered chan PasswordRecoveryMessage
	release chan struct{}
}

func (s *blockingSender) SendPasswordRecovery(ctx context.Context, message PasswordRecoveryMessage) error {
	s.entered <- message
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestRecoveryRequestAnswersWhileTheSenderIsStillBlocked(t *testing.T) {
	store := knownRecoveryStore()
	sender := &blockingSender{entered: make(chan PasswordRecoveryMessage, 1), release: make(chan struct{})}
	queue, err := NewRecoveryQueue(RecoveryQueueConfig{Capacity: 4, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewRecoveryService(store, sender, WithRecoveryResponseTime(0), WithRecoveryDispatcher(queue))
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Start(service); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })

	answered := make(chan error, 1)
	go func() {
		answered <- service.Request(context.Background(), RecoveryRequest{Email: "alice@example.com", TenantSlug: "acme", Source: "192.0.2.1"})
	}()
	select {
	case err := <-answered:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request waited for the account's delivery: inline work reveals whether the account exists")
	}
	select {
	case message := <-sender.entered:
		if message.Email != "alice@example.com" || message.Token.Reveal() == "" {
			t.Fatalf("message=%+v", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never delivered")
	}
	close(sender.release)
	if err := queue.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryQueueIsBoundedAndNeverBlocks(t *testing.T) {
	queue, err := NewRecoveryQueue(RecoveryQueueConfig{Capacity: 2, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := queue.DispatchRecovery(context.Background(), RecoveryJob{Email: "a@example.com", TenantSlug: "acme"}); err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
	}
	done := make(chan error, 1)
	go func() {
		done <- queue.DispatchRecovery(context.Background(), RecoveryJob{Email: "a@example.com", TenantSlug: "acme"})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrRecoveryQueueFull) {
			t.Fatalf("a full queue returned %v, want ErrRecoveryQueueFull", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a full queue made the request wait")
	}
}

type processorFunc func(context.Context, RecoveryJob) error

func (f processorFunc) Process(ctx context.Context, job RecoveryJob) error { return f(ctx, job) }

func TestRecoveryQueueShutdownDrainsQueuedWorkAndStopsAcceptingAny(t *testing.T) {
	queue, err := NewRecoveryQueue(RecoveryQueueConfig{Capacity: 10, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	var processed atomic.Int32
	for i := 0; i < 5; i++ {
		if err := queue.DispatchRecovery(context.Background(), RecoveryJob{Email: "a@example.com", TenantSlug: "acme"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := queue.Start(processorFunc(func(context.Context, RecoveryJob) error { processed.Add(1); return nil })); err != nil {
		t.Fatal(err)
	}
	if err := queue.Start(processorFunc(nil)); err == nil {
		t.Fatal("a second Start was accepted")
	}
	if err := queue.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if processed.Load() != 5 {
		t.Fatalf("processed=%d, want the 5 queued requests drained", processed.Load())
	}
	if err := queue.DispatchRecovery(context.Background(), RecoveryJob{}); !errors.Is(err, ErrRecoveryQueueClosed) {
		t.Fatalf("dispatch after shutdown: %v", err)
	}
	if err := queue.Shutdown(context.Background()); err != nil {
		t.Fatalf("a second Shutdown: %v", err)
	}
}

func TestRecoveryQueueShutdownCancelsWorkThatOutlivesTheDeadlineAndLeavesNoWorkers(t *testing.T) {
	var logs bytes.Buffer
	queue, err := NewRecoveryQueue(RecoveryQueueConfig{Capacity: 10, Workers: 1, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	running := make(chan struct{})
	var cancelled atomic.Bool
	var later atomic.Int32
	err = queue.Start(processorFunc(func(ctx context.Context, job RecoveryJob) error {
		if job.TenantSlug == "later" {
			later.Add(1)
			return nil
		}
		close(running)
		<-ctx.Done()
		cancelled.Store(true)
		return ctx.Err()
	}))
	if err != nil {
		t.Fatal(err)
	}
	_ = queue.DispatchRecovery(context.Background(), RecoveryJob{Email: "a@example.com", TenantSlug: "acme"})
	<-running
	_ = queue.DispatchRecovery(context.Background(), RecoveryJob{Email: "a@example.com", TenantSlug: "later"})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = queue.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown=%v, want the deadline error", err)
	}
	// Shutdown returned only after the worker did: nothing outlives it.
	if !cancelled.Load() {
		t.Fatal("the running request was not cancelled")
	}
	if later.Load() != 0 || !strings.Contains(logs.String(), "abandoned") {
		t.Fatalf("a request queued behind the deadline ran (%d) or was not reported: %s", later.Load(), logs.String())
	}
}

func TestRecoveryQueueSurvivesAPanicAndFailuresWithoutLoggingTheirDetails(t *testing.T) {
	var logs bytes.Buffer
	queue, err := NewRecoveryQueue(RecoveryQueueConfig{Capacity: 10, Workers: 1, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	done := make(chan struct{})
	err = queue.Start(processorFunc(func(_ context.Context, job RecoveryJob) error {
		switch calls.Add(1) {
		case 1:
			panic("secret-token-in-panic")
		case 2:
			return &RecoveryDeliveryError{Cause: errors.New("550 mailbox victim@example.com unavailable")}
		}
		close(done)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := queue.DispatchRecovery(context.Background(), RecoveryJob{Email: "a@example.com", TenantSlug: "acme"}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not survive a panic")
	}
	if err := queue.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-token-in-panic", "victim@example.com"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log leaks %q: %s", secret, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "panicked") || !strings.Contains(logs.String(), "delivery failed") {
		t.Fatalf("failures not logged: %s", logs.String())
	}
}

func TestRecoveryQueueConfigIsValidated(t *testing.T) {
	for name, config := range map[string]RecoveryQueueConfig{
		"negative capacity": {Capacity: -1},
		"huge capacity":     {Capacity: 1 << 30},
		"negative workers":  {Workers: -1},
		"too many workers":  {Workers: 1000},
		"negative timeout":  {JobTimeout: -time.Second},
	} {
		if _, err := NewRecoveryQueue(config); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := (&RecoveryQueue{}).Start(nil); err == nil {
		t.Error("nil processor accepted")
	}
}

type tokenCheckerStore struct {
	recoveryStoreStub
	usable bool
	err    error
	checks int
}

func (s *tokenCheckerStore) RecoveryTokenUsable(context.Context, Digest) (bool, error) {
	s.checks++
	return s.usable, s.err
}

func TestRecoveryResetChecksTheTokenBeforeHashingThePassword(t *testing.T) {
	token, err := NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	store := &tokenCheckerStore{recoveryStoreStub: recoveryStoreStub{consumeResult: true}}
	auditor := &securityAuditorStub{}
	service, err := NewRecoveryService(store, &recoverySenderStub{}, WithRecoveryAuditor(auditor))
	if err != nil {
		t.Fatal(err)
	}
	// A password HashPassword refuses: if it ran before the token check the
	// answer would be ErrPasswordInvalid, not ErrRecoveryInvalid.
	tooLong := strings.Repeat("x", maxPasswordBytes+1)
	err = service.Reset(context.Background(), PasswordReset{Token: token.Reveal(), NewPassword: tooLong})
	if !errors.Is(err, ErrRecoveryInvalid) || errors.Is(err, ErrPasswordInvalid) {
		t.Fatalf("unknown token error=%v, want ErrRecoveryInvalid before any password work", err)
	}
	if store.consumedHash != "" || store.checks != 1 {
		t.Fatalf("consumed=%q checks=%d", store.consumedHash, store.checks)
	}
	if len(auditor.events) != 1 || auditor.events[0].Kind != SecurityPasswordReset || auditor.events[0].Outcome != SecurityOutcomeDenied {
		t.Fatalf("audit=%+v", auditor.events)
	}

	store.usable = true
	if err := service.Reset(context.Background(), PasswordReset{Token: token.Reveal(), NewPassword: "a perfectly fine password"}); err != nil {
		t.Fatalf("a usable token: %v", err)
	}
	if store.consumedHash == "" {
		t.Fatal("a usable token was not consumed")
	}

	store.err = errors.New("database down")
	if err := service.Reset(context.Background(), PasswordReset{Token: token.Reveal(), NewPassword: "a perfectly fine password"}); !errors.Is(err, store.err) {
		t.Fatalf("a failing check must fail closed: %v", err)
	}
}

func TestRecoveryRequestsFromOneSourceCannotFloodOneVictim(t *testing.T) {
	limiter, _ := newTestThrottler(t, LoginThrottleConfig{Window: time.Hour, PairLimit: 2, AccountLimit: 4, SourceLimit: 50, MaxEntries: 100})
	dispatcher := &dispatcherStub{}
	service, err := NewRecoveryService(&recoveryStoreStub{}, &recoverySenderStub{}, WithRecoveryResponseTime(0), WithRecoveryThrottler(limiter), WithRecoveryDispatcher(dispatcher))
	if err != nil {
		t.Fatal(err)
	}
	request := func(source string) error {
		return service.Request(context.Background(), RecoveryRequest{Email: "victim@example.com", TenantSlug: "acme", Source: source})
	}
	for i := 0; i < 2; i++ {
		if err := request("192.0.2.66"); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	var throttled *RecoveryThrottledError
	if err := request("192.0.2.66"); !errors.As(err, &throttled) {
		t.Fatalf("a third request from one source: %v", err)
	}
	// The victim, from elsewhere, can still ask for their own link.
	if err := request("192.0.2.1"); err != nil {
		t.Fatalf("the victim was locked out of recovery by one source: %v", err)
	}
	if len(dispatcher.jobs) != 3 {
		t.Fatalf("jobs=%d, want 3 (the throttled request queues nothing)", len(dispatcher.jobs))
	}
}
