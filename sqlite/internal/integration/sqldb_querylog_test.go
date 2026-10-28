package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
	"github.com/rocketinsidecorp/forge-go/telemetry"
	"github.com/rocketinsidecorp/forge-go/web"
)

// openLogging opens an instrumented SQLite pool whose statements are logged
// as JSON lines into the returned buffer.
func openLogging(t *testing.T, slow time.Duration) (*sqldb.DB, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tel, err := telemetry.New(telemetry.Options{
		ServiceName: "test",
		QueryLog:    &telemetry.QueryLog{Logger: logger, Level: slog.LevelInfo, Slow: slow},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	db, err := sqlite.Open(context.Background(), sqlitetest.Config(t), sqlite.WithTelemetry(tel))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, out
}

func decodeLines(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		lines = append(lines, record)
	}
	return lines
}

func TestObserverLogsStatementsNeverArguments(t *testing.T) {
	db, out := openLogging(t, time.Hour)
	ctx := web.WithRequestID(context.Background(), "req-9")
	if _, err := db.ExecContext(ctx, "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT UNIQUE NOT NULL, token TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO users (email, token) VALUES (?, ?)", "alice@example.com", "tok-secret-123"); err != nil {
		t.Fatal(err)
	}
	var id int
	if err := db.QueryRowContext(ctx, "SELECT id FROM users WHERE email = ? AND id > 0", "alice@example.com").Scan(&id); err != nil {
		t.Fatal(err)
	}
	stmt, err := db.PrepareContext(ctx, "DELETE FROM users WHERE token = ?")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stmt.ExecContext(ctx, "tok-secret-123"); err != nil {
		t.Fatal(err)
	}
	_ = stmt.Close()

	for _, secret := range []string{"alice@example.com", "tok-secret-123"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("an argument reached the log: %s", out.String())
		}
	}
	lines := decodeLines(t, out)
	var statements []string
	for _, line := range lines {
		if line["msg"] != telemetry.LogEventSQL || line["level"] != "INFO" || line["request_id"] != "req-9" {
			t.Fatalf("%v", line)
		}
		if _, ok := line["slow"]; ok {
			t.Fatalf("a fast statement is not slow: %v", line)
		}
		statements = append(statements, line["statement"].(string))
	}
	want := []string{
		"CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT UNIQUE NOT NULL, token TEXT)",
		"INSERT INTO users (email, token) VALUES (?, ?)",
		"SELECT id FROM users WHERE email = ? AND id > ?", // the literal 0 is masked
		"DELETE FROM users WHERE token = ?",
	}
	if strings.Join(statements, "\n") != strings.Join(want, "\n") {
		t.Fatalf("statements:\n%s\nwant\n%s", strings.Join(statements, "\n"), strings.Join(want, "\n"))
	}
}

func TestObserverFlagsSlowAndFailedStatements(t *testing.T) {
	// Every statement is slower than a nanosecond.
	db, out := openLogging(t, time.Nanosecond)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE t (a TEXT UNIQUE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES ('duplicate-value')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES ('duplicate-value')"); err == nil {
		t.Fatal("expected a unique violation")
	}
	lines := decodeLines(t, out)
	if len(lines) != 3 {
		t.Fatalf("%d lines: %s", len(lines), out.String())
	}
	for _, line := range lines {
		if line["slow"] != true {
			t.Errorf("not flagged slow: %v", line)
		}
		if _, ok := line["request_id"]; ok {
			t.Errorf("no request, no request_id: %v", line)
		}
	}
	// The class of the failure, never its message, which quotes the value.
	if lines[2]["error"] != "unique_violation" || lines[1]["error"] != nil || strings.Contains(out.String(), "duplicate-value") {
		t.Fatalf("%s", out.String())
	}
}

func TestObserverCountsStatementsPerRequest(t *testing.T) {
	db, _ := openLogging(t, time.Hour)
	ctx, counter := telemetry.WithQueryCounter(context.Background())
	if _, err := db.ExecContext(ctx, "CREATE TABLE t (a INTEGER)"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES (1)"); err != nil {
			t.Fatal(err)
		}
	}
	// Another request's context is not counted.
	other, otherCounter := telemetry.WithQueryCounter(context.Background())
	if err := db.QueryRowContext(other, "SELECT count(*) FROM t").Scan(new(int)); err != nil {
		t.Fatal(err)
	}
	if counter.Count() != 4 || otherCounter.Count() != 1 {
		t.Fatalf("counts %d and %d", counter.Count(), otherCounter.Count())
	}
}
