package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func endSpans(tel *Telemetry, n int) {
	for range n {
		_, span := tel.Tracer("x").Start(context.Background(), "op", SpanKindInternal)
		span.End()
	}
}

func TestBatcherExportsFullBatchesWithoutWaitingForTheTimer(t *testing.T) {
	exporter := &recordingExporter{}
	tel := newTestTelemetry(t, Options{SampleRatio: 1, SpanExporter: exporter, MaxExportBatch: 10, BatchTimeout: time.Hour})
	endSpans(tel, 25)
	deadline := time.Now().Add(5 * time.Second)
	for len(exporter.exported()) < 20 {
		if time.Now().After(deadline) {
			t.Fatalf("full batches were not exported: %d", len(exporter.exported()))
		}
		time.Sleep(5 * time.Millisecond)
	}
	shutdown(t, tel)
	if got := len(exporter.exported()); got != 25 {
		t.Fatalf("shutdown must export the remainder: %d", got)
	}
	for _, size := range exporter.batches {
		if size > 10 {
			t.Fatalf("batch of %d exceeds the limit", size)
		}
	}
	if exporter.shutdown != 1 {
		t.Fatalf("exporter shut down %d times", exporter.shutdown)
	}
	if stats := tel.Stats(); stats.Exported != 25 || stats.Dropped != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestBatcherDropsWhenTheQueueIsFull(t *testing.T) {
	exporter := &recordingExporter{block: make(chan struct{})}
	tel := newTestTelemetry(t, Options{SampleRatio: 1, SpanExporter: exporter, MaxQueueSize: 8, MaxExportBatch: 4, BatchTimeout: time.Hour})
	// The first full batch blocks in the exporter; the queue then fills.
	endSpans(tel, 4)
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	endSpans(tel, 100)
	if time.Since(start) > time.Second {
		t.Fatal("ending spans blocked on a full queue")
	}
	stats := tel.Stats()
	if stats.Dropped < 90 {
		t.Fatalf("expected drops when the queue is full, got %+v", stats)
	}
	close(exporter.block)
	shutdown(t, tel)
	if got := uint64(len(exporter.exported())) + tel.Stats().Dropped; got != 104 {
		t.Fatalf("every span is either exported or counted as dropped: %d", got)
	}
	var dropped float64
	for _, metric := range tel.Metrics().Collect() {
		if metric.Name == "forge.telemetry.spans.dropped" {
			dropped = metric.Points[0].Value
		}
	}
	if dropped < 90 {
		t.Fatalf("dropped-span metric = %v", dropped)
	}
}

func TestShutdownRespectsItsDeadlineAndStopsTheGoroutine(t *testing.T) {
	before := runtime.NumGoroutine()
	exporter := &recordingExporter{block: make(chan struct{})}
	tel, err := New(Options{ServiceName: "s", SampleRatio: 1, SpanExporter: exporter, BatchTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	endSpans(tel, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = tel.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Shutdown overran its deadline")
	}
	// The counters are final when Shutdown returns, whether the batch was
	// still queued or its export was in flight at the deadline.
	if stats := tel.Stats(); stats.Dropped != 3 || stats.Exported != 0 {
		t.Fatalf("spans at the deadline are dropped when Shutdown returns: %+v", stats)
	}
	if again := tel.Shutdown(context.Background()); !errors.Is(again, context.DeadlineExceeded) {
		t.Fatalf("second Shutdown must return the first result, got %v", again)
	}
	endSpans(tel, 2)
	waitForBatcher(t, tel.spans.done)
	waitForGoroutines(t, before)
	if stats := tel.Stats(); stats.Dropped != 5 || stats.Exported != 0 {
		t.Fatalf("spans after shutdown and at the deadline are dropped once: %+v", stats)
	}
}

// TestAnExportInFlightAtTheDeadlineIsCountedOnceWhenShutdownReturns covers
// the batcher behind both spans and log records: an export still running when
// shutdown's deadline passes is counted as failed before shutdown returns, and
// whatever it returns later is not counted again.
func TestAnExportInFlightAtTheDeadlineIsCountedOnceWhenShutdownReturns(t *testing.T) {
	for _, late := range []error{nil, errors.New("collector hung")} {
		t.Run(fmt.Sprintf("late result %v", late), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var errorsReported atomic.Int64
			b := newBatcher(batcherConfig[int]{
				signal: "test", failedMessage: "export failed", countKey: "items",
				// Ignores its context: the result arrives after the deadline.
				export: func(context.Context, Resource, []int) error {
					close(started)
					<-release
					return late
				},
				shutdownExporter: func(context.Context) error { return nil },
				logger:           slog.New(slog.DiscardHandler),
				maxQueue:         8, maxBatch: 2, interval: time.Hour, timeout: time.Hour,
				onError: func(n int) { errorsReported.Add(int64(n)) },
			})
			for i := range 5 {
				if !b.enqueue(i) {
					t.Fatalf("item %d dropped", i)
				}
			}
			<-started // a full batch of 2 is in flight, 3 items are queued
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := b.shutdown(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("shutdown = %v, want the context error", err)
			}
			check := func(when string) {
				t.Helper()
				if f, d, e := b.failed.Load(), b.dropped.Load(), b.exported.Load(); f != 2 || d != 3 || e != 0 || errorsReported.Load() != 2 {
					t.Fatalf("%s: failed %d, dropped %d, exported %d, reported %d; want 2, 3, 0, 2", when, f, d, e, errorsReported.Load())
				}
			}
			check("when shutdown returns")
			close(release)
			waitForBatcher(t, b.done)
			check("after the late export returned")
		})
	}
}

// waitForBatcher waits for a batcher's goroutine to exit.
func waitForBatcher(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the batcher goroutine did not stop")
	}
}

// hangingExporter blocks until released, ignoring its context: the worst
// case of a hung collector behind a broken client.
type hangingExporter struct {
	release chan struct{}
	calls   chan struct{}
}

func (e *hangingExporter) ExportSpans(context.Context, Resource, []SpanData) error {
	select {
	case e.calls <- struct{}{}:
	default:
	}
	<-e.release
	return errors.New("collector hung")
}

func (e *hangingExporter) ExportMetrics(context.Context, Resource, []MetricData) error {
	<-e.release
	return errors.New("collector hung")
}

func (e *hangingExporter) Shutdown(context.Context) error { return nil }

func TestShutdownWithAHungCollectorAndAFullQueueMeetsItsDeadline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exporter func(*hangingExporter) Options
	}{
		{"exporter ignoring its context", func(e *hangingExporter) Options {
			return Options{SpanExporter: e, MetricExporter: e}
		}},
		{"exporter honoring its context", func(*hangingExporter) Options {
			return Options{SpanExporter: &recordingExporter{block: make(chan struct{})}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exporter := &hangingExporter{release: make(chan struct{}), calls: make(chan struct{}, 1)}
			release := sync.OnceFunc(func() { close(exporter.release) })
			defer release()
			opts := tc.exporter(exporter)
			opts.ServiceName = "s"
			opts.SampleRatio = 1
			opts.MaxQueueSize = 16
			opts.MaxExportBatch = 4
			opts.BatchTimeout = time.Hour
			opts.ExportTimeout = 3 * time.Second
			tel, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			endSpans(tel, 4) // a full batch: the exporter starts and hangs
			time.Sleep(20 * time.Millisecond)
			endSpans(tel, 30) // fills the queue and overflows it

			const budget = 300 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			start := time.Now()
			result := make(chan error, 1)
			go func() { result <- tel.Shutdown(ctx) }()
			select {
			case err = <-result:
			case <-time.After(budget + time.Second):
				t.Fatalf("Shutdown with a %s budget did not return", budget)
			}
			elapsed := time.Since(start)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Shutdown = %v, want a wrapped deadline error", err)
			}
			if elapsed > budget+200*time.Millisecond {
				t.Fatalf("Shutdown took %s with a %s budget", elapsed, budget)
			}
			// Everything not exported, the hung export included, is counted
			// as dropped by the time Shutdown returns, and only once.
			if stats := tel.Stats(); stats.Dropped != 34 {
				t.Fatalf("stats %+v when Shutdown returned, want all 34 spans dropped", stats)
			}
			release()
			waitForBatcher(t, tel.spans.done)
			if stats := tel.Stats(); stats.Dropped != 34 || stats.Exported != 0 {
				t.Fatalf("stats %+v after the hung export returned, want all 34 spans dropped once", stats)
			}
		})
	}
}

