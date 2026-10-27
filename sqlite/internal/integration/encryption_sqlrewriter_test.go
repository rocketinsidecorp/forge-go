package integration_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/rocketinsidecorp/forge-go/encryption"
	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
)

func TestSQLRewriterOnSQLite(t *testing.T) {
	t.Parallel()
	db := sqlitetest.New(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE people (id TEXT PRIMARY KEY, ssn TEXT, ssn_idx TEXT)`); err != nil {
		t.Fatal(err)
	}
	oldKey, newKey := testKey(t, "k1", 1), testKey(t, "k2", 2)
	before, after := newCipher(t, oldKey), newCipher(t, newKey, oldKey)
	for i := range 12 {
		id := fmt.Sprintf("id-%02d", i)
		v := encryption.NewValue[string](before, "people", "ssn").ForRecord(id)
		v.Set("plain-" + id)
		if _, err := db.ExecContext(ctx, `INSERT INTO people (id, ssn) VALUES (?, ?)`, id, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO people (id, ssn) VALUES ('id-null', NULL)`); err != nil {
		t.Fatal(err)
	}
	rewriter, err := encryption.NewSQLRewriter(db, encryption.SQLColumn{Table: "people", Column: "ssn", IndexColumn: "ssn_idx"})
	if err != nil {
		t.Fatal(err)
	}
	report, err := after.Reencrypt(ctx, rewriter, encryption.RotateOptions{BatchSize: 5, BlindIndex: true})
	if err != nil || report.Rotated != 12 || report.Reindexed != 12 || report.Scanned != 12 {
		t.Fatalf("report %+v, %v", report, err)
	}
	var idx, sealed string
	if err := db.QueryRowContext(ctx, `SELECT ssn, ssn_idx FROM people WHERE id = 'id-03'`).Scan(&sealed, &idx); err != nil {
		t.Fatal(err)
	}
	want, _ := after.BlindIndex("people", "ssn", []byte("plain-id-03"))
	v := encryption.NewValue[string](newCipher(t, newKey), "people", "ssn").ForRecord("id-03")
	_ = v.Scan(sealed)
	if got, _, err := v.Get(); idx != want || err != nil || got != "plain-id-03" {
		t.Fatalf("row after rotation: idx ok=%v value=%q err=%v", idx == want, got, err)
	}
}

// TestSQLRewriterIntegerKeysUseDatabaseOrder: bigint ids 1..12 sort 9 before
// 10 in the database but "10" before "9" as Go strings; the rotation must
// trust the database order instead of failing as out of order.
func TestSQLRewriterIntegerKeysUseDatabaseOrder(t *testing.T) {
	t.Parallel()
	db := sqlitetest.New(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`); err != nil {
		t.Fatal(err)
	}
	oldKey, newKey := testKey(t, "k1", 1), testKey(t, "k2", 2)
	before, after := newCipher(t, oldKey), newCipher(t, newKey, oldKey)
	for i := 1; i <= 12; i++ {
		v := encryption.NewValue[string](before, "notes", "body").ForRecord(fmt.Sprint(i))
		v.Set(fmt.Sprintf("note-%d", i))
		if _, err := db.ExecContext(ctx, `INSERT INTO notes (id, body) VALUES (?, ?)`, i, v); err != nil {
			t.Fatal(err)
		}
	}
	rewriter, err := encryption.NewSQLRewriter(db, encryption.SQLColumn{Table: "notes", Column: "body"})
	if err != nil {
		t.Fatal(err)
	}
	report, err := after.Reencrypt(ctx, rewriter, encryption.RotateOptions{BatchSize: 5})
	if err != nil || report.Rotated != 12 || report.LastID != "12" {
		t.Fatalf("report %+v, %v", report, err)
	}
}

// A valid name that is a reserved word works end to end on SQLite (the
// MySQL/MariaDB half is in mysql/internal/integration).
func TestSQLRewriterWorksWithReservedNames(t *testing.T) {
	t.Parallel()
	rotateReservedNames(t, sqlitetest.New(t))
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
