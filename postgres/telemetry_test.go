package postgres_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/telemetry"
	"github.com/rocketinsidecorp/forge-go/telemetry/telemetrytest"
)

func TestTelemetryTracesStatementsWithoutText(t *testing.T) {
	t.Parallel()
	tel, recorder := telemetrytest.New(t)
	ctx := context.Background()
	db, err := postgres.Open(ctx, postgrestest.Config(t), postgres.WithTelemetry(tel))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Exec(ctx, "CREATE TABLE accounts (id int PRIMARY KEY, owner text NOT NULL)"); err != nil {
		t.Fatal(err)
	}

	traced, request := tel.Tracer("test").Start(ctx, "request", telemetry.SpanKindServer)
	if _, err := db.Exec(traced, "INSERT INTO accounts VALUES ($1, $2)", 1, "carol@example.com"); err != nil {
		t.Fatal(err)
	}
	err = db.InTx(traced, func(tx pgx.Tx) error {
		var owner string
		if err := tx.QueryRow(traced, "SELECT owner FROM accounts WHERE id = $1", 1).Scan(&owner); err != nil {
			return err
		}
		_, err := tx.Exec(traced, "UPDATE accounts SET owner = $1 WHERE id = $2", "dave@example.com", 1)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(traced, "INSERT INTO accounts VALUES ($1, $2)", 1, "eve@example.com"); err == nil {
		t.Fatal("expected a unique violation")
	}
	request.End()

	var names []string
	for _, span := range recorder.Spans(t, tel) {
		if span.Name == "request" {
			continue
		}
		names = append(names, span.Name)
		if span.ParentSpanID != request.SpanContext().SpanID {
			t.Fatalf("span %s is not a child of the request", span.Name)
		}
		if system, _ := telemetrytest.Attribute(span, "db.system.name"); system != "postgresql" {
			t.Fatalf("db.system.name = %q", system)
		}
		for _, attr := range span.Attributes {
			if text := attr.Value.Emit(); strings.Contains(text, "example.com") || strings.Contains(text, "accounts") {
				t.Fatalf("statement text or argument recorded: %s=%s", attr.Key, text)
			}
		}
	}
	// BEGIN and COMMIT are pgx statements too.
	if got := strings.Join(names, ","); got != "INSERT,BEGIN,SELECT,UPDATE,COMMIT,INSERT" {
		t.Fatalf("statement spans %s", got)
	}
	series := telemetrytest.Series(tel, sqldb.MetricDBOperationDuration)
	if series["db.system.name=postgresql,db.operation.name=INSERT,error.type=unique_violation"] != 1 ||
		series["db.system.name=postgresql,db.operation.name=CREATE,error.type="] != 1 {
		t.Fatalf("series %v", series)
	}
}

func TestPoolMetricsReportConnections(t *testing.T) {
	t.Parallel()
	tel, _ := telemetrytest.New(t)
	ctx := context.Background()
	cfg := postgrestest.Config(t)
	cfg.MaxConns = 3
	db, err := postgres.Open(ctx, cfg, postgres.WithTelemetry(tel), postgres.WithPoolName("primary"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	conn, err := db.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()

	var out bytes.Buffer
	if err := tel.Metrics().WritePrometheus(&out); err != nil {
		t.Fatal(err)
	}
	const labels = `{db_system_name="postgresql",db_client_connection_pool_name="primary"`
	for _, want := range []string{
		"db_client_connection_count" + labels + `,db_client_connection_state="used"} 1`,
		"db_client_connection_max" + labels + "} 3",
		"forge_db_pool_waits_total" + labels + "} ",
		"forge_db_pool_wait_duration_seconds_total" + labels + "} ",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("metrics lack %q:\n%s", want, out.String())
		}
	}
}

// A closed pool is no longer observed: its connection counts drop to zero.
func TestPoolMetricsStopWhenThePoolCloses(t *testing.T) {
	t.Parallel()
	tel, _ := telemetrytest.New(t)
	ctx := context.Background()
	db, err := postgres.Open(ctx, postgrestest.Config(t), postgres.WithTelemetry(tel), postgres.WithPoolName("closing"))
	if err != nil {
		t.Fatal(err)
	}
	scrape := func() string {
		var out bytes.Buffer
		if err := tel.Metrics().WritePrometheus(&out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	const labels = `{db_system_name="postgresql",db_client_connection_pool_name="closing"`
	if text := scrape(); !strings.Contains(text, "db_client_connection_count"+labels+`,db_client_connection_state="idle"} 1`) {
		t.Fatalf("open pool:\n%s", text)
	}
	db.Close()
	text := scrape()
	for _, want := range []string{
		"db_client_connection_count" + labels + `,db_client_connection_state="idle"} 0`,
		"db_client_connection_count" + labels + `,db_client_connection_state="used"} 0`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("closed pool, metrics lack %q:\n%s", want, text)
		}
	}
}
