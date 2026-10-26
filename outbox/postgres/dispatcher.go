package postgres

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

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

// DefaultBatchSize is the number of events a dispatcher worker claims per
// round trip unless DispatcherOptions.BatchSize says otherwise.
const DefaultBatchSize = 10

// MaxBatchSize bounds DispatcherOptions.BatchSize.
const MaxBatchSize = 1000

// releaseTimeout bounds the statements that hand events back after a worker
// stopped early.
const releaseTimeout = 5 * time.Second

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
	// BatchSize is how many events a worker claims in one transaction; it
	// then delivers them one after another, in claim order, acknowledging
	// each on its own. Defaults to DefaultBatchSize (10), at most
	// MaxBatchSize. The leases of every event of the batch are renewed
	// together, so events waiting their turn are never reclaimed by another
	// dispatcher; on shutdown or failure the ones not yet started are handed
	// back without using an attempt. A larger batch means fewer claim
	// transactions but more events waiting behind a slow handler; 1 claims
	// one event at a time.
	//
	// The claim counts an attempt for every event of the batch. If the
	// process crashes, the events it had claimed but not started keep that
	// attempt when their leases expire: up to BatchSize-1 attempts are spent
	// without a delivery, and an event already on its last attempt is then
	// dead-lettered without having been delivered. Size MaxAttempts with
	// that in mind.
	BatchSize int
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
	if opts.BatchSize < 0 || opts.BatchSize > MaxBatchSize {
		return nil, fmt.Errorf("outbox batch size must be between 0 and %d", MaxBatchSize)
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
	if opts.BatchSize == 0 {
		opts.BatchSize = DefaultBatchSize
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
		worked, err := d.dispatchBatch(ctx)
		if err != nil {
			// pgx interrupts an in-flight query on cancellation by expiring the
			// socket deadline, so the error can be "i/o timeout" instead of
			// context.Canceled. Shutdown is not a delivery failure.
			if ctx.Err() != nil {
				return ctx.Err()
			}
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

// claimed is one event of a claimed batch.
type claimed struct {
	event    events.Event
	attempts int
}

// dispatchBatch claims up to BatchSize events and delivers them in claim
// order. It reports whether it claimed any.
func (d *Dispatcher) dispatchBatch(ctx context.Context) (bool, error) {
	batch, token, err := d.claimBatch(ctx, d.opts.BatchSize)
	if err != nil || len(batch) == 0 {
		return false, err
	}
	leases := newBatchLeases(batch)
	stopRenewal := make(chan struct{})
	renewalDone := make(chan struct{})
	go func() {
		defer close(renewalDone)
		d.renewLeases(ctx, token, leases, stopRenewal)
	}()
	for _, item := range batch {
		if ctx.Err() != nil {
			break
		}
		// The handler's cancel is registered in the same critical section
		// that checks the lease, so a renewal that finds the lease lost
		// either skips the event or cancels its handler.
		workCtx, cancelWork := context.WithCancel(ctx)
		begun, err := leases.begin(item.event.ID, cancelWork)
		if err != nil || !begun {
			cancelWork()
			if err != nil {
				break
			}
			continue // reclaimed by another dispatcher before its turn: it delivers it
		}
		started := time.Now()
		span := d.tel.start(item.event, item.attempts)
		handlerErr, err := d.deliver(telemetry.ContextWithSpan(ctx, span), telemetry.ContextWithSpan(workCtx, span), leases, item.event, token, item.attempts)
		cancelWork()
		d.tel.finish(span, item.event, time.Since(started), d.outcome(item.attempts, handlerErr, err), handlerErr, err)
		leases.settled(item.event.ID)
		if err != nil {
			leases.fail(err)
			break
		}
	}
	close(stopRenewal)
	<-renewalDone
	err = leases.failure()
	// Events never started (shutdown, or a failure above) go back to the
	// outbox at once and keep their attempt.
	if unstarted := leases.held(); len(unstarted) > 0 {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		releaseErr := d.release(releaseCtx, unstarted, token)
		cancel()
		if err != nil && releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
	}
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
// workCtx is the handler's context, which a lost lease cancels.
func (d *Dispatcher) deliver(ctx, workCtx context.Context, leases *batchLeases, event events.Event, token uuid.UUID, attempts int) (error, error) {
	handlerErr := d.handler(workCtx, event)
	if leaseErr := leases.finished(); leaseErr != nil {
		return handlerErr, leaseErr
	}
	if handlerErr != nil {
		if ctx.Err() != nil {
			retryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
			defer cancel()
			return handlerErr, d.retry(retryCtx, event.ID, token, attempts)
		}
		return handlerErr, d.retry(ctx, event.ID, token, attempts)
	}
	return nil, d.ack(ctx, event.ID, token)
}

// batchLeases tracks the events of one claimed batch this worker still holds:
// claimed, not yet acknowledged, and not known to be reclaimed. The renewal
// goroutine and the delivering goroutine share it.
type batchLeases struct {
	mu      sync.Mutex
	holding map[uuid.UUID]bool
	current uuid.UUID          // the event whose handler runs, or zero
	cancel  context.CancelFunc // cancels that handler
	lost    error              // why the current handler's lease ended
	err     error              // a failure that ends the batch
}

func newBatchLeases(batch []claimed) *batchLeases {
	holding := make(map[uuid.UUID]bool, len(batch))
	for _, item := range batch {
		holding[item.event.ID] = true
	}
	return &batchLeases{holding: holding}
}

// begin makes the event the running one, with cancel stopping its handler,
// and reports true when the event is still held. It reports false when the
// lease was lost before the event's turn, and the failure that ended the
// batch, if one did.
func (b *batchLeases) begin(id uuid.UUID, cancel context.CancelFunc) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return false, b.err
	}
	if !b.holding[id] {
		return false, nil
	}
	b.current, b.cancel, b.lost = id, cancel, nil
	return true, nil
}

// finished ends the current handler's turn and returns why its lease ended
// while it ran, if it did.
func (b *batchLeases) finished() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	lost := b.lost
	b.current, b.cancel, b.lost = uuid.UUID{}, nil, nil
	return lost
}

// settled forgets an event whose outcome was written (or lost).
func (b *batchLeases) settled(id uuid.UUID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.holding, id)
}

