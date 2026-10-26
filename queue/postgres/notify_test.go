package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/internal/waittest"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/queue"
	queuepostgres "github.com/rocketinsidecorp/forge-go/queue/postgres"
	"github.com/rocketinsidecorp/forge-go/queue/storetest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func newNotifyStore(t testing.TB, db *postgres.DB) *queuepostgres.Store {
	t.Helper()
	store, err := queuepostgres.NewStore(db, queuepostgres.WithNotify())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// The whole contract holds with announcements and listening workers too.
func TestNotifyStoreConformance(t *testing.T) {
	storetest.Run(t, func(t testing.TB) queue.Store { return newNotifyStore(t, newDB(t)) })
}

func TestNewStoreRefusesANilOption(t *testing.T) {
	if _, err := queuepostgres.NewStoreFromPool(newDB(t).Pool(), nil); err == nil {
		t.Fatal("a nil option was accepted")
	}
}

// listenConn subscribes a dedicated connection to the queue's channel.
func listenConn(t *testing.T, db *postgres.DB) *pgx.Conn {
	t.Helper()
	conn := db.Pool().Config().ConnConfig.Copy()
	listener, err := pgx.ConnectConfig(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close(context.Background()) })
	if _, err := listener.Exec(context.Background(), "LISTEN "+queuepostgres.NotifyChannel); err != nil {
		t.Fatal(err)
	}
	return listener
}

// nextNotification returns the next announcement's payload, or "" when none
// arrives within wait.
func nextNotification(t *testing.T, conn *pgx.Conn, wait time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	notification, err := conn.WaitForNotification(ctx)
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return notification.Payload
}

