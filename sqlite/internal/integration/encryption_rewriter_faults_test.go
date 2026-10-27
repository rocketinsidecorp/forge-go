package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/encryption"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
)

// A row the database cannot hand over (a NULL id, which SQLite allows in a
// TEXT primary key) stops the run with the scan error, not a silent skip.
func TestSQLRewriterReportsARowItCannotScan(t *testing.T) {
	t.Parallel()
	db := sqlitetest.New(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE people (id TEXT PRIMARY KEY, ssn TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO people (id, ssn) VALUES (NULL, 'fg1.k1.AAAA')`); err != nil {
		t.Fatal(err)
	}
	rewriter, err := encryption.NewSQLRewriter(db, encryption.SQLColumn{Table: "people", Column: "ssn"})
	if err != nil {
		t.Fatal(err)
	}
	if page, err := rewriter.Page(ctx, "", 10); err == nil || page != nil {
		t.Fatalf("Page over a NULL id = %v, %v; want an error", page, err)
	}
	c := newCipher(t, testKey(t, "k1", 1))
	report, err := c.Reencrypt(ctx, rewriter, encryption.RotateOptions{})
	if err == nil || !strings.Contains(err.Error(), "read page") || report.Scanned != 0 {
		t.Errorf("Reencrypt = %+v, %v", report, err)
	}
}

func TestSQLRewriterReportsADatabaseThatFails(t *testing.T) {
	t.Parallel()
	if _, err := encryption.NewSQLRewriter(nil, encryption.SQLColumn{Table: "people", Column: "ssn"}); err == nil {
		t.Error("a nil database was accepted")
	}
	db := sqlitetest.New(t)
	if _, err := encryption.NewSQLRewriter(db, encryption.SQLColumn{Table: "people; DROP", Column: "ssn"}); err == nil {
		t.Error("an unsafe identifier was accepted")
	}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE people (id TEXT PRIMARY KEY, ssn TEXT)`); err != nil {
		t.Fatal(err)
	}
	// Without an index column a blind index cannot be written.
	plain, err := encryption.NewSQLRewriter(db, encryption.SQLColumn{Table: "people", Column: "ssn"})
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := plain.Replace(ctx, encryption.Update{ID: "a", OldSealed: "x", NewSealed: "y", Index: "digest"}); err == nil || changed {
		t.Errorf("Replace with an index and no index column = %v, %v", changed, err)
	}
	// A missing column is reported by the database, for pages and for updates.
	broken, err := encryption.NewSQLRewriter(db, encryption.SQLColumn{Table: "people", Column: "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broken.Page(ctx, "", 10); err == nil {
		t.Error("Page over a missing column succeeded")
	}
	if changed, err := broken.Replace(ctx, encryption.Update{ID: "a", OldSealed: "x", NewSealed: "y"}); err == nil || changed {
		t.Errorf("Replace over a missing column = %v, %v", changed, err)
	}
}
