package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync/atomic"
)

// connector wraps the driver's connections so that transaction options mean
// on SQLite what they mean on the other databases Forge supports.
type connector struct {
	base driver.Connector
}

func (c connector) Connect(ctx context.Context) (driver.Conn, error) {
	raw, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	base, ok := raw.(baseConn)
	if !ok {
		_ = raw.Close()
		return nil, fmt.Errorf("sqlite: driver connection %T lacks the expected interfaces", raw)
	}
	return &conn{baseConn: base}, nil
}

func (c connector) Driver() driver.Driver { return c.base.Driver() }

// baseConn is what modernc.org/sqlite connections implement. Embedding it
// forwards every method, so database/sql keeps its fast paths.
type baseConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

// conn enforces two transaction options the driver ignores:
//
//   - ReadOnly: SQLite has no read-only transactions, so the connection is
//     switched to PRAGMA query_only for the transaction's duration, and a
//     write fails with SQLITE_READONLY as it would fail on PostgreSQL or
//     MySQL. query_only is reset when the transaction ends; a connection
//     whose reset failed is discarded, never returned to the pool.
//   - Isolation: every SQLite transaction is serializable. Default and
//     Serializable are accepted; weaker or other levels are rejected rather
//     than silently upgraded, so code that depends on a specific level
//     states it truthfully.
type conn struct {
	baseConn
	// broken marks a connection that may still be in query_only mode.
	broken atomic.Bool
}

var (
	_ driver.ConnBeginTx        = (*conn)(nil)
	_ driver.SessionResetter    = (*conn)(nil)
	_ driver.Validator          = (*conn)(nil)
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.Pinger             = (*conn)(nil)
	_ driver.ConnPrepareContext = (*conn)(nil)
)

// ErrIsolationLevel is returned when a transaction asks for an isolation level
// other than the default or serializable.
var ErrIsolationLevel = errors.New("sqlite: unsupported isolation level; SQLite transactions are serializable, use sql.LevelDefault or sql.LevelSerializable")

// Begin is the legacy entry point; database/sql uses BeginTx.
func (c *conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	switch level := sql.IsolationLevel(opts.Isolation); level {
	case sql.LevelDefault, sql.LevelSerializable:
	default:
		return nil, fmt.Errorf("%w (got %s)", ErrIsolationLevel, level)
	}
	opts.Isolation = driver.IsolationLevel(sql.LevelDefault)
	if !opts.ReadOnly {
		return c.baseConn.BeginTx(ctx, opts) // BEGIN IMMEDIATE, from _txlock
	}
	if _, err := c.ExecContext(ctx, "PRAGMA query_only = 1", nil); err != nil {
		return nil, errors.Join(err, c.leaveReadOnly(ctx))
	}
	tx, err := c.baseConn.BeginTx(ctx, opts) // deferred BEGIN: no write lock
	if err != nil {
		return nil, errors.Join(err, c.leaveReadOnly(ctx))
	}
	return &readOnlyTx{tx: tx, conn: c}, nil
}

// leaveReadOnly resets query_only. On failure the connection is marked
// broken, so database/sql closes it instead of reusing it.
func (c *conn) leaveReadOnly(parent context.Context) error {
	// The caller's context may be the reason the transaction failed to begin,
	// and the reset must still run: keep its values, not its cancellation.
	if _, err := c.ExecContext(context.WithoutCancel(parent), "PRAGMA query_only = 0", nil); err != nil {
		c.broken.Store(true)
		return fmt.Errorf("sqlite: leave read-only mode: %w", err)
	}
	return nil
}

func (c *conn) ResetSession(ctx context.Context) error {
	if c.broken.Load() {
		return driver.ErrBadConn
	}
	return c.baseConn.ResetSession(ctx)
}

func (c *conn) IsValid() bool { return !c.broken.Load() && c.baseConn.IsValid() }

// readOnlyTx ends a read-only transaction and then leaves query_only mode,
// whether it committed or rolled back.
type readOnlyTx struct {
	tx   driver.Tx
	conn *conn
}

func (t *readOnlyTx) Commit() error {
	err := t.tx.Commit()
	return errors.Join(err, t.conn.leaveReadOnly(context.Background()))
}

func (t *readOnlyTx) Rollback() error {
	err := t.tx.Rollback()
	return errors.Join(err, t.conn.leaveReadOnly(context.Background()))
}
