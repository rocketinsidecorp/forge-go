package encryption_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/rocketinsidecorp/forge-go/encryption"
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

func TestSQLColumnRejectsUnsafeIdentifiers(t *testing.T) {
	t.Parallel()
	for _, spec := range []encryption.SQLColumn{
		{Table: "people; DROP TABLE people", Column: "ssn"},
		{Table: "people", Column: "ssn", IDColumn: "id OR 1=1"},
		{Table: "People", Column: "ssn"},
		{Table: "people", Column: "ssn", IndexColumn: "a-b"},
		{Table: "", Column: "ssn"},
	} {
		if _, err := spec.Validate(); err == nil {
			t.Errorf("%+v accepted", spec)
		}
	}
}
