package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/events"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/outbox/postgres"
	forgepostgres "github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

func TestInsertCommitsWithDomainTransaction(t *testing.T) {
	db := postgrestest.NewMigrated(t, postgres.Migrations())
	if _, err := db.Exec(context.Background(), "CREATE TABLE domain_changes (id text PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	event := newTestEvent(t)
	ctx := context.Background()
	if err := db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO domain_changes VALUES ('task-1')"); err != nil {
			return err
		}
		return postgres.Insert(ctx, tx, event)
	}); err != nil {
		t.Fatal(err)
	}
	assertEventStored(t, db, event)
	var domainChanges int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM domain_changes WHERE id = 'task-1'").Scan(&domainChanges); err != nil {
		t.Fatal(err)
	}
	if domainChanges != 1 {
		t.Fatalf("domain changes committed=%d, want 1", domainChanges)
	}
}

func TestInsertRollsBackWithDomainTransaction(t *testing.T) {
	db := postgrestest.NewMigrated(t, postgres.Migrations())
	if _, err := db.Exec(context.Background(), "CREATE TABLE domain_changes (id text PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	event := newTestEvent(t)
	wantErr := errors.New("domain write failed")
	err := db.InTx(context.Background(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), "INSERT INTO domain_changes VALUES ('task-1')"); err != nil {
			return err
		}
		if err := postgres.Insert(context.Background(), tx, event); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("InTx() error=%v, want %v", err, wantErr)
	}
	var count int
	if err := db.QueryRow(context.Background(), "SELECT count(*) FROM forge_outbox_events WHERE id = $1", event.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("outbox row survived rollback: count=%d", count)
	}
	if err := db.QueryRow(context.Background(), "SELECT count(*) FROM domain_changes").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("domain row survived rollback: count=%d", count)
	}
}

func TestInsertRejectsInvalidEventBeforeDatabaseCall(t *testing.T) {
	if err := postgres.Insert(context.Background(), nil, events.Event{}); err == nil {
		t.Fatal("Insert() accepted invalid event")
	}
	if err := postgres.Insert(context.Background(), nil, newTestEvent(t)); err == nil {
		t.Fatal("Insert() accepted nil transaction")
	}
}

func newTestEvent(t *testing.T) events.Event {
	t.Helper()
	event, err := events.New("task.created", 1, json.RawMessage(`{"task_id":"task-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func assertEventStored(t *testing.T, db *forgepostgres.DB, event events.Event) {
	t.Helper()
	var eventType string
	var version int
	var payload []byte
	if err := db.QueryRow(context.Background(), `
		SELECT event_type, schema_version, payload FROM forge_outbox_events WHERE id = $1
	`, event.ID).Scan(&eventType, &version, &payload); err != nil {
		t.Fatal(err)
	}
	if eventType != event.Type || version != event.Version || !json.Valid(payload) {
		t.Fatalf("stored event type=%q version=%d payload=%s", eventType, version, payload)
	}
	var want, got map[string]string
	if err := json.Unmarshal(event.Payload, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if got["task_id"] != want["task_id"] {
		t.Fatalf("stored payload=%s want=%s", payload, event.Payload)
	}
}

func TestMigrationsComposeWithIdentitySchema(t *testing.T) {
	loaded, err := migrate.LoadMigrationSets(authpostgres.Migrations(), postgres.Migrations())
	if err != nil {
		t.Fatalf("LoadMigrationSets(): %v", err)
	}
	if len(loaded) == 0 {
		t.Fatal("no migrations loaded")
	}
	var outboxMigration *migrate.Migration
	for i := range loaded {
		if loaded[i].Name == "outbox" {
			outboxMigration = &loaded[i]
			break
		}
	}
	if outboxMigration == nil {
		t.Fatal("outbox migration missing from composed set")
	}
	if outboxMigration.Down.SQL != "" {
		t.Fatal("outbox migration must be irreversible to protect persisted events")
	}
}
