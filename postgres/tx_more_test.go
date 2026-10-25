package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestTransactionHelpersRefuseMisuseBeforeTouchingTheDatabase(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	ctx := context.Background()
	if err := db.InTx(ctx, nil); err == nil || !strings.Contains(err.Error(), "transaction function is required") {
		t.Errorf("InTx(nil) = %v", err)
	}
	if err := db.InTenantTx(ctx, nil); err == nil || !strings.Contains(err.Error(), "transaction function is required") {
		t.Errorf("InTenantTx(nil) = %v", err)
	}
	called := false
	err := db.InTenantTx(ctx, func(pgx.Tx) error { called = true; return nil })
	if err == nil {
		t.Error("a tenant transaction without a tenant in the context succeeded")
	}
	if called {
		t.Error("the function ran without a tenant")
	}
}

func TestReadOnlyTransactionsRefuseWrites(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE notes (id uuid PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	err := db.InTxWith(ctx, postgres.TxOptions{ReadOnly: true}, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO notes VALUES ($1)`, uuid.MustNew())
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("a write in a read-only transaction: %v", err)
	}
	var rows int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM notes`).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("the refused write left %d rows (%v)", rows, err)
	}
}
