package sqlite_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

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
	done := make(chan error, 1)
	go func() {
		var one int
		done <- db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
	}()
	time.Sleep(50 * time.Millisecond)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
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
