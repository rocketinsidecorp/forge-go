package mysql

import (
	"context"
	sqldriver "database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

var errScripted = errors.New("scripted failure")

type versionRows struct {
	columns []string
	value   sqldriver.Value
	err     error
}

func (r *versionRows) Columns() []string { return r.columns }
func (r *versionRows) Close() error      { return nil }
func (r *versionRows) Next(dest []sqldriver.Value) error {
	if r.err != nil {
		return r.err
	}
	dest[0] = r.value
	return nil
}

// sessionConn can query and exec, as the real driver connection does.
type sessionConn struct {
	rows     *versionRows
	queryErr error
	execErr  error
	executed []string
	closed   bool
}

func (c *sessionConn) Prepare(string) (sqldriver.Stmt, error) { return nil, errors.New("unused") }
func (c *sessionConn) Close() error                           { c.closed = true; return nil }
func (c *sessionConn) Begin() (sqldriver.Tx, error)           { return nil, errors.New("unused") }
func (c *sessionConn) QueryContext(context.Context, string, []sqldriver.NamedValue) (sqldriver.Rows, error) {
	if c.queryErr != nil {
		return nil, c.queryErr
	}
	return c.rows, nil
}
func (c *sessionConn) ExecContext(_ context.Context, query string, _ []sqldriver.NamedValue) (sqldriver.Result, error) {
	c.executed = append(c.executed, query)
	return sqldriver.RowsAffected(0), c.execErr
}

// queryOnlyConn lacks ExecerContext.
type queryOnlyConn struct{ sessionConn }

func (queryOnlyConn) ExecContext() {}

// noQueryConn implements neither optional interface.
type noQueryConn struct{}

func (noQueryConn) Prepare(string) (sqldriver.Stmt, error) { return nil, errors.New("unused") }
func (noQueryConn) Close() error                           { return nil }
func (noQueryConn) Begin() (sqldriver.Tx, error)           { return nil, errors.New("unused") }

type fakeConnector struct {
	conn sqldriver.Conn
	err  error
}

func (c fakeConnector) Connect(context.Context) (sqldriver.Conn, error) { return c.conn, c.err }
func (c fakeConnector) Driver() sqldriver.Driver                        { return nil }

func oneColumn(value sqldriver.Value) *versionRows {
	return &versionRows{columns: []string{"VERSION()"}, value: value}
}

func TestConnectorConfiguresEverySessionBeforeHandingItOut(t *testing.T) {
	t.Parallel()
	for name, value := range map[string]sqldriver.Value{
		"bytes (the wire format)": []byte("8.4.5"),
		"text":                    "10.11.6-MariaDB",
	} {
		conn := &sessionConn{rows: oneColumn(value)}
		c := &connector{base: fakeConnector{conn: conn}, statementTimeout: 2 * time.Second}
		got, err := c.Connect(context.Background())
		if err != nil || got != sqldriver.Conn(conn) {
			t.Fatalf("%s: Connect = %v, %v", name, got, err)
		}
		if len(conn.executed) != 1 || !strings.HasPrefix(conn.executed[0], "SET NAMES utf8mb4") || conn.closed {
			t.Errorf("%s: session setup = %v (closed=%v)", name, conn.executed, conn.closed)
		}
	}
	// MariaDB and MySQL name the statement timeout differently.
	maria := &sessionConn{rows: oneColumn("10.11.6-MariaDB")}
	if _, err := (&connector{base: fakeConnector{conn: maria}, statementTimeout: time.Second}).Connect(context.Background()); err != nil || !strings.Contains(maria.executed[0], "max_statement_time") {
		t.Errorf("MariaDB setup = %v, %v", maria.executed, err)
	}
	if (&connector{base: fakeConnector{}}).Driver() != nil {
		t.Error("the connector's driver is the base connector's")
	}
}

func TestConnectorClosesAConnectionItCannotConfigure(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		conn sqldriver.Conn
		want string
	}{
		"version query fails":     {&sessionConn{queryErr: errScripted}, "scripted failure"},
		"setup statement fails":   {&sessionConn{rows: oneColumn("8.4.5"), execErr: errScripted}, "scripted failure"},
		"several columns":         {&sessionConn{rows: &versionRows{columns: []string{"a", "b"}}}, "unexpected server version result"},
		"no row":                  {&sessionConn{rows: &versionRows{columns: []string{"v"}, err: io.EOF}}, "did not report its version"},
		"row read fails":          {&sessionConn{rows: &versionRows{columns: []string{"v"}, err: errScripted}}, "scripted failure"},
		"version of a wrong type": {&sessionConn{rows: oneColumn(int64(8))}, "unexpected server version result"},
		"driver cannot exec":      {&queryOnlyConn{}, "cannot run queries"},
		"driver cannot query":     {noQueryConn{}, "cannot run queries"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := (&connector{base: fakeConnector{conn: tc.conn}}).Connect(context.Background())
			if err == nil || got != nil || !strings.Contains(err.Error(), "configure database session") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Connect = %v, %v; want %q", got, err, tc.want)
			}
			switch conn := tc.conn.(type) {
			case *sessionConn:
				if !conn.closed {
					t.Error("the half-configured connection was handed out or left open")
				}
			case *queryOnlyConn:
				if !conn.closed {
					t.Error("the unusable connection was left open")
				}
			}
		})
	}
	down := &connector{base: fakeConnector{err: errScripted}}
	if conn, err := down.Connect(context.Background()); !errors.Is(err, errScripted) || conn != nil {
		t.Errorf("a failing base connector gave %v, %v", conn, err)
	}
}
