package integration_test

import (
	"context"
	"testing"

	"github.com/rocketinsidecorp/forge-go/encryption"
	"github.com/rocketinsidecorp/forge-go/mysql/mysqltest"
	"github.com/rocketinsidecorp/forge-go/sqldb"
)

// A valid name that is a reserved word works end to end on MySQL/MariaDB (the
// SQLite half is in sqlite/internal/integration).
func TestSQLRewriterWorksWithReservedNamesOnMySQL(t *testing.T) {
	t.Parallel()
	rotateReservedNames(t, mysqltest.New(t))
}

func rotateReservedNames(t *testing.T, db *sqldb.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE `order` (`group` VARCHAR(20) PRIMARY KEY, `user` TEXT, `select` TEXT)"); err != nil {
		t.Fatal(err)
	}
	oldKey, newKey := testKey(t, "k1", 1), testKey(t, "k2", 2)
	before, after := newCipher(t, oldKey), newCipher(t, newKey, oldKey)
	for _, id := range []string{"a", "b", "c"} {
		v := encryption.NewValue[string](before, "order", "user").ForRecord(id)
		v.Set("plain-" + id)
		if _, err := db.ExecContext(ctx, "INSERT INTO `order` (`group`, `user`) VALUES (?, ?)", id, v); err != nil {
			t.Fatal(err)
		}
	}
	rewriter, err := encryption.NewSQLRewriter(db, encryption.SQLColumn{Table: "order", Column: "user", IDColumn: "group", IndexColumn: "select"})
	if err != nil {
		t.Fatal(err)
	}
	report, err := after.Reencrypt(ctx, rewriter, encryption.RotateOptions{BatchSize: 2, BlindIndex: true})
	if err != nil || report.Rotated != 3 || report.Reindexed != 3 {
		t.Fatalf("report %+v, %v", report, err)
	}
}
