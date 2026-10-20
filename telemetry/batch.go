package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// SpanExporter sends ended spans to a backend. ExportSpans is called from one
// goroutine at a time; it must honor ctx and must not retain spans after it
// returns. Shutdown is called once, after the last export. An exporter given
// for several signals (one OTLPExporter as SpanExporter, MetricExporter and
// LogRecordExporter) is shut down once, after every one of those signals has
// stopped, so Shutdown never runs concurrently with an export.
type SpanExporter interface {
	ExportSpans(ctx context.Context, resource Resource, spans []SpanData) error
	Shutdown(ctx context.Context) error
}

// batcher is the bounded in-memory queue between a producer (span End, a log
// call) and an exporter, for spans and for log records. One goroutine,
// started by newBatcher and stopped by shutdown, owns export. A full queue
// drops new items and counts them instead of blocking requests.
//
// Every export runs under runCtx, which shutdown cancels when its own context
// ends, so a hung collector cannot hold the process past its shutdown budget.
type batcher[T any] struct {
	// signal names the kind of item in messages: "span" or "log".
	signal string
	// failedMessage and countKey make the warning logged when an export
	// fails; the span ones are kept verbatim for existing log alerts.
	failedMessage string
	countKey      string
	// export sends one batch; shutdownExporter is called once, after the
	// last export. It is the release of a shared exporter (see
	// exporterShutdowns), which shuts the exporter down after its last
	// signal stops.
	export           func(ctx context.Context, resource Resource, batch []T) error
	shutdownExporter func(ctx context.Context) error
	resource         Resource
	logger           *slog.Logger
	maxQueue         int
	maxBatch         int
	interval         time.Duration
	timeout          time.Duration
	// onError is called after a failed export of n items.
	onError func(n int)

	runCtx    context.Context
	cancelRun context.CancelFunc

	mu     sync.Mutex
	queue  []T
	closed bool

	full     chan struct{}
	flushes  chan chan struct{}
	stopOnce sync.Once
	stopping chan struct{}
	done     chan struct{}

	dropped  atomic.Uint64
	exported atomic.Uint64
	failed   atomic.Uint64
}

// newRunContext returns the context every export of one batcher runs under,
// and the function that cancels it.
func newRunContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

// batcherConfig configures newBatcher.
type batcherConfig[T any] struct {
	signal           string
	failedMessage    string
	countKey         string
	export           func(ctx context.Context, resource Resource, batch []T) error
	shutdownExporter func(ctx context.Context) error
	resource         Resource
	logger           *slog.Logger
	maxQueue         int
	maxBatch         int
	interval         time.Duration
	timeout          time.Duration
	onError          func(n int)
}

func newBatcher[T any](c batcherConfig[T]) *batcher[T] {
	b := &batcher[T]{
		signal:           c.signal,
		failedMessage:    c.failedMessage,
		countKey:         c.countKey,
		export:           c.export,
		shutdownExporter: c.shutdownExporter,
		resource:         c.resource,
		logger:           c.logger,
		maxQueue:         c.maxQueue,
		maxBatch:         c.maxBatch,
		interval:         c.interval,
		timeout:          c.timeout,
		onError:          c.onError,
		queue:            make([]T, 0, c.maxBatch),
		full:             make(chan struct{}, 1),
		flushes:          make(chan chan struct{}),
		stopping:         make(chan struct{}),
		done:             make(chan struct{}),
	}
	b.runCtx, b.cancelRun = newRunContext()
	go b.run()
	return b
}

// enqueue adds an item without blocking. It reports false when the item was
// dropped.
func (b *batcher[T]) enqueue(item T) bool {
	b.mu.Lock()
	if b.closed || len(b.queue) >= b.maxQueue {
		b.mu.Unlock()
		b.dropped.Add(1)
		return false
	}
	b.queue = append(b.queue, item)
	ready := len(b.queue) >= b.maxBatch
	b.mu.Unlock()
	if ready {
		select {
		case b.full <- struct{}{}:
		default:
		}
	}
	return true
}

func (b *batcher[T]) run() {
	defer close(b.done)
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			b.drain(false)
		case <-b.full:
			b.drain(true)
		case reply := <-b.flushes:
			b.drain(false)
			close(reply)
		case <-b.stopping:
			b.drain(false)
			// The exporter is shut down here, after its last export, never
			// concurrently with one.
			_ = b.shutdownExporter(b.runCtx)
			return
		}
	}
}

// drain exports queued items in batches. With fullOnly it stops once less
// than a full batch remains, leaving the rest for the next tick. Once runCtx
// is cancelled the remaining items are dropped and counted.
func (b *batcher[T]) drain(fullOnly bool) {
	for {
		if b.runCtx.Err() != nil {
			b.dropRemaining()
			return
		}
		b.mu.Lock()
		n := min(len(b.queue), b.maxBatch)
		if n == 0 || (fullOnly && n < b.maxBatch) {
			b.mu.Unlock()
			return
		}
		batch := make([]T, n)
		copy(batch, b.queue[:n])
		rest := copy(b.queue, b.queue[n:])
		clear(b.queue[rest:])
		b.queue = b.queue[:rest]
		b.mu.Unlock()

		exportCtx, cancel := context.WithTimeout(b.runCtx, b.timeout)
		err := b.export(exportCtx, b.resource, batch)
		cancel()
		if err != nil {
			b.failed.Add(uint64(n)) // #nosec G115 -- n is a slice length
			if b.onError != nil {
				b.onError(n)
			}
			b.logger.Warn(b.failedMessage, b.countKey, n, "error_type", ErrorType(err), "error", err)
		} else {
			b.exported.Add(uint64(n)) // #nosec G115 -- n is a slice length
		}
	}
}

func (b *batcher[T]) dropRemaining() {
	b.mu.Lock()
	n := len(b.queue)
	clear(b.queue)
	b.queue = b.queue[:0]
	b.mu.Unlock()
	b.dropped.Add(uint64(n)) // #nosec G115 -- n is a slice length
}

// flush exports everything queued so far, waiting at most until ctx ends.
func (b *batcher[T]) flush(ctx context.Context) error {
	reply := make(chan struct{})
	select {
	case b.flushes <- reply:
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-reply:
		return nil
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// shutdown refuses new items and exports the queue, returning no later than
// ctx's end. When ctx ends first, the export in progress is cancelled, the
// items still queued are dropped and counted, and the wrapped ctx error is
// returned; the goroutine exits as soon as the exporter honors the
// cancellation.
func (b *batcher[T]) shutdown(ctx context.Context) error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	stopCancel := context.AfterFunc(ctx, b.cancelRun)
	defer stopCancel()
	b.stopOnce.Do(func() { close(b.stopping) })
	select {
	case <-b.done:
		b.cancelRun()
		return nil
	case <-ctx.Done():
		b.cancelRun()
		b.dropRemaining()
		return fmt.Errorf("telemetry: %s export did not finish before the shutdown deadline: %w", b.signal, ctx.Err())
	}
}
