package auth

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const (
	// DefaultRecoveryQueueCapacity is how many accepted requests an in-process
	// RecoveryQueue holds while its workers are busy.
	DefaultRecoveryQueueCapacity = 256
	// DefaultRecoveryQueueWorkers is how many requests an in-process
	// RecoveryQueue processes at once.
	DefaultRecoveryQueueWorkers = 2
	// DefaultRecoveryQueueJobTimeout bounds one request's lookup, issuance and
	// delivery.
	DefaultRecoveryQueueJobTimeout = 30 * time.Second

	maxRecoveryQueueCapacity = 1 << 20
	maxRecoveryQueueWorkers  = 256
)

// RecoveryProcessor does the work of one accepted recovery request.
// *RecoveryService is one.
type RecoveryProcessor interface {
	Process(ctx context.Context, job RecoveryJob) error
}

// RecoveryQueueConfig configures NewRecoveryQueue. Zero values mean the
// defaults.
type RecoveryQueueConfig struct {
	// Capacity bounds the requests waiting for a worker; more are dropped.
	Capacity int
	// Workers is the number of goroutines processing requests.
	Workers int
	// JobTimeout bounds one request.
	JobTimeout time.Duration
	// Logger receives drops and failures: never a login, tenant, source,
	// token or error text that can quote an address. Default slog.Default().
	Logger *slog.Logger
}

// RecoveryQueue is a RecoveryDispatcher for applications without a
// persistent job queue: a bounded in-process queue with a fixed set of
// workers owned by the application's lifecycle. Start launches the workers
// once the RecoveryService exists, Shutdown stops them; no goroutine outlives
// Shutdown. A full queue never blocks or grows: DispatchRecovery returns
// ErrRecoveryQueueFull and the request is dropped (and logged by the
// RecoveryService), so a flood can delay legitimate resets but cannot exhaust
// memory or slow the page.
//
// It is not durable: requests waiting or running when the process stops are
// lost, which a person asking again recovers from. Applications with a
// persistent queue should use auth/recoveryqueue instead.
//
//	queue, err := auth.NewRecoveryQueue(auth.RecoveryQueueConfig{})
//	service, err := auth.NewRecoveryService(store, sender, auth.WithRecoveryDispatcher(queue))
//	err = queue.Start(service)
//	app.OnShutdown(queue.Shutdown) // after the database's hook is registered
type RecoveryQueue struct {
	config RecoveryQueueConfig

	mu      sync.RWMutex
	jobs    chan RecoveryJob
	started bool
	closed  bool

	cancel context.CancelFunc
	ctx    context.Context
	wg     sync.WaitGroup
}

var _ RecoveryDispatcher = (*RecoveryQueue)(nil)

// NewRecoveryQueue validates config and returns a queue that accepts work
// (up to its capacity) but processes nothing until Start.
func NewRecoveryQueue(config RecoveryQueueConfig) (*RecoveryQueue, error) {
	if config.Capacity == 0 {
		config.Capacity = DefaultRecoveryQueueCapacity
	}
	if config.Workers == 0 {
		config.Workers = DefaultRecoveryQueueWorkers
	}
	if config.JobTimeout == 0 {
		config.JobTimeout = DefaultRecoveryQueueJobTimeout
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Capacity < 1 || config.Capacity > maxRecoveryQueueCapacity {
		return nil, errors.New("recovery queue capacity must be between 1 and 1048576")
	}
	if config.Workers < 1 || config.Workers > maxRecoveryQueueWorkers {
		return nil, errors.New("recovery queue workers must be between 1 and 256")
	}
	if config.JobTimeout < 0 || config.JobTimeout > time.Hour {
		return nil, errors.New("recovery queue job timeout must be between 0 and 1 hour")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &RecoveryQueue{
		config: config,
		jobs:   make(chan RecoveryJob, config.Capacity),
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

// DispatchRecovery queues job without waiting: ErrRecoveryQueueFull when the
// queue holds its capacity, ErrRecoveryQueueClosed after Shutdown.
func (q *RecoveryQueue) DispatchRecovery(_ context.Context, job RecoveryJob) error {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		return ErrRecoveryQueueClosed
	}
	select {
	case q.jobs <- job:
		return nil
	default:
		return ErrRecoveryQueueFull
	}
}

// Start launches the workers. It may be called once, before Shutdown.
func (q *RecoveryQueue) Start(processor RecoveryProcessor) error {
	if processor == nil {
		return errors.New("recovery queue processor is required")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrRecoveryQueueClosed
	}
	if q.started {
		return errors.New("recovery queue already started")
	}
	q.started = true
	for i := 0; i < q.config.Workers; i++ {
		q.wg.Add(1)
		go q.work(processor)
	}
	return nil
}

// Shutdown stops accepting requests, lets the workers finish the queued ones
// until ctx ends, then cancels the work still running and waits for the
// workers to return. It returns ctx's error when requests had to be
// abandoned. It is safe to call more than once.
func (q *RecoveryQueue) Shutdown(ctx context.Context) error {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.jobs)
	}
	started := q.started
	q.mu.Unlock()
	if !started {
		q.cancel()
		return nil
	}

	drained := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		q.cancel()
		return nil
	case <-ctx.Done():
		q.cancel()
		<-drained
		return ctx.Err()
	}
}

func (q *RecoveryQueue) work(processor RecoveryProcessor) {
	defer q.wg.Done()
	abandoned := 0
	for job := range q.jobs {
		if q.ctx.Err() != nil {
			abandoned++
			continue
		}
		q.run(processor, job)
	}
	if abandoned > 0 {
		q.config.Logger.Warn("password recovery requests abandoned at shutdown", "count", abandoned)
	}
}

func (q *RecoveryQueue) run(processor RecoveryProcessor, job RecoveryJob) {
	defer func() {
		if recover() != nil {
			q.config.Logger.Error("password recovery processing panicked")
		}
	}()
	ctx := q.ctx
	if q.config.JobTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, q.config.JobTimeout)
		defer cancel()
	}
	if err := processor.Process(ctx, job); err != nil {
		q.logFailure(ctx, err)
	}
}

// logFailure logs why a request failed without quoting error text that can
// contain an address or a server reply: delivery failures are logged by
// their sender (recoverymail), everything else by its error.
func (q *RecoveryQueue) logFailure(ctx context.Context, err error) {
	var delivery *RecoveryDeliveryError
	if errors.As(err, &delivery) {
		q.config.Logger.ErrorContext(ctx, "password recovery delivery failed")
		return
	}
	q.config.Logger.ErrorContext(ctx, "password recovery request failed", "error", err)
}
