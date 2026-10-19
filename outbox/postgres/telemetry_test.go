package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/events"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/telemetry"
	"github.com/rocketinsidecorp/forge-go/telemetry/telemetrytest"
)

func TestDispatcherTelemetry(t *testing.T) {
	db := postgrestest.NewMigrated(t, Migrations())
	created := insertEvents(t, db, 2)
	tel, recorder := telemetrytest.New(t)
	var handled []string
	d, err := NewDispatcher(db.Pool(), func(ctx context.Context, event events.Event) error {
		if !telemetry.SpanFromContext(ctx).IsRecording() {
			t.Error("the handler context must carry the delivery span")
		}
		handled = append(handled, event.ID.String())
		if event.ID == created[1].ID {
			return errors.New("webhook for task-1 returned 500 with token abc")
		}
		return nil
	}, DispatcherOptions{MaxAttempts: 1, Telemetry: tel})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if worked, err := d.dispatchOne(context.Background()); err != nil || !worked {
			t.Fatalf("dispatchOne = %v, %v", worked, err)
		}
	}
	if len(handled) != 2 {
		t.Fatalf("handled %v", handled)
	}
	series := telemetrytest.Series(tel, MetricDispatchDuration)
	if series["forge.event.type=task.created,forge.outbox.outcome=delivered"] != 1 || series["forge.event.type=task.created,forge.outbox.outcome=dead_lettered"] != 1 {
		t.Fatalf("series %v", series)
	}
	var failed int
	for _, span := range recorder.Spans(t, tel) {
		if span.Kind != telemetry.SpanKindConsumer {
			continue
		}
		if span.ParentSpanID.IsValid() || span.Name != "process outbox" {
			t.Fatalf("delivery span %+v", span)
		}
		for _, attr := range span.Attributes {
			if strings.Contains(attr.Value.Emit(), "task-1") || strings.Contains(attr.Value.Emit(), "abc") {
				t.Fatalf("payload or error message recorded: %+v", attr)
			}
		}
		if span.Status == telemetry.StatusError {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("failed spans %d", failed)
	}
}
