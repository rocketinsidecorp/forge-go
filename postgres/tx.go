package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/tenancy"
)

// Isolation levels accepted by TxOptions.
const (
	ReadCommitted  = pgx.ReadCommitted
	RepeatableRead = pgx.RepeatableRead
	Serializable   = pgx.Serializable
)

const rollbackTimeout = 5 * time.Second

// TxOptions configures InTxWith.
type TxOptions struct {
	// Isolation defaults to PostgreSQL's default, READ COMMITTED.
	Isolation pgx.TxIsoLevel
	ReadOnly  bool
	// Attempts is how many times the whole transaction may run when it fails
	// with a serialization failure or deadlock (SQLSTATE 40001 / 40P01),
	// which SERIALIZABLE transactions must expect. Values below 1 mean 1.
	// Retried functions run again from the start, so they must not have
	// effects outside the transaction.
	Attempts int
}

// InTx runs fn in a READ COMMITTED transaction. The transaction commits when
// fn returns nil and rolls back when it returns an error or panics. fn's error
// is returned unchanged.
func (db *DB) InTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return db.InTxWith(ctx, TxOptions{}, fn)
}

// InTxWith is InTx with explicit isolation, access mode and retry policy.
func (db *DB) InTxWith(ctx context.Context, opts TxOptions, fn func(tx pgx.Tx) error) error {
	if fn == nil {
		return errors.New("transaction function is required")
	}
	return sqldb.Retry(ctx, opts.Attempts, isRetryable, func() error { return db.runTx(ctx, opts, fn) })
}

// InTenantTx runs fn in a transaction whose PostgreSQL-local tenant setting
// comes from ctx. It fails before acquiring a connection when ctx has no
// tenant. The setting is transaction-local, so it cannot leak through the
// pool. RLS policies should read current_setting('forge.tenant_id', true).
func (db *DB) InTenantTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return db.InTenantTxWith(ctx, TxOptions{}, fn)
}

// InTenantTxWith is InTenantTx with explicit transaction options. The tenant
// setting is installed again for every serialization/deadlock retry.
func (db *DB) InTenantTxWith(ctx context.Context, opts TxOptions, fn func(tx pgx.Tx) error) error {
	if fn == nil {
		return errors.New("transaction function is required")
	}
	tenant, err := tenancy.Require(ctx)
	if err != nil {
		return err
	}
	return db.InTxWith(ctx, opts, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('forge.tenant_id', $1, true)", tenant.ID.String()); err != nil {
			return fmt.Errorf("set transaction tenant: %w", err)
		}
		return fn(tx)
	})
}

func (db *DB) runTx(ctx context.Context, opts TxOptions, fn func(tx pgx.Tx) error) error {
	access := pgx.ReadWrite
	if opts.ReadOnly {
		access = pgx.ReadOnly
	}
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: opts.Isolation, AccessMode: access})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		// A no-op after Commit. Uses a fresh deadline so a cancelled request
		// still releases its connection cleanly.
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func isRetryable(err error) bool { return classify(err).Kind == sqldb.KindConflict }
