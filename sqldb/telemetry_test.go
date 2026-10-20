package sqldb_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
	"github.com/rocketinsidecorp/forge-go/telemetry"
	"github.com/rocketinsidecorp/forge-go/telemetry/telemetrytest"
)

func TestOperation(t *testing.T) {
	for query, want := range map[string]string{
		"SELECT 1":                          "SELECT",
		"  select * from users":             "SELECT",
		"\n\tInsert INTO t VALUES (1)":      "INSERT",
		"-- comment\nUPDATE t SET a = 1":    "UPDATE",
		"/* hint */ DELETE FROM t":          "DELETE",
		"(SELECT 1) UNION (SELECT 2)":       "SELECT",
		"WITH x AS (SELECT 1) SELECT 1":     "WITH",
		"secret_function('password')":       "OTHER",
		"selectx":                           "OTHER",
		"":                                  "OTHER",
		"-- ping":                           "OTHER",
		"/* unterminated":                   "OTHER",
		"PRAGMA foreign_keys":               "PRAGMA",
		"SELECTSELECTSELECTSELECT password": "OTHER",
	} {
		if got := sqldb.Operation(query); got != want {
			t.Errorf("Operation(%q) = %q, want %q", query, got, want)
		}
	}
	allocs := testing.AllocsPerRun(100, func() { _ = sqldb.Operation("/* c */ select 1") })
	if allocs != 0 {
		t.Fatalf("Operation allocated %v times", allocs)
	}
}

func TestNilObserverIsANoOp(t *testing.T) {
	observer, err := sqldb.NewObserver(nil, "sqlite", func(error) sqldb.Classification { return sqldb.Classification{} })
	if observer != nil || err != nil {
		t.Fatal("a nil Telemetry gives a nil observer")
	}
	observer.Start(context.Background(), "SELECT 1").End(errors.New("x"))
	if sqldb.KindUniqueViolation.String() != "unique_violation" || sqldb.KindUnknown.String() != "other" {
		t.Fatal("kind names")
	}
}

func openInstrumented(t *testing.T) (*sqldb.DB, *telemetry.Telemetry, *telemetrytest.Recorder) {
	t.Helper()
	tel, recorder := telemetrytest.New(t)
	db, err := sqlite.Open(context.Background(), sqlitetest.Config(t), sqlite.WithTelemetry(tel))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, tel, recorder
}

func TestInstrumentedPoolTracesStatementsWithoutText(t *testing.T) {
	db, tel, recorder := openInstrumented(t)
	background := context.Background()
	// Outside a trace: measured, but no root span.
	if _, err := db.ExecContext(background, "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT UNIQUE NOT NULL)"); err != nil {
		t.Fatal(err)
	}

	ctx, request := tel.Tracer("test").Start(background, "request", telemetry.SpanKindServer)
	if _, err := db.ExecContext(ctx, "INSERT INTO users (email) VALUES (?)", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	var email string
	if err := db.QueryRowContext(ctx, "SELECT email FROM users WHERE id = ?", 1).Scan(&email); err != nil {
		t.Fatal(err)
	}
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE users SET email = ? WHERE id = ?", "bob@example.com", 1)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := db.PrepareContext(ctx, "DELETE FROM users WHERE id = ?")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stmt.ExecContext(ctx, 42); err != nil {
		t.Fatal(err)
	}
	_ = stmt.Close()
	_, duplicate := db.ExecContext(ctx, "INSERT INTO users (email) VALUES (?), (?)", "x@example.com", "x@example.com")
	if duplicate == nil {
		t.Fatal("expected a unique violation")
	}
	request.End()

	spans := recorder.Spans(t, tel)
	var names []string
	for _, span := range spans {
		if span.Name == "request" {
			continue
		}
		names = append(names, span.Name)
		if span.ParentSpanID != request.SpanContext().SpanID || span.Kind != telemetry.SpanKindClient {
			t.Fatalf("statement span %s is not a client child of the request", span.Name)
		}
		if system, _ := telemetrytest.Attribute(span, "db.system.name"); system != "sqlite" {
			t.Fatalf("db.system.name = %q", system)
		}
		for _, attr := range span.Attributes {
			if text := attr.Value.Emit(); strings.Contains(text, "example.com") || strings.Contains(text, "users") {
				t.Fatalf("statement text or argument recorded: %s=%s", attr.Key, text)
			}
		}
	}
	if got := strings.Join(names, ","); got != "INSERT,SELECT,UPDATE,DELETE,INSERT" {
		t.Fatalf("statement spans %s", got)
	}
	last := spans[len(spans)-2]
	if errorType, _ := telemetrytest.Attribute(last, "error.type"); errorType != "unique_violation" || last.Status != telemetry.StatusError {
		t.Fatalf("failed statement span %+v", last)
	}

	series := telemetrytest.Series(tel, sqldb.MetricDBOperationDuration)
	for key, want := range map[string]float64{
		"db.system.name=sqlite,db.operation.name=CREATE,error.type=":                 1,
		"db.system.name=sqlite,db.operation.name=INSERT,error.type=":                 1,
		"db.system.name=sqlite,db.operation.name=INSERT,error.type=unique_violation": 1,
		"db.system.name=sqlite,db.operation.name=SELECT,error.type=":                 1,
	} {
		if series[key] < want {
			t.Errorf("series %s = %v; all %v", key, series[key], series)
		}
	}
}

