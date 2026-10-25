package sqlite

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"
)

var errScripted = errors.New("scripted failure")

// scriptedConn is a driver connection whose PRAGMA query_only statements and
// transaction start can fail on demand.
type scriptedConn struct {
	mu          sync.Mutex
	statements  []string
	enterErr    error // PRAGMA query_only = 1
	leaveErr    error // PRAGMA query_only = 0
	beginErr    error
	began       []driver.TxOptions
	resetCalled bool
}

type scriptedTx struct{}

func (scriptedTx) Commit() error   { return nil }
func (scriptedTx) Rollback() error { return nil }

func (c *scriptedConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *scriptedConn) Close() error                        { return nil }
func (c *scriptedConn) Begin() (driver.Tx, error)           { return scriptedTx{}, nil }
func (c *scriptedConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.began = append(c.began, opts)
	if c.beginErr != nil {
		return nil, c.beginErr
	}
	return scriptedTx{}, nil
}
func (c *scriptedConn) PrepareContext(context.Context, string) (driver.Stmt, error) {
	return nil, errors.New("unused")
}
func (c *scriptedConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statements = append(c.statements, query)
	switch query {
	case "PRAGMA query_only = 1":
		return driver.RowsAffected(0), c.enterErr
	case "PRAGMA query_only = 0":
		return driver.RowsAffected(0), c.leaveErr
	}
	return driver.RowsAffected(0), nil
}
func (c *scriptedConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return nil, errors.New("unused")
}
func (c *scriptedConn) Ping(context.Context) error { return nil }
func (c *scriptedConn) ResetSession(context.Context) error {
	c.resetCalled = true
	return nil
}
func (c *scriptedConn) IsValid() bool { return true }

func wrap(raw *scriptedConn) *conn { return &conn{baseConn: raw} }

func TestReadOnlyTransactionsLeaveQueryOnlyModeWhenTheyCannotStart(t *testing.T) {
	t.Parallel()
	readOnly := driver.TxOptions{ReadOnly: true}

	// The transaction cannot start: the connection must not stay read-only.
	raw := &scriptedConn{beginErr: errScripted}
	c := wrap(raw)
	if _, err := c.BeginTx(context.Background(), readOnly); !errors.Is(err, errScripted) {
		t.Fatalf("BeginTx = %v", err)
	}
	if got := strings.Join(raw.statements, "; "); got != "PRAGMA query_only = 1; PRAGMA query_only = 0" {
		t.Errorf("statements = %q, want enter then leave", got)
	}
	if c.broken.Load() || !c.IsValid() {
		t.Error("a connection that left read-only mode cleanly was discarded")
	}
	if err := c.ResetSession(context.Background()); err != nil || !raw.resetCalled {
		t.Errorf("ResetSession = %v (reset reached the driver: %v)", err, raw.resetCalled)
	}

	// Entering read-only mode fails, and so does leaving it: the connection
	// may still be read-only, so it is discarded rather than reused.
	raw = &scriptedConn{enterErr: errScripted, leaveErr: errors.New("leave failed")}
	c = wrap(raw)
	_, err := c.BeginTx(context.Background(), readOnly)
	if !errors.Is(err, errScripted) || !strings.Contains(err.Error(), "leave read-only mode") {
		t.Fatalf("BeginTx = %v, want both failures", err)
	}
	if !c.broken.Load() || c.IsValid() {
		t.Error("a connection that may still be read-only is still valid")
	}
	if err := c.ResetSession(context.Background()); !errors.Is(err, driver.ErrBadConn) {
		t.Errorf("ResetSession = %v, want driver.ErrBadConn so database/sql drops it", err)
	}
	if len(raw.began) != 0 {
		t.Error("a transaction began on a connection whose read-only switch failed")
	}
}

func TestLegacyBeginStartsAWritableTransaction(t *testing.T) {
	t.Parallel()
	raw := &scriptedConn{}
	tx, err := wrap(raw).Begin()
	if err != nil || tx == nil {
		t.Fatalf("Begin = %v, %v", tx, err)
	}
	if len(raw.began) != 1 || raw.began[0].ReadOnly {
		t.Errorf("began %+v, want one writable transaction", raw.began)
	}
	if len(raw.statements) != 0 {
		t.Errorf("a writable transaction ran %v", raw.statements)
	}
}

type fakeConnector struct {
	conn driver.Conn
	err  error
}

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, c.err }
func (c fakeConnector) Driver() driver.Driver                        { return nil }

// plainConn lacks the interfaces of the real driver's connections.
type plainConn struct{ closed bool }

func (c *plainConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *plainConn) Close() error                        { c.closed = true; return nil }
func (c *plainConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }

func TestConnectorRejectsAndClosesAnUnexpectedDriverConnection(t *testing.T) {
	t.Parallel()
	plain := &plainConn{}
	got, err := (connector{base: fakeConnector{conn: plain}}).Connect(context.Background())
	if err == nil || got != nil || !strings.Contains(err.Error(), "lacks the expected interfaces") || !plain.closed {
		t.Fatalf("Connect = %v, %v (closed=%v)", got, err, plain.closed)
	}
	if conn, err := (connector{base: fakeConnector{err: errScripted}}).Connect(context.Background()); !errors.Is(err, errScripted) || conn != nil {
		t.Errorf("a failing driver gave %v, %v", conn, err)
	}
	if (connector{base: fakeConnector{}}).Driver() != nil {
		t.Error("the connector must report its base driver")
	}
}

func TestFileURIAnchorsRelativePathsAndEncodesTheName(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{
		"/data/app.db":   "file:///data/app.db",
		"data/app.db":    "file:///data/app.db",
		"/a?b#c/app.db":  "file:///a%3Fb%23c/app.db",
		"/space dir/x.d": "file:///space%20dir/x.d",
	} {
		if got := fileURI(path, nil); got != want {
			t.Errorf("fileURI(%q) = %q, want %q", path, got, want)
		}
	}
}