func (b *batchLeases) fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err == nil {
		b.err = err
	}
}

func (b *batchLeases) failure() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

func (b *batchLeases) held() []uuid.UUID {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := make([]uuid.UUID, 0, len(b.holding))
	for id := range b.holding {
		ids = append(ids, id)
	}
	return ids
}

// renewed records a renewal of ids: the ones missing from kept were
// reclaimed by another dispatcher. Losing the running event's lease cancels
// its handler and ends the batch; an event that was waiting its turn is
// simply skipped.
func (b *batchLeases) renewed(ids []uuid.UUID, kept map[uuid.UUID]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range ids {
		if kept[id] || !b.holding[id] {
			continue
		}
		delete(b.holding, id)
		if id == b.current {
			b.lost = ErrLeaseLost
			if b.err == nil {
				b.err = ErrLeaseLost
			}
			b.cancel()
		}
	}
}

// renewalFailed ends the batch after a renewal error: the running handler is
// canceled, since its lease can no longer be trusted.
func (b *batchLeases) renewalFailed(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err == nil {
		b.err = err
	}
	if b.cancel != nil {
		b.lost = err
		b.cancel()
	}
}

// renewLeases extends the leases of every event the batch still holds, in
// one statement every Lease/3, until stop closes or a renewal fails.
func (d *Dispatcher) renewLeases(ctx context.Context, token uuid.UUID, leases *batchLeases, stop <-chan struct{}) {
	interval := max(d.opts.Lease/3, time.Microsecond)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		ids := leases.held()
		if len(ids) == 0 {
			continue
		}
		refreshCtx, cancel := context.WithTimeout(ctx, min(5*time.Second, interval))
		kept, err := d.renew(refreshCtx, ids, token)
		cancel()
		if err != nil {
			leases.renewalFailed(fmt.Errorf("renew outbox event lease: %w", err))
			return
		}
		leases.renewed(ids, kept)
	}
}

