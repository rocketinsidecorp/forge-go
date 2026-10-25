package sqldb_test

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"testing"

	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/telemetry/telemetrytest"
)

// bareConn implements only driver.Conn, as old drivers do, so the wrappers
// have to supply what database/sql would have done without the optional
// interfaces.
type bareConn struct {
	begun    int
	prepared []string
	stmt     *bareStmt
	fail     error
}

func (c *bareConn) Prepare(query string) (driver.Stmt, error) {
	if c.fail != nil {
		return nil, c.fail
	}
	c.prepared = append(c.prepared, query)
	c.stmt = &bareStmt{}
	return c.stmt, nil
}
func (c *bareConn) Close() error              { return nil }
func (c *bareConn) Begin() (driver.Tx, error) { c.begun++; return skipTx{}, nil }

type bareStmt struct {
	closed bool
	execs  [][]driver.Value
}

func (s *bareStmt) Close() error  { s.closed = true; return nil }
func (s *bareStmt) NumInput() int { return 2 }
func (s *bareStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.execs = append(s.execs, args)
	return driver.RowsAffected(7), nil
}
func (s *bareStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.execs = append(s.execs, args)
	return emptyRows{}, nil
}

type bareConnector struct {
	conn *bareConn
	err  error
}

func (c bareConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, c.err }
func (c bareConnector) Driver() driver.Driver                        { return &skipDriver{} }

// convertingStmt adds the deprecated ColumnConverter.
type convertingStmt struct{ bareStmt }

type upperConverter struct{}

func (upperConverter) ConvertValue(v any) (driver.Value, error) { return v, nil }

func (*convertingStmt) ColumnConverter(int) driver.ValueConverter { return upperConverter{} }

type convertingConn struct{ bareConn }

func (c *convertingConn) Prepare(string) (driver.Stmt, error) { return &convertingStmt{}, nil }

// checkingStmt answers CheckNamedValue itself.
type checkingStmt struct{ bareStmt }

func (*checkingStmt) CheckNamedValue(v *driver.NamedValue) error {
	if s, ok := v.Value.(string); ok && s == "stmt-rejects" {
		return errors.New("statement checker")
	}
	return nil
}

type checkingConn struct{ bareConn }

func (c *checkingConn) Prepare(string) (driver.Stmt, error) { return &checkingStmt{}, nil }

// ctxStmt implements the context-aware statement interfaces.
type ctxStmt struct {
	bareStmt
	queried, executed int
}

func (s *ctxStmt) ExecContext(context.Context, []driver.NamedValue) (driver.Result, error) {
	s.executed++
	return driver.RowsAffected(3), nil
}
func (s *ctxStmt) QueryContext(context.Context, []driver.NamedValue) (driver.Rows, error) {
	s.queried++
	return emptyRows{}, nil
}

type ctxConn struct {
	bareConn
	stmt *ctxStmt
}

func (c *ctxConn) Prepare(string) (driver.Stmt, error) { c.stmt = &ctxStmt{}; return c.stmt, nil }

type connectorOf struct{ conn driver.Conn }

func (c connectorOf) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (c connectorOf) Driver() driver.Driver                        { return &skipDriver{} }

func instrument(t *testing.T, base driver.Connector) (driver.Conn, func() map[string]float64) {
	t.Helper()
	tel, _ := telemetrytest.New(t)
	observer, err := sqldb.NewObserver(tel, "mysql", func(error) sqldb.Classification { return sqldb.Classification{} })
	if err != nil {
		t.Fatal(err)
	}
	wrapped := sqldb.InstrumentConnector(base, observer)
	conn, err := wrapped.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if wrapped.Driver() == nil {
		t.Error("the wrapped connector lost its driver")
	}
	return conn, func() map[string]float64 { return telemetrytest.Series(tel, sqldb.MetricDBOperationDuration) }
}

func TestInstrumentConnectorReturnsTheBaseWithoutAnObserver(t *testing.T) {
	t.Parallel()
	base := bareConnector{conn: &bareConn{}}
	if got := sqldb.InstrumentConnector(base, nil); got != driver.Connector(base) {
		t.Errorf("a nil observer must leave the connector alone, got %T", got)
	}
}

func TestInstrumentedConnectorPropagatesAConnectFailure(t *testing.T) {
	t.Parallel()
	tel, _ := telemetrytest.New(t)
	observer, err := sqldb.NewObserver(tel, "mysql", func(error) sqldb.Classification { return sqldb.Classification{} })
	if err != nil {
		t.Fatal(err)
	}
	down := errors.New("connection refused")
	conn, err := sqldb.InstrumentConnector(bareConnector{err: down}, observer).Connect(context.Background())
	if !errors.Is(err, down) || conn != nil {
		t.Fatalf("conn=%v err=%v", conn, err)
	}
}

