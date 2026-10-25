package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rocketinsidecorp/forge-go/encryption"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestEncryptedColumnRefusesBadArguments(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	spec := encryption.SQLColumn{Table: "people", Column: "ssn"}
	if _, err := postgres.EncryptedColumn(nil, spec); err == nil {
		t.Error("a nil database was accepted")
	}
	for _, bad := range []encryption.SQLColumn{
		{Table: "people; DROP TABLE people", Column: "ssn"},
		{Table: "people", Column: "ssn", IndexColumn: "idx space"},
	} {
		if _, err := postgres.EncryptedColumn(db, bad); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
	if _, err := postgres.EncryptedColumn(db, spec, nil, postgres.WithTenant(tenancy.Tenant{ID: uuid.MustNew()})); err != nil {
		t.Errorf("a nil option and a valid tenant: %v", err)
	}
	// A tenant that is not a UUIDv7 never reaches the database.
	if _, err := postgres.EncryptedColumn(db, spec, postgres.WithTenant(tenancy.Tenant{})); !errors.Is(err, tenancy.ErrInvalid) {
		t.Errorf("an invalid tenant: %v", err)
	}
	// Tenant scope needs something that can open a transaction.
	if _, err := postgres.EncryptedColumn(noTransactions{db}, spec, postgres.WithTenant(tenancy.Tenant{ID: uuid.MustNew()})); err == nil ||
		!strings.Contains(err.Error(), "needs a *postgres.DB or a pgx.Tx") {
		t.Errorf("a tenant scope without transactions: %v", err)
	}
}

// noTransactions hides the transaction methods of a *postgres.DB.
type noTransactions struct{ db *postgres.DB }

func (n noTransactions) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return n.db.Exec(ctx, sql, args...)
}
func (n noTransactions) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return n.db.Query(ctx, sql, args...)
}
func (n noTransactions) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return n.db.QueryRow(ctx, sql, args...)
}

func TestEncryptedColumnReportsDatabaseFailures(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	ctx := context.Background()
	// An array is not a text column: the page cannot be read.
	if _, err := db.Exec(ctx, `CREATE TABLE counters (id bigint PRIMARY KEY, secret int[])`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO counters VALUES (1, '{1,2}')`); err != nil {
		t.Fatal(err)
	}
	rewriter, err := postgres.EncryptedColumn(db, encryption.SQLColumn{Table: "counters", Column: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rewriter.Page(ctx, "", 10); err == nil || !strings.Contains(err.Error(), "must be uuid or text columns") {
		t.Errorf("Page over a non-text column = %v", err)
	}
	// A query that fails: the column does not exist.
	missing, err := postgres.EncryptedColumn(db, encryption.SQLColumn{Table: "counters", Column: "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.Page(ctx, "", 10); err == nil {
		t.Error("Page over a missing column succeeded")
	}
	// Replace reports an update the database refuses, and changes nothing.
	changed, err := missing.Replace(ctx, encryption.Update{ID: "1", OldSealed: "sealed", NewSealed: "new"})
	if err == nil || changed {
		t.Errorf("Replace over a missing column = %v, %v", changed, err)
	}
	// A closed database fails the catalog check that guards unscoped pages.
	closed := postgrestest.New(t)
	column, err := postgres.EncryptedColumn(closed, encryption.SQLColumn{Table: "counters", Column: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if _, err := column.Page(ctx, "", 10); err == nil || !strings.Contains(err.Error(), "check row-level security") {
		t.Errorf("Page on a closed database = %v", err)
	}
}

func TestEncryptedColumnTenantScopeOverEachKindOfConnection(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE notes (id uuid PRIMARY KEY, body text)`); err != nil {
		t.Fatal(err)
	}
	id := uuid.MustNew()
	if _, err := db.Exec(ctx, `INSERT INTO notes VALUES ($1, 'sealed')`, id); err != nil {
		t.Fatal(err)
	}
	tenant := tenancy.Tenant{ID: uuid.MustNew()}
	spec := encryption.SQLColumn{Table: "notes", Column: "body"}

	// A pool opens a transaction for each step.
	column, err := postgres.EncryptedColumn(db.Pool(), spec, postgres.WithTenant(tenant))
	if err != nil {
		t.Fatal(err)
	}
	page, err := column.Page(ctx, "", 10)
	if err != nil || len(page) != 1 || page[0].ID != id.String() {
		t.Fatalf("Page through a pool = %+v, %v", page, err)
	}
	if changed, err := column.Replace(ctx, encryption.Update{ID: id.String(), OldSealed: "sealed", NewSealed: "rotated"}); err != nil || !changed {
		t.Fatalf("Replace through a pool = %v, %v", changed, err)
	}

	// Inside the caller's transaction each step is a savepoint, and the
	// caller's own tenant is back afterwards.
	caller := uuid.MustNew().String()
	err = db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('forge.tenant_id', $1, true)", caller); err != nil {
			return err
		}
		inTx, err := postgres.EncryptedColumn(tx, spec, postgres.WithTenant(tenant))
		if err != nil {
			return err
		}
		if _, err := inTx.Page(ctx, "", 10); err != nil {
			return err
		}
		var current string
		if err := tx.QueryRow(ctx, "SELECT current_setting('forge.tenant_id', true)").Scan(&current); err != nil {
			return err
		}
		if current != caller {
			t.Errorf("the caller's tenant after a step is %q, want %q", current, caller)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A transaction that already failed cannot say which tenant it is in.
	err = db.InTx(ctx, func(tx pgx.Tx) error {
		_, _ = tx.Exec(ctx, "SELECT no_such_function()") // aborts the transaction
		inTx, err := postgres.EncryptedColumn(tx, spec, postgres.WithTenant(tenant))
		if err != nil {
			return err
		}
		_, err = inTx.Page(ctx, "", 10)
		if err == nil || !strings.Contains(err.Error(), "read transaction tenant") {
			t.Errorf("Page in an aborted transaction = %v", err)
		}
		return errors.New("rollback")
	})
	if err == nil || err.Error() != "rollback" {
		t.Fatal(err)
	}
}

// restoreFails fails the statement that restores the caller's tenant.
type restoreFails struct{ pgx.Tx }

func (r restoreFails) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "set_config('forge.tenant_id'") {
		return pgconn.CommandTag{}, errors.New("restore refused")
	}
	return r.Tx.Exec(ctx, sql, args...)
}

func TestEncryptedColumnReportsAFailureToRestoreTheCallersTenant(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE notes (id uuid PRIMARY KEY, body text)`); err != nil {
		t.Fatal(err)
	}
	err := db.InTx(ctx, func(tx pgx.Tx) error {
		column, err := postgres.EncryptedColumn(restoreFails{tx}, encryption.SQLColumn{Table: "notes", Column: "body"},
			postgres.WithTenant(tenancy.Tenant{ID: uuid.MustNew()}))
		if err != nil {
			return err
		}
		_, err = column.Page(ctx, "", 10)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "restore transaction tenant") {
		t.Fatalf("Page = %v, want the restore failure", err)
	}
}