func TestInstrumentationPreservesTransactionSemantics(t *testing.T) {
	db, _, _ := openInstrumented(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE t (v INTEGER)"); err != nil {
		t.Fatal(err)
	}
	err := db.InTxWith(ctx, sqldb.TxOptions{ReadOnly: true}, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO t VALUES (1)")
		return err
	})
	if err == nil {
		t.Fatal("read-only transactions must still reject writes through the wrapper")
	}
	err = db.InTxWith(ctx, sqldb.TxOptions{Isolation: sql.LevelReadCommitted}, func(*sql.Tx) error { return nil })
	if !errors.Is(err, sqlite.ErrIsolationLevel) {
		t.Fatalf("isolation errors must pass through: %v", err)
	}
	if err := db.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

// ObservePool may be called again for a DB, concurrently with itself and
// with Close: the earlier registration is stopped, a closed DB is refused,
// and Close stops whatever registration won.
func TestObservePoolAgainStopsTheEarlierRegistration(t *testing.T) {
	tel, _ := telemetrytest.New(t)
	observer, err := sqldb.NewObserver(tel, "sqlite", func(error) sqldb.Classification { return sqldb.Classification{} })
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(context.Background(), sqlitetest.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	maxOf := func(name string) (float64, bool) {
		value, ok := telemetrytest.Series(tel, telemetry.MetricDBConnectionMax)["db.system.name=sqlite,db.client.connection.pool.name="+name]
		return value, ok
	}
	if err := observer.ObservePool("first", db); err != nil {
		t.Fatal(err)
	}
	if limit, ok := maxOf("first"); !ok || limit == 0 {
		t.Fatalf("first pool max %v %t", limit, ok)
	}
	if err := observer.ObservePool("second", db); err != nil {
		t.Fatal(err)
	}
	if limit, _ := maxOf("first"); limit != 0 {
		t.Fatalf("the earlier registration still reads the pool: max %v", limit)
	}
	if limit, ok := maxOf("second"); !ok || limit == 0 {
		t.Fatalf("second pool max %v %t", limit, ok)
	}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			_ = observer.ObservePool(fmt.Sprintf("racer%d", i), db)
		})
	}
	wg.Go(func() { _ = db.Close() })
	wg.Wait()
	for key, value := range telemetrytest.Series(tel, telemetry.MetricDBConnectionMax) {
		if value != 0 {
			t.Fatalf("a closed DB is still observed: %s = %v", key, value)
		}
	}
	if err := observer.ObservePool("late", db); err == nil {
		t.Fatal("a closed DB must not be observed")
	}
	if err := observer.ObservePool("nil", nil); err == nil {
		t.Fatal("a nil DB must be refused")
	}
}