func TestInstrumentedConnWithoutOptionalInterfacesBehavesLikeDatabaseSQL(t *testing.T) {
	t.Parallel()
	base := &bareConn{}
	conn, series := instrument(t, bareConnector{conn: base})

	// Prepare without a context goes through PrepareContext to Prepare.
	stmt, err := conn.Prepare("UPDATE t SET a = ?")
	if err != nil {
		t.Fatal(err)
	}
	if len(base.prepared) != 1 || stmt.NumInput() != 2 {
		t.Fatalf("prepared %v, NumInput %d", base.prepared, stmt.NumInput())
	}
	result, err := stmt.Exec([]driver.Value{int64(1), "x"})
	if n, _ := result.RowsAffected(); err != nil || n != 7 {
		t.Fatalf("Exec = %v, %v", n, err)
	}
	rows, err := stmt.Query([]driver.Value{int64(2)})
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if got := len(base.stmt.execs); got != 2 {
		t.Fatalf("the base statement saw %d calls", got)
	}
	// The context-aware statement paths fall back to the legacy methods
	// with the arguments converted, and record the statement.
	execer := stmt.(driver.StmtExecContext)
	if _, err := execer.ExecContext(context.Background(), []driver.NamedValue{{Ordinal: 1, Value: int64(5)}}); err != nil {
		t.Fatal(err)
	}
	queryer := stmt.(driver.StmtQueryContext)
	rows, err = queryer.QueryContext(context.Background(), []driver.NamedValue{{Ordinal: 1, Value: int64(6)}})
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if last := base.stmt.execs[len(base.stmt.execs)-1]; len(last) != 1 || last[0] != int64(6) {
		t.Errorf("converted arguments = %v", last)
	}
	// Named parameters cannot be passed to a legacy driver.
	named := []driver.NamedValue{{Name: "id", Ordinal: 1, Value: int64(1)}}
	if _, err := execer.ExecContext(context.Background(), named); err == nil {
		t.Error("named parameters reached a driver that cannot take them (Exec)")
	}
	if _, err := queryer.QueryContext(context.Background(), named); err == nil {
		t.Error("named parameters reached a driver that cannot take them (Query)")
	}
	// A canceled context stops the statement before it reaches the driver.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	before := len(base.stmt.execs)
	if _, err := execer.ExecContext(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled Exec = %v", err)
	}
	if _, err := queryer.QueryContext(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled Query = %v", err)
	}
	if len(base.stmt.execs) != before {
		t.Error("a canceled statement reached the driver")
	}
	if err := stmt.Close(); err != nil || !base.stmt.closed {
		t.Errorf("Close: %v closed=%v", err, base.stmt.closed)
	}

	// Without ExecerContext/QueryerContext the connection tells database/sql
	// to prepare instead, and nothing is measured for that attempt.
	if _, err := conn.(driver.ExecerContext).ExecContext(context.Background(), "UPDATE t SET a = 1", nil); !errors.Is(err, driver.ErrSkip) {
		t.Errorf("ExecContext = %v, want ErrSkip", err)
	}
	if _, err := conn.(driver.QueryerContext).QueryContext(context.Background(), "SELECT 1", nil); !errors.Is(err, driver.ErrSkip) {
		t.Errorf("QueryContext = %v, want ErrSkip", err)
	}
	// Optional connection interfaces fall back to their defaults.
	if err := conn.(driver.Pinger).Ping(context.Background()); err != nil {
		t.Errorf("Ping = %v", err)
	}
	if err := conn.(driver.SessionResetter).ResetSession(context.Background()); err != nil {
		t.Errorf("ResetSession = %v", err)
	}
	if !conn.(driver.Validator).IsValid() {
		t.Error("a connection without a validator must be valid")
	}
	if err := conn.(driver.NamedValueChecker).CheckNamedValue(&driver.NamedValue{Value: 1}); !errors.Is(err, driver.ErrSkip) {
		t.Errorf("CheckNamedValue = %v, want ErrSkip", err)
	}
	if err := stmt.(driver.NamedValueChecker).CheckNamedValue(&driver.NamedValue{Value: 1}); !errors.Is(err, driver.ErrSkip) {
		t.Errorf("statement CheckNamedValue = %v, want ErrSkip", err)
	}

	// Each statement call on the prepared UPDATE was measured once: two
	// that ran, two refused for named parameters (an error the driver
	// classification does not know) and two stopped by the context.
	got := series()
	for errorType, want := range map[string]float64{"": 2, "other": 2, "canceled": 2} {
		key := "db.system.name=mysql,db.operation.name=UPDATE,error.type=" + errorType
		if got[key] != want {
			t.Errorf("%s measured %v times, want %v (all: %v)", key, got[key], want, got)
		}
	}
	if err := conn.Close(); err != nil {
		t.Error(err)
	}
}

