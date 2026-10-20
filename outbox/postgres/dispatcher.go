package postgres

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rocketinsidecorp/forge-go/events"
	"github.com/rocketinsidecorp/forge-go/telemetry"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// ErrLeaseLost means another dispatcher reclaimed the event before this
// worker could acknowledge or reject it.
var ErrLeaseLost = errors.New("outbox event lease lost")

const maxDispatcherDuration = 24 * time.Hour
const maxDispatcherAttempts = 1<<31 - 1

// Handler processes one event. Handlers must be idempotent: a process can
// fail after the side effect succeeds but before the outbox acknowledgement.
type Handler func(context.Context, events.Event) error

// DispatcherOptions bounds concurrency, leases, and retry work.
type DispatcherOptions struct {
	Workers      int           // Concurrent handlers; defaults to 1, maximum 256.
	PollInterval time.Duration // Idle polling interval; defaults to 1 second.
	Lease        time.Duration // Initial lease, renewed during handlers; defaults to 30 seconds.
	MaxAttempts  int           // Total handler attempts before dead-lettering; defaults to 10.
	RetryBase    time.Duration // Initial exponential retry bound; defaults to 1 second.
	RetryMax     time.Duration // Maximum retry bound; defaults to 1 minute or RetryBase, whichever is greater.
	// Telemetry traces each delivery attempt with a consumer span (the root
	// of a new trace) and records forge.outbox.dispatch.duration by event
	// type and outcome. Payloads and error messages are never recorded. Nil
	// disables it.
	Telemetry *telemetry.Telemetry
}

// Dispatcher delivers persisted events with at-least-once semantics.
// Construct one per process and call Run from the application lifecycle.
type Dispatcher struct {
	pool    *pgxpool.Pool
	handler Handler
	opts    DispatcherOptions
	tel     *dispatchTelemetry
}

// NewDispatcher creates a PostgreSQL-backed event dispatcher. Zero or negative
// values for options with defaults use conservative defaults; unsafe values
// are rejected.
func NewDispatcher(pool *pgxpool.Pool, handler Handler, opts DispatcherOptions) (*Dispatcher, error) {
	if pool == nil {
		return nil, errors.New("outbox PostgreSQL pool is required")
	}
	if handler == nil {
		return nil, errors.New("outbox event handler is required")
	}
	if opts.Workers < 0 || opts.Workers > 256 {
		return nil, errors.New("outbox workers must be between 0 and 256")
	}
	if opts.MaxAttempts > maxDispatcherAttempts {
		return nil, fmt.Errorf("outbox maximum attempts must not exceed %d", maxDispatcherAttempts)
	}
	if (opts.Lease > 0 && opts.Lease < time.Microsecond) || (opts.RetryBase > 0 && opts.RetryBase < time.Microsecond) || (opts.RetryMax > 0 && opts.RetryMax < time.Microsecond) {
		return nil, errors.New("outbox lease and retry durations must be at least one microsecond")
	}
	if opts.Lease > maxDispatcherDuration || opts.RetryBase > maxDispatcherDuration || opts.RetryMax > maxDispatcherDuration {
		return nil, fmt.Errorf("outbox lease and retry durations must not exceed %s", maxDispatcherDuration)
	}
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = time.Second
	}
	if opts.Lease <= 0 {
		opts.Lease = 30 * time.Second
	}
	if opts.MaxAttempts < 1 {
		opts.MaxAttempts = 10
	}
	if opts.RetryBase <= 0 {
		opts.RetryBase = time.Second
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = max(time.Minute, opts.RetryBase)
	}
	if opts.RetryMax < opts.RetryBase {
		return nil, errors.New("outbox retry maximum must be greater than or equal to its base")
	}
	tel, err := newDispatchTelemetry(opts.Telemetry)
	if err != nil {
		return nil, err
	}
	return &Dispatcher{pool: pool, handler: handler, opts: opts, tel: tel}, nil
}

// Run starts a bounded worker pool and returns when ctx is canceled or a
// persistence/lease error makes continued delivery unsafe.
func (d *Dispatcher) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("outbox dispatcher context is required")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, d.opts.Workers)
	var workers sync.WaitGroup
	for range d.opts.Workers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := d.runWorker(ctx); err != nil && !errors.Is(err, context.Canceled) {
				select {
				case errCh <- err:
					cancel()
				default:
				}
			}
		}()
	}
	workers.Wait()
	select {
	case err := <-errCh:
		return err
	default:
		return ctx.Err()
	}
}

