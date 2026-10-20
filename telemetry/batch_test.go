package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"runtime"
	"strings"
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
	if again := tel.Shutdown(context.Background()); !errors.Is(again, context.DeadlineExceeded) {
		t.Fatalf("second Shutdown must return the first result, got %v", again)
	}
	endSpans(tel, 2)
	waitForGoroutines(t, before)
	if stats := tel.Stats(); stats.Dropped != 5 {
		t.Fatalf("spans after shutdown and at the deadline are dropped: %+v", stats)
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
			defer close(exporter.release)
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
			// Everything not exported is counted as dropped once the
			// in-flight export gives up.
			if _, ok := opts.SpanExporter.(*hangingExporter); ok {
				exporter.release <- struct{}{}
			}
			deadline := time.Now().Add(5 * time.Second)
			for tel.Stats().Dropped != 34 {
				if time.Now().After(deadline) {
					t.Fatalf("stats %+v, want all 34 spans dropped", tel.Stats())
				}
				time.Sleep(5 * time.Millisecond)
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
