package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// SpanExporter sends ended spans to a backend. ExportSpans is called from one
// goroutine at a time; it must honor ctx and must not retain spans after it
// returns.
type SpanExporter interface {
	ExportSpans(ctx context.Context, resource Resource, spans []SpanData) error
	Shutdown(ctx context.Context) error
}

// batcher is the bounded in-memory span queue between End and the exporter.
// One goroutine, started by newBatcher and stopped by shutdown, owns export.
// A full queue drops new spans and counts them instead of blocking requests.
type batcher struct {
	exporter SpanExporter
	resource Resource
	logger   *slog.Logger
	maxQueue int
	maxBatch int
	interval time.Duration
	timeout  time.Duration
	// onError is called after a failed export of n spans.
	onError func(n int)

	mu     sync.Mutex
	queue  []SpanData
	closed bool

	full     chan struct{}
	flushes  chan chan struct{}
	stopping chan context.Context
	done     chan struct{}

	dropped  atomic.Uint64
	exported atomic.Uint64
	failed   atomic.Uint64
}

func newBatcher(exporter SpanExporter, resource Resource, logger *slog.Logger, maxQueue, maxBatch int, interval, timeout time.Duration, onError func(n int)) *batcher {
	b := &batcher{
		exporter: exporter,
		resource: resource,
		logger:   logger,
		maxQueue: maxQueue,
		maxBatch: maxBatch,
		interval: interval,
		timeout:  timeout,
		onError:  onError,
		queue:    make([]SpanData, 0, maxBatch),
		full:     make(chan struct{}, 1),
		flushes:  make(chan chan struct{}),
		stopping: make(chan context.Context),
		done:     make(chan struct{}),
	}
	go b.run()
	return b
}

// enqueue adds an ended span without blocking. It reports false when the
// span was dropped.
func (b *batcher) enqueue(span SpanData) bool {
	b.mu.Lock()
	if b.closed || len(b.queue) >= b.maxQueue {
		b.mu.Unlock()
		b.dropped.Add(1)
		return false
	}
	b.queue = append(b.queue, span)
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

func (b *batcher) run() {
	defer close(b.done)
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			b.drain(context.Background(), false)
		case <-b.full:
			b.drain(context.Background(), true)
		case reply := <-b.flushes:
			b.drain(context.Background(), false)
			close(reply)
		case ctx := <-b.stopping:
			b.drain(ctx, false)
			return
		}
	}
}

// drain exports queued spans in batches. With fullOnly it stops once less
// than a full batch remains, leaving the rest for the next tick.
func (b *batcher) drain(ctx context.Context, fullOnly bool) {
	for {
		b.mu.Lock()
		n := min(len(b.queue), b.maxBatch)
		if n == 0 || (fullOnly && n < b.maxBatch) {
			b.mu.Unlock()
			return
		}
		batch := make([]SpanData, n)
		copy(batch, b.queue[:n])
		rest := copy(b.queue, b.queue[n:])
		clear(b.queue[rest:])
		b.queue = b.queue[:rest]
		b.mu.Unlock()

		exportCtx, cancel := context.WithTimeout(ctx, b.timeout)
		err := b.exporter.ExportSpans(exportCtx, b.resource, batch)
		cancel()
		if err != nil {
			b.failed.Add(uint64(n)) // #nosec G115 -- n is a slice length
			if b.onError != nil {
				b.onError(n)
			}
			b.logger.Warn("telemetry: span export failed; spans dropped", "spans", n, "error_type", ErrorType(err), "error", err)
		} else {
			b.exported.Add(uint64(n)) // #nosec G115 -- n is a slice length
		}
		if ctx.Err() != nil {
			b.dropRemaining()
			return
		}
	}
}

func (b *batcher) dropRemaining() {
	b.mu.Lock()
	n := len(b.queue)
	clear(b.queue)
	b.queue = b.queue[:0]
	b.mu.Unlock()
	b.dropped.Add(uint64(n)) // #nosec G115 -- n is a slice length
}

// flush exports everything queued so far, waiting at most until ctx ends.
func (b *batcher) flush(ctx context.Context) error {
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
	case <-ctx.Done():
		return ctx.Err()
	}
}

// shutdown refuses new spans, exports the queue within ctx, stops the
// goroutine and shuts the exporter down. Spans still queued when ctx ends
// are dropped and counted.
func (b *batcher) shutdown(ctx context.Context) error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	select {
	case b.stopping <- ctx:
	case <-b.done:
	}
	var err error
	select {
	case <-b.done:
	case <-ctx.Done():
		err = ctx.Err()
		// drain observes ctx through its export contexts and returns soon.
		<-b.done
	}
	return errors.Join(err, b.exporter.Shutdown(ctx))
}