func (d *Dispatcher) runWorker(ctx context.Context) error {
	ticker := time.NewTicker(d.opts.PollInterval)
	defer ticker.Stop()
	for {
		worked, err := d.dispatchOne(ctx)
		if err != nil {
			return err
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher) dispatchOne(ctx context.Context) (bool, error) {
	event, token, attempts, found, err := d.claim(ctx)
	if err != nil || !found {
		return found, err
	}
	started := time.Now()
	span := d.tel.start(event, attempts)
	handlerErr, err := d.deliver(telemetry.ContextWithSpan(ctx, span), event, token, attempts)
	d.tel.finish(span, event, time.Since(started), d.outcome(attempts, handlerErr, err), handlerErr, err)
	return true, err
}

// outcome names how a delivery attempt ended, for telemetry.
func (d *Dispatcher) outcome(attempts int, handlerErr, err error) string {
	switch {
	case errors.Is(err, ErrLeaseLost):
		return "lease_lost"
	case err != nil:
		return "error"
	case handlerErr == nil:
		return "delivered"
	case attempts >= d.opts.MaxAttempts:
		return "dead_lettered"
	}
	return "retry"
}

// deliver runs the handler for a claimed event and records the result. It
// returns the handler's error and the persistence or lease error.
func (d *Dispatcher) deliver(ctx context.Context, event events.Event, token uuid.UUID, attempts int) (error, error) {
	workCtx, cancelWork := context.WithCancel(ctx)
	stopRenewal := make(chan struct{})
	renewalResult := make(chan error, 1)
	go d.renewLease(ctx, event.ID, token, stopRenewal, cancelWork, renewalResult)
	handlerErr := d.handler(workCtx, event)
	close(stopRenewal)
	renewalErr := <-renewalResult
	cancelWork()
	if renewalErr != nil {
		return handlerErr, renewalErr
	}
	if handlerErr != nil {
		if ctx.Err() != nil {
			retryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			return handlerErr, d.retry(retryCtx, event.ID, token, attempts)
		}
		return handlerErr, d.retry(ctx, event.ID, token, attempts)
	}
	return nil, d.ack(ctx, event.ID, token)
}

func (d *Dispatcher) renewLease(ctx context.Context, id, token uuid.UUID, stop <-chan struct{}, cancelHandler context.CancelFunc, result chan<- error) {
	interval := max(d.opts.Lease/3, time.Microsecond)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			result <- nil
			return
		case <-ctx.Done():
			result <- nil
			return
		case <-ticker.C:
			refreshCtx, cancel := context.WithTimeout(ctx, min(5*time.Second, interval))
			tag, err := d.pool.Exec(refreshCtx, `
				UPDATE forge_outbox_events
				SET lease_until = clock_timestamp() + ($3 * interval '1 microsecond')
				WHERE id = $1 AND lease_token = $2 AND delivered_at IS NULL AND dead_lettered_at IS NULL
			`, id, token, d.opts.Lease.Microseconds())
			cancel()
			if err == nil && tag.RowsAffected() == 1 {
				continue
			}
			cancelHandler()
			if err != nil {
				result <- fmt.Errorf("renew outbox event lease: %w", err)
			} else {
				result <- ErrLeaseLost
			}
			return
		}
	}
}

