package sqlite_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
	"github.com/rocketinsidecorp/forge-go/telemetry/telemetrytest"
)

func TestPoolMetricsReportConnectionsAndWaits(t *testing.T) {
	t.Parallel()
	tel, _ := telemetrytest.New(t)
	cfg := sqlitetest.Config(t)
	cfg.MaxConns = 1
	ctx := context.Background()
	db, err := sqlite.Open(ctx, cfg, sqlite.WithTelemetry(tel), sqlite.WithPoolName("main"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	scrape := func() string {
		var out bytes.Buffer
		if err := tel.Metrics().WritePrometheus(&out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	const labels = `{db_system_name="sqlite",db_client_connection_pool_name="main"`
	conn, err := db.SQL().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	text := scrape()
	for _, want := range []string{
		"db_client_connection_count" + labels + `,db_client_connection_state="used"} 1`,
		"db_client_connection_max" + labels + "} 1",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics lack %q:\n%s", want, text)
		}
	}

	// A second caller must wait for the only connection.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	waitForTheOnlyConnection(t, db)
	text = scrape()
	if !strings.Contains(text, "forge_db_pool_waits_total"+labels+"} 1") {
		t.Fatalf("a wait was not counted:\n%s", text)
	}
	if strings.Contains(text, "forge_db_pool_wait_duration_seconds_total"+labels+"} 0\n") {
		t.Fatalf("wait time was not counted:\n%s", text)
	}
	if !strings.Contains(text, "db_client_connection_count"+labels+`,db_client_connection_state="idle"} 1`) {
		t.Fatalf("idle connection not reported:\n%s", text)
	}
}

func TestPoolMetricsDefaultNameAndNoTelemetry(t *testing.T) {
	t.Parallel()
	tel, _ := telemetrytest.New(t)
	db, err := sqlite.Open(context.Background(), sqlitetest.Config(t), sqlite.WithTelemetry(tel))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var out bytes.Buffer
	if err := tel.Metrics().WritePrometheus(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `db_client_connection_pool_name="default"`) {
		t.Fatalf("default pool name missing:\n%s", out.String())
	}
	// Without telemetry Open is unchanged.
	plain, err := sqlite.Open(context.Background(), sqlitetest.Config(t), sqlite.WithTelemetry(nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = plain.Close()
}

// Closing a database stops its pool metrics, and the wait counters of a
// database reopened under the same name continue from the closed pool's
// totals instead of freezing until the new pool catches up.
func TestPoolMetricsStopOnCloseAndKeepCountingAfterReopen(t *testing.T) {
	t.Parallel()
	tel, _ := telemetrytest.New(t)
	cfg := sqlitetest.Config(t)
	cfg.MaxConns = 1
	ctx := context.Background()
	const labels = `{db_system_name="sqlite",db_client_connection_pool_name="reopened"`
	scrape := func() string {
		var out bytes.Buffer
		if err := tel.Metrics().WritePrometheus(&out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	first, err := sqlite.Open(ctx, cfg, sqlite.WithTelemetry(tel), sqlite.WithPoolName("reopened"))
	if err != nil {
		t.Fatal(err)
	}
	waitForTheOnlyConnection(t, first)
	if text := scrape(); !strings.Contains(text, "forge_db_pool_waits_total"+labels+"} 1") ||
		!strings.Contains(text, "db_client_connection_count"+labels+`,db_client_connection_state="idle"} 1`) {
		t.Fatalf("before close:\n%s", text)
	}
	if err := first.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	text := scrape()
	for _, want := range []string{
		"db_client_connection_count" + labels + `,db_client_connection_state="idle"} 0`,
		"db_client_connection_count" + labels + `,db_client_connection_state="used"} 0`,
		"forge_db_pool_waits_total" + labels + "} 1",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("after close, metrics lack %q:\n%s", want, text)
		}
	}

	second, err := sqlite.Open(ctx, cfg, sqlite.WithTelemetry(tel), sqlite.WithPoolName("reopened"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	waitForTheOnlyConnection(t, second)
	if text := scrape(); !strings.Contains(text, "forge_db_pool_waits_total"+labels+"} 2") {
		t.Fatalf("the reopened pool's wait was not added:\n%s", text)
	}
}

// waitForTheOnlyConnection makes one query wait for the only connection of
// db, so the pool counts one wait.
func waitForTheOnlyConnection(t *testing.T, db *sqldb.DB) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.SQL().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waits := db.SQL().Stats().WaitCount
	done := make(chan error, 1)
	go func() {
		var one int
		done <- db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
	}()
	// database/sql counts the wait when the query starts waiting.
	for deadline := time.Now().Add(5 * time.Second); db.SQL().Stats().WaitCount == waits; {
		if time.Now().After(deadline) {
			t.Fatal("the query never waited for the connection")
		}
		time.Sleep(time.Millisecond)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
