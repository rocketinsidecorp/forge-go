package sqldb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/rocketinsidecorp/forge-go/config"
)

// DBTX is satisfied by *DB, *sql.DB, *sql.Tx and *sql.Conn, so a repository
// written against it runs standalone or inside a transaction. It matches the
// interface sqlc generates for database/sql.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DB is a database/sql pool configured by Forge and bound to one database's
// error classification.
type DB struct {
	db       *sql.DB
	classify Classifier
	// unobserve stops the pool metrics (Observer.ObservePool); nil when
	// the pool is not observed.
	unobserve func()
}

var _ DBTX = (*DB)(nil)

// New wraps db, applying the pool limits from cfg.
func New(db *sql.DB, cfg config.Database, classify Classifier) (*DB, error) {
	if db == nil || classify == nil {
		return nil, errors.New("sqldb: database and classifier are required")
	}
	db.SetMaxOpenConns(int(cfg.MaxConns))
	db.SetMaxIdleConns(int(cfg.MaxConns))
	db.SetConnMaxLifetime(cfg.MaxConnLifetime)
	db.SetConnMaxIdleTime(cfg.MaxConnIdleTime)
	return &DB{db: db, classify: classify}, nil
}

func (d *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.db.ExecContext(ctx, query, args...)
}

func (d *DB) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return d.db.PrepareContext(ctx, query)
}

func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.db.QueryContext(ctx, query, args...)
}

func (d *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.db.QueryRowContext(ctx, query, args...)
}

// Translate maps err with this database's classification; see the package
// function Translate.
func (d *DB) Translate(err error) error { return Translate(err, d.classify) }

// TxOptions configures InTxWith.
type TxOptions struct {
	// Isolation defaults to the database's default level.
	Isolation sql.IsolationLevel
	ReadOnly  bool
	// Attempts is how many times the whole transaction may run when it fails
	// with a serialization failure or deadlock. Values below 1 mean 1.
	// Retried functions run again from the start, so they must not have
	// effects outside the transaction.
	Attempts int
}

// InTx runs fn in a transaction that commits when fn returns nil and rolls
// back when it returns an error or panics. fn's error is returned unchanged.
func (d *DB) InTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return d.InTxWith(ctx, TxOptions{}, fn)
}

// InTxWith is InTx with explicit isolation, access mode and retry policy.
func (d *DB) InTxWith(ctx context.Context, opts TxOptions, fn func(tx *sql.Tx) error) error {
	if fn == nil {
		return errors.New("transaction function is required")
	}
	retryable := func(err error) bool { return d.classify(err).Kind == KindConflict }
	return Retry(ctx, opts.Attempts, retryable, func() error { return d.runTx(ctx, opts, fn) })
}

func (d *DB) runTx(ctx context.Context, opts TxOptions, fn func(tx *sql.Tx) error) error {
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{Isolation: opts.Isolation, ReadOnly: opts.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		// A no-op after Commit. database/sql rolls back on context
		// cancellation by itself; this covers errors and panics.
		_ = tx.Rollback()
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// Ping verifies a connection can be used. Its signature matches health.Check.
func (d *DB) Ping(ctx context.Context) error { return d.db.PingContext(ctx) }

// Close closes the pool. database/sql does not wait for queries in progress:
// idle connections close at once and busy ones when they are released.
func (d *DB) Close() error {
	d.stopObserving()
	return d.db.Close()
}

// Shutdown is Close bounded by ctx, for symmetry with the postgres adapter,
// whose pool does wait for borrowed connections. Its signature matches
// forge.ShutdownHook.
func (d *DB) Shutdown(ctx context.Context) error {
	d.stopObserving()
	return CloseWithin(ctx, d.db.Close, func() int { return d.db.Stats().InUse })
}

// stopObserving removes the pool from the connection metrics: a closed pool
// is no longer read at every collection.
func (d *DB) stopObserving() {
	if d.unobserve != nil {
		d.unobserve()
	}
}

// SQL exposes the underlying *sql.DB: the explicit escape hatch.
func (d *DB) SQL() *sql.DB { return d.db }