func (d *Dispatcher) claim(ctx context.Context) (events.Event, uuid.UUID, int, bool, error) {
	var event events.Event
	var attempts int
	token, err := uuid.NewV7()
	if err != nil {
		return events.Event{}, uuid.UUID{}, 0, false, fmt.Errorf("generate outbox lease token: %w", err)
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return events.Event{}, uuid.UUID{}, 0, false, fmt.Errorf("begin outbox claim: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	_, err = tx.Exec(ctx, `
		WITH exhausted AS (
			SELECT id FROM forge_outbox_events
			WHERE delivered_at IS NULL AND dead_lettered_at IS NULL
			  AND attempts >= $1 AND available_at <= clock_timestamp()
			  AND (lease_until IS NULL OR lease_until <= clock_timestamp())
			ORDER BY available_at, enqueued_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT 100
		)
		UPDATE forge_outbox_events AS event
		SET dead_lettered_at = clock_timestamp(), lease_token = NULL, lease_until = NULL
		FROM exhausted
		WHERE event.id = exhausted.id
	`, d.opts.MaxAttempts)
	if err != nil {
		return events.Event{}, uuid.UUID{}, 0, false, fmt.Errorf("dead-letter exhausted outbox leases: %w", err)
	}
	err = tx.QueryRow(ctx, `
		WITH candidate AS (
			SELECT id FROM forge_outbox_events
			WHERE delivered_at IS NULL AND dead_lettered_at IS NULL
			  AND attempts < $3
			  AND available_at <= clock_timestamp()
			  AND (lease_until IS NULL OR lease_until <= clock_timestamp())
			ORDER BY available_at, enqueued_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE forge_outbox_events AS event
		SET attempts = event.attempts + 1,
		    lease_token = $1,
		    lease_until = clock_timestamp() + ($2 * interval '1 microsecond')
		FROM candidate
		WHERE event.id = candidate.id
		RETURNING event.id, event.event_type, event.schema_version,
		          event.occurred_at, event.payload, event.attempts
	`, token, d.opts.Lease.Microseconds(), d.opts.MaxAttempts).Scan(&event.ID, &event.Type, &event.Version, &event.OccurredAt, &event.Payload, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return events.Event{}, uuid.UUID{}, 0, false, fmt.Errorf("commit outbox claim cleanup: %w", err)
		}
		return events.Event{}, uuid.UUID{}, 0, false, nil
	}
	if err != nil {
		return events.Event{}, uuid.UUID{}, 0, false, fmt.Errorf("claim outbox event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return events.Event{}, uuid.UUID{}, 0, false, fmt.Errorf("commit outbox event claim: %w", err)
	}
	return event, token, attempts, true, nil
}

func (d *Dispatcher) ack(ctx context.Context, id, token uuid.UUID) error {
	tag, err := d.pool.Exec(ctx, `
		UPDATE forge_outbox_events
		SET delivered_at = clock_timestamp(), lease_token = NULL, lease_until = NULL
		WHERE id = $1 AND lease_token = $2 AND delivered_at IS NULL AND dead_lettered_at IS NULL
	`, id, token)
	if err != nil {
		return fmt.Errorf("acknowledge outbox event: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (d *Dispatcher) retry(ctx context.Context, id, token uuid.UUID, attempts int) error {
	var tag pgconn.CommandTag
	var err error
	if attempts >= d.opts.MaxAttempts {
		tag, err = d.pool.Exec(ctx, `
			UPDATE forge_outbox_events
			SET dead_lettered_at = clock_timestamp(), lease_token = NULL, lease_until = NULL
			WHERE id = $1 AND lease_token = $2 AND delivered_at IS NULL AND dead_lettered_at IS NULL
		`, id, token)
	} else {
		delay, delayErr := d.retryDelay(attempts)
		if delayErr != nil {
			return fmt.Errorf("calculate outbox retry delay: %w", delayErr)
		}
		tag, err = d.pool.Exec(ctx, `
			UPDATE forge_outbox_events
			SET available_at = clock_timestamp() + ($3 * interval '1 microsecond'), lease_token = NULL, lease_until = NULL
			WHERE id = $1 AND lease_token = $2 AND delivered_at IS NULL AND dead_lettered_at IS NULL
		`, id, token, delay.Microseconds())
	}
	if err != nil {
		return fmt.Errorf("schedule outbox retry: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (d *Dispatcher) retryDelay(attempt int) (time.Duration, error) {
	delay := d.opts.RetryBase
	for i := 1; i < attempt && delay < d.opts.RetryMax; i++ {
		if delay > d.opts.RetryMax/2 {
			delay = d.opts.RetryMax
			break
		}
		delay *= 2
	}
	delay = min(delay, d.opts.RetryMax)
	// Full jitter avoids synchronized retries when a downstream service
	// recovers after many events failed at once.
	jitter, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(delay)+1))
	if err != nil {
		return 0, err
	}
	return time.Duration(jitter.Int64()), nil
}
