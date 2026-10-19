package postgres_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/dbtest"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/queue"
	queuepostgres "github.com/rocketinsidecorp/forge-go/queue/postgres"
	"github.com/rocketinsidecorp/forge-go/telemetry"
	"github.com/rocketinsidecorp/forge-go/telemetry/telemetrytest"
)

func TestWorkerTelemetryOnPostgres(t *testing.T) {
	t.Parallel()
	tel, recorder := telemetrytest.New(t)
	cfg := postgrestest.Config(t)
	dbtest.Migrate(t, queuepostgres.Migrations(), func(loaded []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
		return postgres.NewMigrator(cfg, loaded, logger)
	})
	ctx := context.Background()
	db, err := postgres.Open(ctx, cfg, postgres.WithTelemetry(tel))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	store, err := queuepostgres.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	registry := queue.NewRegistry()
	done := make(chan struct{}, 1)
	if err := queue.Register(registry, "order.confirm", func(ctx context.Context, _ order) error {
		if _, err := db.Exec(ctx, "SELECT 1"); err != nil {
			return err
		}
		done <- struct{}{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	worker, err := queue.NewWorker(store, registry, queue.WorkerOptions{PollInterval: 10 * time.Millisecond, MaxPollInterval: 20 * time.Millisecond, Telemetry: tel})
	if err != nil {
		t.Fatal(err)
	}
	traced, request := tel.Tracer("test").Start(ctx, "request", telemetry.SpanKindServer)
	result, err := queue.Enqueue(traced, store, registry, "order.confirm", order{ID: "o-1"})
	if err != nil {
		t.Fatal(err)
	}
	request.End()
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("job did not run")
	}
	stop, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := worker.Shutdown(stop); err != nil {
		t.Fatal(err)
	}

	var process telemetry.SpanData
	var insertUnderPublish, selectUnderProcess bool
	spans := recorder.Spans(t, tel)
	kinds := map[telemetry.SpanID]telemetry.SpanKind{}
	for _, span := range spans {
		kinds[span.SpanContext.SpanID] = span.Kind
		if span.Kind == telemetry.SpanKindConsumer {
			process = span
		}
	}
	for _, span := range spans {
		switch {
		case span.Name == "INSERT" && kinds[span.ParentSpanID] == telemetry.SpanKindProducer:
			insertUnderPublish = true
		case span.Name == "SELECT" && span.ParentSpanID == process.SpanContext.SpanID:
			selectUnderProcess = true
		}
	}
	if id, _ := telemetrytest.Attribute(process, "messaging.message.id"); id != result.ID.String() {
		t.Fatalf("processing span job id %q, want %s", id, result.ID)
	}
	if !insertUnderPublish || !selectUnderProcess {
		t.Fatalf("database spans not nested: insert under publish %v, select under process %v", insertUnderPublish, selectUnderProcess)
	}
	if telemetrytest.Series(tel, queue.MetricJobDuration)["messaging.destination.name=default,forge.job.kind=order.confirm,forge.job.outcome=succeeded"] != 1 {
		t.Fatal("job duration not recorded")
	}
}
