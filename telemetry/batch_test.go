package telemetry

import (
	"context"
	"errors"
	"runtime"
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
	if stats := tel.Stats(); stats.Dropped != 5 {
		t.Fatalf("spans after shutdown and at the deadline are dropped: %+v", stats)
	}
	waitForGoroutines(t, before)
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
	tel := newTestTelemetry(t, Options{SampleRatio: 1, SpanExporter: exporter, BatchTimeout: time.Hour})
	endSpans(tel, 2)
	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := tel.Stats(); stats.Failed != 2 || stats.Dropped != 2 {
		t.Fatalf("stats %+v", stats)
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