func TestForceFlushExportsQueuedSpans(t *testing.T) {
	exporter := &recordingExporter{}
	tel := newTestTelemetry(t, Options{SampleRatio: 1, SpanExporter: exporter, BatchTimeout: time.Hour})
	endSpans(tel, 3)
	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(exporter.exported()); got != 3 {
		t.Fatalf("ForceFlush exported %d spans", got)
	}
}

func TestExportFailuresAreCountedAndLogged(t *testing.T) {
	exporter := &recordingExporter{err: errors.New("collector down")}
	var diagnostics bytes.Buffer
	tel := newTestTelemetry(t, Options{SampleRatio: 1, SpanExporter: exporter, BatchTimeout: time.Hour, Logger: slog.New(slog.NewJSONHandler(&diagnostics, nil))})
	endSpans(tel, 2)
	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := tel.Stats(); stats.Failed != 2 || stats.Dropped != 2 {
		t.Fatalf("stats %+v", stats)
	}
	// Existing alerts match this message and key.
	if !strings.Contains(diagnostics.String(), `"msg":"telemetry: span export failed; spans dropped","spans":2,`) {
		t.Fatalf("diagnostics: %s", diagnostics.String())
	}
}

func waitForGoroutines(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > want {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines leaked: %d > %d", runtime.NumGoroutine(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sharedTestExporter serves traces, metrics and logs, like the OTLP
// exporter FromConfig builds. Log exports are slow, so the other signals stop
// first; Shutdown records whether any export was still running.
type sharedTestExporter struct {
	inFlight   atomic.Int32
	shutdowns  atomic.Int32
	concurrent atomic.Bool
	logExports atomic.Int32
}

func (e *sharedTestExporter) run(ctx context.Context, delay time.Duration) error {
	e.inFlight.Add(1)
	defer e.inFlight.Add(-1)
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *sharedTestExporter) ExportSpans(ctx context.Context, _ Resource, _ []SpanData) error {
	return e.run(ctx, 0)
}

func (e *sharedTestExporter) ExportMetrics(ctx context.Context, _ Resource, _ []MetricData) error {
	return e.run(ctx, 0)
}

func (e *sharedTestExporter) ExportLogs(ctx context.Context, _ Resource, _ []LogRecord) error {
	e.logExports.Add(1)
	return e.run(ctx, 200*time.Millisecond)
}

func (e *sharedTestExporter) Shutdown(context.Context) error {
	if e.inFlight.Load() != 0 {
		e.concurrent.Store(true)
	}
	e.shutdowns.Add(1)
	return nil
}

func TestASharedExporterIsShutDownOnceAfterEverySignalStopped(t *testing.T) {
	exporter := &sharedTestExporter{}
	tel := newTestTelemetry(t, Options{SampleRatio: 1, SpanExporter: exporter, MetricExporter: exporter, LogExporter: exporter, MetricsInterval: time.Hour})
	endSpans(tel, 1)
	slog.New(tel.LogHandler()).Info("slow to export")
	shutdown(t, tel)
	if exporter.logExports.Load() != 1 {
		t.Fatalf("log exports %d", exporter.logExports.Load())
	}
	if exporter.concurrent.Load() || exporter.shutdowns.Load() != 1 {
		t.Fatalf("shutdowns %d, concurrent with an export: %v", exporter.shutdowns.Load(), exporter.concurrent.Load())
	}

	// Distinct exporters are each shut down once.
	spans, logs := &recordingExporter{}, &recordingLogExporter{}
	tel = newTestTelemetry(t, Options{SpanExporter: spans, LogExporter: logs})
	shutdown(t, tel)
	if spans.shutdown != 1 || logs.shutdown != 1 {
		t.Fatalf("span exporter shutdowns %d, log exporter shutdowns %d", spans.shutdown, logs.shutdown)
	}
}

func TestSameExporterNeverPanicsOnIncomparableValues(t *testing.T) {
	type incomparable struct{ Items []int }
	type holder struct{ value any }
	shared := &recordingExporter{}
	switch {
	case !sameExporter(shared, shared):
		t.Fatal("one pointer is the same exporter")
	case sameExporter(shared, &recordingExporter{}):
		t.Fatal("two pointers are different exporters")
	case sameExporter(incomparable{}, incomparable{}):
		t.Fatal("incomparable values are never the same")
	case sameExporter(holder{value: 1}, holder{value: []int{1}}):
		t.Fatal("a holder of an incomparable value is never the same")
	}
}
