package sqlite_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/dbtest"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/queue"
	queuesqlite "github.com/rocketinsidecorp/forge-go/queue/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
	"github.com/rocketinsidecorp/forge-go/telemetry"
	"github.com/rocketinsidecorp/forge-go/telemetry/telemetrytest"
)

func TestWorkerTelemetryLinksEnqueueAndProcessing(t *testing.T) {
	tel, recorder := telemetrytest.New(t)
	cfg := sqlitetest.Config(t)
	dbtest.Migrate(t, queuesqlite.Migrations(), func(loaded []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
		return sqlite.NewMigrator(cfg, loaded, logger)
	})
	ctx := context.Background()
	db, err := sqlite.Open(ctx, cfg, sqlite.WithTelemetry(tel))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := queuesqlite.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	registry := queue.NewRegistry()
	done := make(chan string, 2)
	if err := queue.Register(registry, "order.confirm", func(ctx context.Context, o order) error {
		var one int
		if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
			return err
		}
		done <- o.ID
		if o.ID == "bad" {
			return queue.Permanent(errors.New("customer alice@example.com refused"))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	worker, err := queue.NewWorker(store, registry, queue.WorkerOptions{PollInterval: 10 * time.Millisecond, MaxPollInterval: 20 * time.Millisecond, Telemetry: tel})
	if err != nil {
		t.Fatal(err)
	}

	traced, request := tel.Tracer("test").Start(ctx, "POST /orders", telemetry.SpanKindServer)
	good, err := queue.Enqueue(traced, store, registry, "order.confirm", order{ID: "good"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(traced, store, registry, "order.confirm", order{ID: "bad"}); err != nil {
		t.Fatal(err)
	}
	request.End()

	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("jobs did not run")
		}
	}
	stop, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := worker.Shutdown(stop); err != nil {
		t.Fatal(err)
	}

	spans := recorder.Spans(t, tel)
	var publish, process []telemetry.SpanData
	byID := map[telemetry.SpanID]telemetry.SpanData{}
	for _, span := range spans {
		byID[span.SpanContext.SpanID] = span
		switch span.Kind {
		case telemetry.SpanKindProducer:
			publish = append(publish, span)
		case telemetry.SpanKindConsumer:
			process = append(process, span)
		}
	}
	if len(publish) != 2 || len(process) != 2 {
		t.Fatalf("publish %d process %d spans", len(publish), len(process))
	}
	if publish[0].ParentSpanID != request.SpanContext().SpanID || publish[0].Name != "publish default" {
		t.Fatalf("enqueue span %+v", publish[0])
	}
	if id, _ := telemetrytest.Attribute(publish[0], "messaging.message.id"); id != good.ID.String() {
		t.Fatalf("enqueue span job id %q", id)
	}
	outcomes := map[string]string{}
	for _, span := range process {
		if span.ParentSpanID.IsValid() || span.Name != "process default" {
			t.Fatalf("processing span must be a root named after the queue: %+v", span)
		}
		id, _ := telemetrytest.Attribute(span, "messaging.message.id")
		outcome, _ := telemetrytest.Attribute(span, "forge.job.outcome")
		outcomes[id] = outcome
		for _, attr := range span.Attributes {
			if strings.Contains(attr.Value.Emit(), "alice") || strings.Contains(attr.Value.Emit(), "good") {
				t.Fatalf("payload or error message recorded: %+v", attr)
			}
		}
	}
	if outcomes[good.ID.String()] != string(queue.OutcomeSucceeded) || len(outcomes) != 2 {
		t.Fatalf("outcomes %v", outcomes)
	}
	// The handler's database query is a child of its processing span.
	var nested bool
	for _, span := range spans {
		if span.Name == "SELECT" {
			if parent, ok := byID[span.ParentSpanID]; ok && parent.Kind == telemetry.SpanKindConsumer {
				nested = true
			}
		}
	}
	if !nested {
		t.Fatal("handler database spans must nest under the processing span")
	}
	series := telemetrytest.Series(tel, queue.MetricJobDuration)
	if series["messaging.destination.name=default,forge.job.kind=order.confirm,forge.job.outcome=succeeded"] != 1 ||
		series["messaging.destination.name=default,forge.job.kind=order.confirm,forge.job.outcome=dead"] != 1 {
		t.Fatalf("job duration series %v", series)
	}
	if telemetrytest.Series(tel, sqldb.MetricDBOperationDuration)["db.system.name=sqlite,db.operation.name=SELECT,error.type="] == 0 {
		t.Fatal("database statements must be measured")
	}
}
