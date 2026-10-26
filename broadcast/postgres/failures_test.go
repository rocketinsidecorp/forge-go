package postgres_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/broadcast"
	bcpostgres "github.com/rocketinsidecorp/forge-go/broadcast/postgres"
	"github.com/rocketinsidecorp/forge-go/internal/waittest"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

// recorder keeps log lines, with their error attribute, for a test to wait on.
type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *recorder) Handle(_ context.Context, record slog.Record) error {
	line := record.Message
	record.Attrs(func(a slog.Attr) bool { line += " " + a.Key + "=" + a.Value.String(); return true })
	r.mu.Lock()
	r.lines = append(r.lines, line)
	r.mu.Unlock()
	return nil
}
func (r *recorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recorder) WithGroup(string) slog.Handler      { return r }
func (r *recorder) has(text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range r.lines {
		if strings.Contains(line, text) {
			return true
		}
	}
	return false
}

func TestBrokerCountsSubscribersAndUsesTheDefaultLogger(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	local, err := broadcast.NewMemory(context.Background(), broadcast.MemoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	broker, err := bcpostgres.New(context.Background(), db, local, bcpostgres.Options{}) // no logger: slog.Default
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = broker.Shutdown(ctx)
	})
	waitListening(t, broker)
	if broker.Subscribers() != 0 {
		t.Fatalf("a new broker has %d subscribers", broker.Subscribers())
	}
	sub, err := broker.Subscribe(context.Background(), global(t, "posts"))
	if err != nil {
		t.Fatal(err)
	}
	if broker.Subscribers() != 1 {
		t.Errorf("Subscribers = %d, want 1", broker.Subscribers())
	}
	sub.Close()
}

func TestPublishTxRefusesWhatCannotBeSentAndReportsAFailedNotify(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	broker := newBroker(t, db, bcpostgres.Options{MaxMessageBytes: 100})
	ctx := context.Background()
	channel := global(t, "posts")

	err := db.InTx(ctx, func(tx pgx.Tx) error {
		return broker.PublishTx(ctx, tx, channel, broadcast.Message{Data: strings.Repeat("x", 101)})
	})
	if !errors.Is(err, broadcast.ErrMessageTooLarge) {
		t.Errorf("an oversized message in a transaction: %v", err)
	}

	// In a transaction that already failed, the notification cannot be queued.
	err = db.InTx(ctx, func(tx pgx.Tx) error {
		_, _ = tx.Exec(ctx, "SELECT no_such_function()")
		if err := broker.PublishTx(ctx, tx, channel, broadcast.Message{Data: "x"}); err == nil || !strings.Contains(err.Error(), "notify") {
			t.Errorf("PublishTx in an aborted transaction = %v", err)
		}
		return errors.New("rollback")
	})
	if err == nil || err.Error() != "rollback" {
		t.Fatal(err)
	}
}

func TestPublishAfterShutdownIsRefusedAndSendsNothing(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	other := newBroker(t, db, bcpostgres.Options{})
	closing := newBroker(t, db, bcpostgres.Options{})
	ctx := context.Background()
	channel := global(t, "posts")
	heard, err := other.Subscribe(ctx, channel)
	if err != nil {
		t.Fatal(err)
	}
	shutdown, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := closing.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	if err := closing.Publish(ctx, channel, broadcast.Message{Data: "late"}); !errors.Is(err, broadcast.ErrClosed) {
		t.Errorf("Publish after Shutdown = %v, want ErrClosed", err)
	}
	expectNothing(t, heard)
}

// A broker whose database is gone keeps retrying with backoff, says why, and
// still stops as soon as it is asked to.
func TestListenerReportsAnUnreachableDatabaseAndStopsPromptly(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	db.Close()
	logs := &recorder{}
	local, err := broadcast.NewMemory(context.Background(), broadcast.MemoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	broker, err := bcpostgres.New(context.Background(), db, local, bcpostgres.Options{Logger: slog.New(logs)})
	if err != nil {
		t.Fatal(err)
	}
	if !waittest.Poll(15*time.Second, func() bool { return logs.has("acquire connection") }) {
		t.Fatal("the unreachable database was never reported")
	}
	if broker.Listening() {
		t.Error("a broker without a connection claims to be listening")
	}
	stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	began := time.Now()
	if err := broker.Shutdown(stop); err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("Shutdown waited %s for a backoff", took)
	}
}