func TestInstrumentedConnTransactionsFollowDatabaseSQLRules(t *testing.T) {
	t.Parallel()
	base := &bareConn{}
	conn, _ := instrument(t, bareConnector{conn: base})
	begin := conn.(driver.ConnBeginTx)

	if _, err := begin.BeginTx(context.Background(), driver.TxOptions{Isolation: driver.IsolationLevel(6)}); err == nil {
		t.Error("an isolation level was accepted by a driver without ConnBeginTx")
	}
	if _, err := begin.BeginTx(context.Background(), driver.TxOptions{ReadOnly: true}); err == nil {
		t.Error("a read-only transaction was accepted by a driver without ConnBeginTx")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := begin.BeginTx(canceled, driver.TxOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled BeginTx = %v", err)
	}
	if base.begun != 0 {
		t.Fatalf("%d transactions began despite the refusals", base.begun)
	}
	if _, err := begin.BeginTx(context.Background(), driver.TxOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Begin(); err != nil {
		t.Fatal(err)
	}
	if base.begun != 2 {
		t.Errorf("began %d transactions, want 2", base.begun)
	}
}

func TestInstrumentedConnPrepareFailuresAndCancellation(t *testing.T) {
	t.Parallel()
	broken := errors.New("syntax error")
	base := &bareConn{fail: broken}
	conn, _ := instrument(t, bareConnector{conn: base})
	if _, err := conn.Prepare("SELEC"); !errors.Is(err, broken) {
		t.Fatalf("Prepare = %v", err)
	}
	base.fail = nil
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := conn.(driver.ConnPrepareContext).PrepareContext(canceled, "SELECT 1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled PrepareContext = %v", err)
	}
	if base.stmt == nil || !base.stmt.closed {
		t.Error("the statement prepared for a canceled context was not closed")
	}
}

func TestInstrumentedStatementsKeepTheDriversConverterAndChecker(t *testing.T) {
	t.Parallel()
	conn, _ := instrument(t, connectorOf{conn: &convertingConn{}})
	stmt, err := conn.Prepare("SELECT ?")
	if err != nil {
		t.Fatal(err)
	}
	converter, ok := stmt.(driver.ColumnConverter) //nolint:staticcheck // the interface under test
	if !ok {
		t.Fatalf("%T dropped the driver's ColumnConverter", stmt)
	}
	if _, ok := converter.ColumnConverter(0).(upperConverter); !ok {
		t.Errorf("ColumnConverter = %T, want the driver's", converter.ColumnConverter(0))
	}

	conn, _ = instrument(t, connectorOf{conn: &checkingConn{}})
	stmt, err = conn.Prepare("SELECT ?")
	if err != nil {
		t.Fatal(err)
	}
	checker := stmt.(driver.NamedValueChecker)
	if err := checker.CheckNamedValue(&driver.NamedValue{Value: "stmt-rejects"}); err == nil {
		t.Error("the statement's own checker did not run")
	}
	if err := checker.CheckNamedValue(&driver.NamedValue{Value: "fine"}); err != nil {
		t.Errorf("CheckNamedValue = %v", err)
	}
}

func TestInstrumentedStatementsUseTheContextAwareDriverMethods(t *testing.T) {
	t.Parallel()
	base := &ctxConn{}
	conn, series := instrument(t, connectorOf{conn: base})
	stmt, err := conn.Prepare("DELETE FROM t WHERE a = ?")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stmt.(driver.StmtExecContext).ExecContext(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	rows, err := stmt.(driver.StmtQueryContext).QueryContext(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rows.Next(make([]driver.Value, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("rows = %v", err)
	}
	if base.stmt.executed != 1 || base.stmt.queried != 1 {
		t.Errorf("driver saw %d executions and %d queries", base.stmt.executed, base.stmt.queried)
	}
	if got := series()["db.system.name=mysql,db.operation.name=DELETE,error.type="]; got < 2 {
		t.Errorf("DELETE measured %v times, want 2", got)
	}
}
