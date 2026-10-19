package sqldb_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/telemetry"
	"github.com/rocketinsidecorp/forge-go/telemetry/telemetrytest"
)

// skipDriver behaves like go-sql-driver/mysql without interpolateParams: a
// statement with arguments answers driver.ErrSkip, so database/sql prepares
// it; values are checked by the connection's NamedValueChecker, which
// records what it saw.
type skipDriver struct {
	mu      sync.Mutex
	checked int
	execs   []string
}

func (d *skipDriver) Open(string) (driver.Conn, error) { return &skipConn{driver: d}, nil }

type skipConnector struct{ driver *skipDriver }

func (c skipConnector) Connect(context.Context) (driver.Conn, error) {
	return &skipConn{driver: c.driver}, nil
}
func (c skipConnector) Driver() driver.Driver { return c.driver }

type skipConn struct{ driver *skipDriver }

func (c *skipConn) Prepare(query string) (driver.Stmt, error) {
	return &skipStmt{conn: c, query: query}, nil
}
func (c *skipConn) Close() error              { return nil }
func (c *skipConn) Begin() (driver.Tx, error) { return skipTx{}, nil }
func (c *skipConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if len(args) > 0 {
		return nil, driver.ErrSkip
	}
	c.record("direct:" + query)
	return driver.RowsAffected(0), nil
}

func (c *skipConn) CheckNamedValue(value *driver.NamedValue) error {
	c.driver.mu.Lock()
	c.driver.checked++
	c.driver.mu.Unlock()
	if s, ok := value.Value.(string); ok && s == "reject" {
		return errors.New("rejected by the driver's checker")
	}
	return driver.ErrSkip
}

func (c *skipConn) record(entry string) {
	c.driver.mu.Lock()
	defer c.driver.mu.Unlock()
	c.driver.execs = append(c.driver.execs, entry)
}

type skipStmt struct {
	conn  *skipConn
	query string
}

func (s *skipStmt) Close() error  { return nil }
func (s *skipStmt) NumInput() int { return -1 }
func (s *skipStmt) Exec([]driver.Value) (driver.Result, error) {
	s.conn.record("prepared:" + s.query)
	return driver.RowsAffected(1), nil
}
func (s *skipStmt) Query([]driver.Value) (driver.Rows, error) { return emptyRows{}, nil }

type skipTx struct{}

func (skipTx) Commit() error   { return nil }
func (skipTx) Rollback() error { return nil }

type emptyRows struct{}

func (emptyRows) Columns() []string         { return []string{"x"} }
func (emptyRows) Close() error              { return nil }
func (emptyRows) Next([]driver.Value) error { return io.EOF }

func TestInstrumentedConnectorFollowsDatabaseSQLFallbacks(t *testing.T) {
	tel, recorder := telemetrytest.New(t)
	observer, err := sqldb.NewObserver(tel, "mysql", func(error) sqldb.Classification { return sqldb.Classification{} })
	if err != nil {
		t.Fatal(err)
	}
	fake := &skipDriver{}
	db := sql.OpenDB(sqldb.InstrumentConnector(skipConnector{driver: fake}, observer))
	defer func() { _ = db.Close() }()

	ctx, parent := tel.Tracer("test").Start(context.Background(), "request", telemetry.SpanKindServer)
	if _, err := db.ExecContext(ctx, "UPDATE t SET a = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE t SET a = ?", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE t SET a = ?", "reject"); err == nil {
		t.Fatal("the connection's value checker must still run for prepared statements")
	}
	rows, err := db.QueryContext(ctx, "SELECT x FROM t WHERE a = ?", 3)
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err == nil {
		_ = tx.Rollback()
		t.Fatal("a driver without ConnBeginTx cannot start read-only transactions")
	}
	parent.End()

	fake.mu.Lock()
	execs, checked := append([]string(nil), fake.execs...), fake.checked
	fake.mu.Unlock()
	if len(execs) != 2 || execs[0] != "direct:UPDATE t SET a = 1" || execs[1] != "prepared:UPDATE t SET a = ?" {
		t.Fatalf("driver saw %v", execs)
	}
	if checked < 3 {
		t.Fatalf("connection value checker ran %d times", checked)
	}
	var names []string
	for _, span := range recorder.Spans(t, tel) {
		names = append(names, span.Name)
	}
	// The ErrSkip attempts leave no spans; the prepared executions do.
	if got := len(names); got != 4 || names[0] != "UPDATE" || names[1] != "UPDATE" || names[2] != "SELECT" {
		t.Fatalf("spans %v", names)
	}
}
