package mysql

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/mysql/mysqltest"
	"github.com/rocketinsidecorp/forge-go/sqldb"
)

// The setup lock: a concurrent holder refuses, a failing setup rolls back
// and releases the lock, and an unreachable database fails.
func TestSetupLock(t *testing.T) {
	db := mysqltest.New(t)
	ctx := context.Background()
	const name = "forge:identity_setup_test"
	holder, err := db.SQL().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got sql.NullInt64
	if err := holder.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", name).Scan(&got); err != nil || got.Int64 != 1 {
		t.Fatalf("GET_LOCK = %v %v", got, err)
	}
	previous := setupLockTimeout
	setupLockTimeout = 0
	t.Cleanup(func() { setupLockTimeout = previous })
	ran := false
	err = setupLock(ctx, db, name, dialect.Tx, func(*sql.Tx) error { ran = true; return nil })
	if err == nil || ran || !strings.Contains(err.Error(), "another setup") {
		t.Fatalf("setup under a held lock: ran=%v err=%v", ran, err)
	}
	if err := holder.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", name).Scan(&got); err != nil {
		t.Fatal(err)
	}
	_ = holder.Close()

	failure := errors.New("setup failed")
	if err := setupLock(ctx, db, name, dialect.Tx, func(*sql.Tx) error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("failing setup: %v", err)
	}
	if err := setupLock(ctx, db, name, sqldb.TxOptions{Isolation: sql.LevelLinearizable}, func(*sql.Tx) error { return nil }); err == nil {
		t.Fatal("an unsupported isolation level began a transaction")
	}
	if err := setupLock(ctx, db, name, dialect.Tx, func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	_ = db.Close()
	if err := setupLock(ctx, db, name, dialect.Tx, func(*sql.Tx) error { return nil }); err == nil {
		t.Fatal("setup on a closed database succeeded")
	}
}
