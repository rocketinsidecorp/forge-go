package encryption_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/encryption"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
)

func TestKeyDerivationAndAEADRefuseBadInput(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, 16, 31, 33, 64} {
		if aead, err := encryption.NewAEAD(make([]byte, size)); !errors.Is(err, encryption.ErrInvalidKey) || aead != nil {
			t.Errorf("NewAEAD(%d bytes) = %v, %v; want ErrInvalidKey", size, aead, err)
		}
		if key, err := encryption.DeriveKey(make([]byte, size), "purpose"); size != encryption.KeySize && (!errors.Is(err, encryption.ErrInvalidKey) || key != nil) {
			t.Errorf("DeriveKey(%d bytes) = %v, %v; want ErrInvalidKey", size, key, err)
		}
	}
	if aead, err := encryption.NewAEAD(make([]byte, encryption.KeySize)); err != nil || aead == nil {
		t.Errorf("NewAEAD(%d bytes) = %v, %v", encryption.KeySize, aead, err)
	}
	// A master key with no purpose would derive the same key everywhere.
	if key, err := encryption.DeriveKey(make([]byte, encryption.KeySize), ""); err == nil || key != nil || !strings.Contains(err.Error(), "purpose") {
		t.Errorf("DeriveKey without a purpose = %v, %v", key, err)
	}
}

// A cancelled context stops a rotation before it reads a page, and the
// report keeps the checkpoint it was given so the run can resume.
func TestReencryptStopsOnACancelledContextBeforeReadingAnything(t *testing.T) {
	t.Parallel()
	c := newCipher(t, testKey(t, "k1", 1))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	read := false
	rewriter := scriptedColumn{table: "people", column: "ssn", pages: func(string, int) ([]encryption.Record, error) {
		read = true
		return nil, nil
	}}
	report, err := c.Reencrypt(ctx, rewriter, encryption.RotateOptions{After: "checkpoint"})
	if !errors.Is(err, context.Canceled) || read || report.LastID != "checkpoint" || report.Scanned != 0 {
		t.Errorf("Reencrypt = %+v, %v (page read: %v)", report, err, read)
	}
}

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

// A scanned ciphertext that does not belong to the record it is written back
// for is refused rather than re-encrypted, and a plaintext that cannot be
// encoded is reported without being stored.
func TestValueRefusesToWriteWhatItCannotVerifyOrEncode(t *testing.T) {
	t.Parallel()
	c := newCipher(t, testKey(t, "k1", 1))
	sealed, err := c.Encrypt([]byte("secret"), encryption.AAD{Table: "people", Column: "ssn", RecordID: "row-1"})
	if err != nil {
		t.Fatal(err)
	}
	moved := encryption.NewValue[string](c, "people", "ssn")
	if err := moved.Scan(sealed); err != nil {
		t.Fatal(err)
	}
	moved.ForRecord("row-2")
	stored, err := moved.Value()
	if !errors.Is(err, encryption.ErrDecrypt) || stored != nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("Value of a ciphertext moved to another row = %v, %v", stored, err)
	}

	unencodable := encryption.NewValue[chan int](c, "people", "ssn").ForRecord("row-1")
	unencodable.Set(make(chan int))
	if stored, err := unencodable.Value(); err == nil || stored != nil {
		t.Errorf("Value of a channel = %v, %v; want an error", stored, err)
	}
}