// renew extends the leases of ids still held with token and returns those.
func (d *Dispatcher) renew(ctx context.Context, ids []uuid.UUID, token uuid.UUID) (map[uuid.UUID]bool, error) {
	rows, err := d.pool.Query(ctx, `
		UPDATE forge_outbox_events
		SET lease_until = clock_timestamp() + ($3 * interval '1 microsecond')
		WHERE id = ANY($1) AND lease_token = $2 AND delivered_at IS NULL AND dead_lettered_at IS NULL
		RETURNING id
	`, ids, token, d.opts.Lease.Microseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kept := make(map[uuid.UUID]bool, len(ids))
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		kept[id] = true
	}
	return kept, rows.Err()
}

// release hands back events this worker claimed but never started, without
// counting the attempt the claim recorded.
func (d *Dispatcher) release(ctx context.Context, ids []uuid.UUID, token uuid.UUID) error {
	if _, err := d.pool.Exec(ctx, `
		UPDATE forge_outbox_events
		SET attempts = GREATEST(attempts - 1, 0), lease_token = NULL, lease_until = NULL
		WHERE id = ANY($1) AND lease_token = $2 AND delivered_at IS NULL AND dead_lettered_at IS NULL
	`, ids, token); err != nil {
		return fmt.Errorf("release unstarted outbox events: %w", err)
	}
	return nil
}

// claimBatch dead-letters exhausted events whose lease expired, then leases
// up to limit eligible events in one transaction and returns them in claim
// order. The events share one lease token.
func (d *Dispatcher) claimBatch(ctx context.Context, limit int) ([]claimed, uuid.UUID, error) {
	token, err := uuid.NewV7()
	if err != nil {
		return nil, uuid.UUID{}, fmt.Errorf("generate outbox lease token: %w", err)
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, uuid.UUID{}, fmt.Errorf("begin outbox claim: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
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
		return nil, uuid.UUID{}, fmt.Errorf("dead-letter exhausted outbox leases: %w", err)
	}
	rows, err := tx.Query(ctx, `
		WITH candidate AS MATERIALIZED (
			SELECT id FROM forge_outbox_events
			WHERE delivered_at IS NULL AND dead_lettered_at IS NULL
			  AND attempts < $3
			  AND available_at <= clock_timestamp()
			  AND (lease_until IS NULL OR lease_until <= clock_timestamp())
			ORDER BY available_at, enqueued_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $4
		), leased AS (
			UPDATE forge_outbox_events AS event
			SET attempts = event.attempts + 1,
			    lease_token = $1,
			    lease_until = clock_timestamp() + ($2 * interval '1 microsecond')
			FROM candidate
			WHERE event.id = candidate.id
			RETURNING event.id, event.event_type, event.schema_version,
			          event.occurred_at, event.payload, event.attempts,
			          event.available_at, event.enqueued_at
		)
		SELECT id, event_type, schema_version, occurred_at, payload, attempts
		FROM leased
		ORDER BY available_at, enqueued_at, id
	`, token, d.opts.Lease.Microseconds(), d.opts.MaxAttempts, limit)
	if err != nil {
		return nil, uuid.UUID{}, fmt.Errorf("claim outbox events: %w", err)
	}
	batch := make([]claimed, 0, limit)
	for rows.Next() {
		var item claimed
		if err := rows.Scan(&item.event.ID, &item.event.Type, &item.event.Version, &item.event.OccurredAt, &item.event.Payload, &item.attempts); err != nil {
			rows.Close()
			return nil, uuid.UUID{}, fmt.Errorf("scan claimed outbox event: %w", err)
		}
		batch = append(batch, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, uuid.UUID{}, fmt.Errorf("claim outbox events: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, uuid.UUID{}, fmt.Errorf("commit outbox event claim: %w", err)
	}
	return batch, token, nil
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