func TestEnqueueAnnouncesOnlyDueCommittedJobs(t *testing.T) {
	db := newDB(t)
	store := newNotifyStore(t, db)
	listener := listenConn(t, db)
	ctx := context.Background()
	job := func(queueName, key string, runAt time.Time) queue.NewJob {
		return queue.NewJob{ID: uuid.MustNew(), Kind: "k", Queue: queueName, Payload: json.RawMessage(`{}`), MaxAttempts: 3, Key: key, RunAt: runAt}
	}

	if _, err := store.Insert(ctx, job("mail", "once", time.Time{})); err != nil {
		t.Fatal(err)
	}
	if got := nextNotification(t, listener, 2*time.Second); got != "mail" {
		t.Fatalf("announcement of a due job = %q, want mail", got)
	}
	// A duplicate stores nothing and announces nothing; neither does a job
	// due later (polling finds it when it is due).
	if result, err := store.Insert(ctx, job("mail", "once", time.Time{})); err != nil || !result.Duplicate {
		t.Fatalf("duplicate insert = %+v, %v", result, err)
	}
	if _, err := store.Insert(ctx, job("mail", "", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	// A rolled-back transaction announces nothing; a committed one announces
	// at commit, once per queue however many jobs it stored.
	rollback := errors.New("roll back")
	if err := db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := store.In(tx).Insert(ctx, job("reports", "", time.Time{})); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if got := nextNotification(t, listener, 300*time.Millisecond); got != "" {
		t.Fatalf("unexpected announcement %q", got)
	}
	if err := db.InTx(ctx, func(tx pgx.Tx) error {
		for range 3 {
			if _, err := store.In(tx).Insert(ctx, job("reports", "", time.Time{})); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := nextNotification(t, listener, 2*time.Second); got != "reports" {
		t.Fatalf("announcement at commit = %q, want reports", got)
	}
	if got := nextNotification(t, listener, 300*time.Millisecond); got != "" {
		t.Fatalf("identical announcements of one transaction were not merged: %q", got)
	}

	// Without WithNotify nothing is announced and Listen is unsupported.
	plain, err := queuepostgres.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Insert(ctx, job("mail", "", time.Time{})); err != nil {
		t.Fatal(err)
	}
	if got := nextNotification(t, listener, 300*time.Millisecond); got != "" {
		t.Fatalf("a store without WithNotify announced %q", got)
	}
	if err := plain.Listen(ctx, func() {}, func(string) {}); !errors.Is(err, queue.ErrListenUnsupported) {
		t.Fatalf("Listen without WithNotify = %v, want ErrListenUnsupported", err)
	}
}

// slowPollingWorker starts a worker whose polls are far apart, so only an
// announcement can make it claim quickly.
func slowPollingWorker(t *testing.T, store queue.Store, registry *queue.Registry, poll time.Duration) *queue.Worker {
	t.Helper()
	worker, err := queue.NewWorker(store, registry, queue.WorkerOptions{PollInterval: poll, MaxPollInterval: poll, RetainSucceeded: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = worker.Shutdown(ctx)
	})
	return worker
}

func startedRegistry(t *testing.T) (*queue.Registry, chan time.Time) {
	t.Helper()
	started := make(chan time.Time, 16)
	registry := queue.NewRegistry()
	if err := queue.Register(registry, "k", func(context.Context, struct{}) error { started <- time.Now(); return nil }); err != nil {
		t.Fatal(err)
	}
	return registry, started
}

func waitListening(t *testing.T, worker *queue.Worker, want bool) {
	t.Helper()
	if !waittest.Poll(10*time.Second, func() bool { return worker.Listening() == want }) {
		t.Fatalf("worker Listening() never became %v", want)
	}
}

func enqueueK(t *testing.T, store queue.Inserter, registry *queue.Registry) time.Time {
	t.Helper()
	enqueued := time.Now()
	if _, err := queue.Enqueue(context.Background(), store, registry, "k", struct{}{}); err != nil {
		t.Fatal(err)
	}
	return enqueued
}

func TestNotifyWakesAnIdleWorkerWellUnderItsPollInterval(t *testing.T) {
	db := newDB(t)
	store := newNotifyStore(t, db)
	registry, started := startedRegistry(t)
	worker := slowPollingWorker(t, store, registry, time.Minute)
	waitListening(t, worker, true)
	time.Sleep(200 * time.Millisecond) // real-clock: the catch-up claim after subscribing runs on the worker's real timers

	for range 3 {
		enqueued := enqueueK(t, store, registry)
		select {
		case at := <-started:
			if latency := at.Sub(enqueued); latency > 2*time.Second {
				t.Fatalf("announced job started after %s", latency)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("an announced job did not wake the idle worker (its poll interval is a minute)")
		}
		time.Sleep(50 * time.Millisecond) // real-clock: lets the worker return to its idle wait between announcements
	}
}

// listenerPID finds the backend of the worker's LISTEN connection.
func listenerPID(t *testing.T, db *postgres.DB) int {
	t.Helper()
	var pid int
	err := db.QueryRow(context.Background(), `
		SELECT pid FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid() AND query LIKE 'LISTEN %'`).Scan(&pid)
	if err != nil {
		t.Fatalf("find the listener backend: %v", err)
	}
	return pid
}

func TestListenerLossFallsBackToPollingAndReconnects(t *testing.T) {
	db := newDB(t)
	store := newNotifyStore(t, db)
	registry, started := startedRegistry(t)
	worker := slowPollingWorker(t, store, registry, 300*time.Millisecond)
	waitListening(t, worker, true)

	var terminated bool
	if err := db.QueryRow(context.Background(), "SELECT pg_terminate_backend($1)", listenerPID(t, db)).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate the listener: %v, %v", terminated, err)
	}
	waitListening(t, worker, false)
	// No subscription now: polling alone runs the job.
	enqueueK(t, store, registry)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("a job enqueued while the listener was down never ran")
	}
	// The listener comes back on its own (backoff of about a second).
	waitListening(t, worker, true)
	enqueueK(t, store, registry)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the job after reconnecting never ran")
	}
}

func TestShutdownWithAnActiveListenerIsPromptAndClosesIt(t *testing.T) {
	db := newDB(t)
	store := newNotifyStore(t, db)
	registry, _ := startedRegistry(t)
	worker, err := queue.NewWorker(store, registry, queue.WorkerOptions{RetainSucceeded: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitListening(t, worker, true)
	listenerPID(t, db)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	began := time.Now()
	if err := worker.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > time.Second {
		t.Fatalf("Shutdown took %s with the listener active", took)
	}
	if worker.Listening() {
		t.Fatal("still listening after Shutdown")
	}
	var listeners int
	if !waittest.Poll(5*time.Second, func() bool {
		if err := db.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND query LIKE 'LISTEN %'`).Scan(&listeners); err != nil {
			t.Fatal(err)
		}
		return listeners == 0
	}) {
		t.Fatalf("%d listening connection(s) left after Shutdown", listeners)
	}
}
