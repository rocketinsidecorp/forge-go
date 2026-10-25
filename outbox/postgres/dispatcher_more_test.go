package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/events"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

func noopHandler(context.Context, events.Event) error { return nil }

func TestNewDispatcherRefusesUnsafeOptions(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, Migrations())
	pool := db.Pool()
	if _, err := NewDispatcher(nil, noopHandler, DispatcherOptions{}); err == nil {
		t.Error("a nil pool was accepted")
	}
	if _, err := NewDispatcher(pool, nil, DispatcherOptions{}); err == nil {
		t.Error("a nil handler was accepted")
	}
	for name, opts := range map[string]DispatcherOptions{
		"too many workers":      {Workers: 257},
		"negative workers":      {Workers: -1},
		"too many attempts":     {MaxAttempts: maxDispatcherAttempts + 1},
		"sub-microsecond lease": {Lease: time.Nanosecond},
		"sub-microsecond base":  {RetryBase: time.Nanosecond},
		"sub-microsecond max":   {RetryMax: time.Nanosecond},
		"huge lease":            {Lease: 25 * time.Hour},
		"huge base":             {RetryBase: 25 * time.Hour},
		"huge max":              {RetryMax: 25 * time.Hour},
		"max below base":        {RetryBase: time.Minute, RetryMax: time.Second},
	} {
		if _, err := NewDispatcher(pool, noopHandler, opts); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	var ctx context.Context
	d, err := NewDispatcher(pool, noopHandler, DispatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Run(ctx); err == nil {
		t.Error("Run accepted a nil context")
	}
	if d.opts.Workers != 1 || d.opts.MaxAttempts != 10 || d.opts.Lease != 30*time.Second || d.opts.RetryMax != time.Minute {
		t.Errorf("defaults = %+v", d.opts)
	}
}

func TestRetryDelayIsBoundedAndJittered(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, Migrations())
	d, err := NewDispatcher(db.Pool(), noopHandler, DispatcherOptions{RetryBase: time.Millisecond, RetryMax: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 40; attempt++ {
		delay, err := d.retryDelay(attempt)
		if err != nil || delay < 0 || delay > 5*time.Millisecond {
			t.Fatalf("attempt %d: delay %v err %v", attempt, delay, err)
		}
	}
	// Past the doubling range the upper bound is the maximum itself: some
	// draw out of many must exceed the base.
	exceeded := false
	for range 200 {
		if delay, _ := d.retryDelay(30); delay > time.Millisecond {
			exceeded = true
		}
	}
	if !exceeded {
		t.Error("a late attempt never waited longer than the base")
	}
}

func TestOutcomeNamesEachWayAnAttemptEnds(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, Migrations())
	d, err := NewDispatcher(db.Pool(), noopHandler, DispatcherOptions{MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	for name, tc := range map[string]struct {
		attempts        int
		handlerErr, err error
		want            string
	}{
		"lease lost":    {1, nil, ErrLeaseLost, "lease_lost"},
		"store failure": {1, nil, boom, "error"},
		"delivered":     {1, nil, nil, "delivered"},
		"dead lettered": {3, boom, nil, "dead_lettered"},
		"retry":         {1, boom, nil, "retry"},
	} {
		if got := d.outcome(tc.attempts, tc.handlerErr, tc.err); got != tc.want {
			t.Errorf("%s: outcome = %q, want %q", name, got, tc.want)
		}
	}
}

func TestDispatcherOperationsFailWhenTheTableIsUnavailable(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, Migrations())
	event := insertEvents(t, db, 1)[0]
	d, err := NewDispatcher(db.Pool(), noopHandler, DispatcherOptions{MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, token, attempts, found, err := d.claim(ctx)
	if err != nil || !found {
		t.Fatalf("claim: %v %v", found, err)
	}
	rename := func(from, to string) {
		t.Helper()
		if _, err := db.Exec(ctx, "ALTER TABLE "+from+" RENAME TO "+to); err != nil {
			t.Fatal(err)
		}
	}
	rename("forge_outbox_events", "forge_outbox_events_gone")
	if _, _, _, found, err := d.claim(ctx); err == nil || found {
		t.Errorf("claim without the table: found=%v err=%v", found, err)
	}
	for name, err := range map[string]error{
		"ack":               d.ack(ctx, event.ID, token),
		"retry":             d.retry(ctx, event.ID, token, attempts),
		"dead-letter":       d.retry(ctx, event.ID, token, 2),
		"dispatchOne":       func() error { _, err := d.dispatchOne(ctx); return err }(),
		"stats":             func() error { _, err := d.Stats(ctx); return err }(),
		"list dead letters": func() error { _, err := d.ListDeadLetters(ctx, nil, 10); return err }(),
		"replay":            d.ReplayDeadLetter(ctx, event.ID),
	} {
		if err == nil {
			t.Errorf("%s succeeded without the table", name)
		}
		if errors.Is(err, ErrLeaseLost) {
			t.Errorf("%s reported a lost lease for a database failure", name)
		}
	}
	// Run stops with the persistence error instead of spinning.
	if err := d.Run(ctx); err == nil || strings.Contains(err.Error(), "canceled") {
		t.Errorf("Run = %v, want the persistence error", err)
	}
	rename("forge_outbox_events_gone", "forge_outbox_events")
	// The lease survived the outage: the acknowledgement now succeeds.
	if err := d.ack(ctx, event.ID, token); err != nil {
		t.Errorf("ack after the table returned: %v", err)
	}
}

func TestRunStopsWhenTheLeaseIsLostMidHandler(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, Migrations())
	event := insertEvents(t, db, 1)[0]
	started := make(chan struct{})
	d, err := NewDispatcher(db.Pool(), func(ctx context.Context, _ events.Event) error {
		close(started)
		<-ctx.Done() // the renewal failure cancels the handler's context
		return ctx.Err()
	}, DispatcherOptions{PollInterval: time.Millisecond, Lease: 900 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- d.Run(context.Background()) }()
	<-started
	// Another dispatcher reclaims the event: the token changes.
	if _, err := db.Exec(context.Background(), `UPDATE forge_outbox_events SET lease_token = $2 WHERE id = $1`, event.ID, "01890000-0000-7000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("Run = %v, want ErrLeaseLost", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not stop after the lease was lost")
	}
}

func TestRunStopsWhenTheLeaseCannotBeRenewed(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, Migrations())
	insertEvents(t, db, 1)
	started := make(chan struct{})
	d, err := NewDispatcher(db.Pool(), func(ctx context.Context, _ events.Event) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}, DispatcherOptions{PollInterval: time.Millisecond, Lease: 60 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- d.Run(context.Background()) }()
	<-started
	if _, err := db.Exec(context.Background(), `ALTER TABLE forge_outbox_events RENAME TO forge_outbox_events_gone`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "renew outbox event lease") {
			t.Fatalf("Run = %v, want the renewal failure", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not stop after the renewal failed")
	}
}

func TestShutdownDuringAHandlerSchedulesARetry(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, Migrations())
	event := insertEvents(t, db, 1)[0]
	started := make(chan struct{})
	d, err := NewDispatcher(db.Pool(), func(ctx context.Context, _ events.Event) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}, DispatcherOptions{PollInterval: time.Millisecond, Lease: time.Minute, RetryBase: time.Hour, RetryMax: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- d.Run(ctx) }()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	var attempts int
	var leased bool
	var delivered, dead bool
	if err := db.QueryRow(context.Background(),
		`SELECT attempts, lease_token IS NOT NULL, delivered_at IS NOT NULL, dead_lettered_at IS NOT NULL FROM forge_outbox_events WHERE id = $1`, event.ID).
		Scan(&attempts, &leased, &delivered, &dead); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || leased || delivered || dead {
		t.Fatalf("attempts=%d leased=%v delivered=%v dead=%v: the interrupted event must be released for a retry", attempts, leased, delivered, dead)
	}
	var waiting bool
	if err := db.QueryRow(context.Background(), `SELECT available_at > clock_timestamp() FROM forge_outbox_events WHERE id = $1`, event.ID).Scan(&waiting); err != nil || !waiting {
		t.Fatalf("the retry must wait for its backoff: %v %v", waiting, err)
	}
}
