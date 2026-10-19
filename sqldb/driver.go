package sqldb

import (
	"context"
	"database/sql/driver"
	"errors"
)

// InstrumentConnector returns a connector whose connections report every
// statement to obs, including statements inside transactions and prepared
// statements. It returns base unchanged when obs is nil.
//
// The wrappers forward each optional database/sql/driver interface to the
// driver's connection and statement, and answer exactly as database/sql
// would treat the interface's absence, so wrapping changes no behavior.
// Spans end when the driver returns: for queries that is when the first
// result is available, not when the rows are closed.
func InstrumentConnector(base driver.Connector, obs *Observer) driver.Connector {
	if obs == nil {
		return base
	}
	return &instrumentedConnector{base: base, obs: obs}
}

type instrumentedConnector struct {
	base driver.Connector
	obs  *Observer
}

func (c *instrumentedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &instrumentedConn{base: conn, obs: c.obs}, nil
}

func (c *instrumentedConnector) Driver() driver.Driver { return c.base.Driver() }

type instrumentedConn struct {
	base driver.Conn
	obs  *Observer
}

var (
	_ driver.Conn               = (*instrumentedConn)(nil)
	_ driver.ConnBeginTx        = (*instrumentedConn)(nil)
	_ driver.ConnPrepareContext = (*instrumentedConn)(nil)
	_ driver.ExecerContext      = (*instrumentedConn)(nil)
	_ driver.QueryerContext     = (*instrumentedConn)(nil)
	_ driver.Pinger             = (*instrumentedConn)(nil)
	_ driver.SessionResetter    = (*instrumentedConn)(nil)
	_ driver.Validator          = (*instrumentedConn)(nil)
	_ driver.NamedValueChecker  = (*instrumentedConn)(nil)
)

func (c *instrumentedConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *instrumentedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	var stmt driver.Stmt
	var err error
	if preparer, ok := c.base.(driver.ConnPrepareContext); ok {
		stmt, err = preparer.PrepareContext(ctx, query)
	} else {
		stmt, err = c.base.Prepare(query)
		if err == nil && ctx.Err() != nil {
			_ = stmt.Close()
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	wrapped := &instrumentedStmt{base: stmt, conn: c, query: query}
	if converter, ok := stmt.(driver.ColumnConverter); ok { //nolint:staticcheck // forwarded so database/sql keeps the driver's conversions
		return &converterStmt{instrumentedStmt: wrapped, converter: converter}, nil
	}
	return wrapped, nil
}

func (c *instrumentedConn) Close() error { return c.base.Close() }

func (c *instrumentedConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *instrumentedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if beginner, ok := c.base.(driver.ConnBeginTx); ok {
		return beginner.BeginTx(ctx, opts)
	}
	// What database/sql does for a driver without ConnBeginTx.
	if opts.Isolation != driver.IsolationLevel(0) {
		return nil, errors.New("sql: driver does not support non-default isolation level")
	}
	if opts.ReadOnly {
		return nil, errors.New("sql: driver does not support read-only transactions")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.base.Begin() //nolint:staticcheck // the fallback database/sql itself uses
}

func (c *instrumentedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.base.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	obs := c.obs.Start(ctx, query)
	result, err := execer.ExecContext(ctx, query, args)
	obs.End(err)
	return result, err
}

func (c *instrumentedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.base.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	obs := c.obs.Start(ctx, query)
	rows, err := queryer.QueryContext(ctx, query, args)
	obs.End(err)
	return rows, err
}

func (c *instrumentedConn) Ping(ctx context.Context) error {
	if pinger, ok := c.base.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *instrumentedConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.base.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *instrumentedConn) IsValid() bool {
	if validator, ok := c.base.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

func (c *instrumentedConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.base.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

type instrumentedStmt struct {
	base  driver.Stmt
	conn  *instrumentedConn
	query string
}

var (
	_ driver.StmtExecContext   = (*instrumentedStmt)(nil)
	_ driver.StmtQueryContext  = (*instrumentedStmt)(nil)
	_ driver.NamedValueChecker = (*instrumentedStmt)(nil)
)

func (s *instrumentedStmt) Close() error  { return s.base.Close() }
func (s *instrumentedStmt) NumInput() int { return s.base.NumInput() }

func (s *instrumentedStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.base.Exec(args) //nolint:staticcheck // driver.Stmt requires Exec
}

func (s *instrumentedStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.base.Query(args) //nolint:staticcheck // driver.Stmt requires Query
}

func (s *instrumentedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	obs := s.conn.obs.Start(ctx, s.query)
	var result driver.Result
	var err error
	if execer, ok := s.base.(driver.StmtExecContext); ok {
		result, err = execer.ExecContext(ctx, args)
	} else {
		var values []driver.Value
		if values, err = namedValues(args); err == nil {
			if err = ctx.Err(); err == nil {
				result, err = s.base.Exec(values) //nolint:staticcheck // the fallback database/sql itself uses
			}
		}
	}
	obs.End(err)
	return result, err
}

func (s *instrumentedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	obs := s.conn.obs.Start(ctx, s.query)
	var rows driver.Rows
	var err error
	if queryer, ok := s.base.(driver.StmtQueryContext); ok {
		rows, err = queryer.QueryContext(ctx, args)
	} else {
		var values []driver.Value
		if values, err = namedValues(args); err == nil {
			if err = ctx.Err(); err == nil {
				rows, err = s.base.Query(values) //nolint:staticcheck // the fallback database/sql itself uses
			}
		}
	}
	obs.End(err)
	return rows, err
}

// CheckNamedValue keeps database/sql's precedence: the statement's checker,
// then the connection's, then the default conversion.
func (s *instrumentedStmt) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := s.base.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return s.conn.CheckNamedValue(value)
}

// converterStmt forwards the deprecated ColumnConverter of drivers that
// still implement it.
type converterStmt struct {
	*instrumentedStmt
	converter driver.ColumnConverter //nolint:staticcheck // see PrepareContext
}

func (s *converterStmt) ColumnConverter(index int) driver.ValueConverter {
	return s.converter.ColumnConverter(index)
}

func namedValues(args []driver.NamedValue) ([]driver.Value, error) {
	values := make([]driver.Value, len(args))
	for i, arg := range args {
		if arg.Name != "" {
			return nil, errors.New("sql: driver does not support the use of Named Parameters")
		}
		values[i] = arg.Value
	}
	return values, nil
}
