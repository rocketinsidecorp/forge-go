package encryption_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/encryption"
)

func TestCipherRefusesWhatItCannotSealOrIndex(t *testing.T) {
	t.Parallel()
	if _, err := encryption.NewCipher(nil); err == nil {
		t.Error("a nil key ring was accepted")
	}
	cipher := newCipher(t, testKey(t, "k1", 1), testKey(t, "k0", 9))
	if cipher.ActiveKeyID() != "k1" {
		t.Errorf("ActiveKeyID = %q", cipher.ActiveKeyID())
	}
	if _, err := cipher.Encrypt([]byte("x"), encryption.AAD{Table: "people", Column: "ssn"}); err == nil {
		t.Error("a value without a record id was sealed")
	}
	if _, err := cipher.Encrypt(make([]byte, encryption.MaxPlaintextBytes+1), rowAAD); !errors.Is(err, encryption.ErrTooLarge) {
		t.Errorf("an oversized value: %v", err)
	}
	if _, err := cipher.BlindIndex("", "ssn", []byte("x")); err == nil {
		t.Error("a blind index without a table was computed")
	}
	if _, err := cipher.BlindIndex("people", "ssn", make([]byte, encryption.MaxPlaintextBytes+1)); !errors.Is(err, encryption.ErrTooLarge) {
		t.Errorf("an oversized blind index input: %v", err)
	}
	if _, err := cipher.BlindIndexes("people", "", []byte("x")); err == nil {
		t.Error("blind indexes without a column were computed")
	}
	digests, err := cipher.BlindIndexes("people", "ssn", []byte("x"))
	if err != nil || len(digests) != 2 || digests[0] == digests[1] {
		t.Errorf("BlindIndexes = %v, %v; want one distinct digest per key, active first", digests, err)
	}
	active, err := cipher.BlindIndex("people", "ssn", []byte("x"))
	if err != nil || digests[0] != active {
		t.Errorf("the first digest is not the active key's: %v vs %v (%v)", digests[0], active, err)
	}
	for _, malformed := range []string{"", "plain text", "fg1.k1", "fg1.unknownkey.AAAA", "fg1.k1.!!!"} {
		if _, err := cipher.NeedsRotation(malformed); err == nil {
			t.Errorf("NeedsRotation(%q) reported no error", malformed)
		}
		if _, err := cipher.Decrypt(malformed, rowAAD); err == nil {
			t.Errorf("Decrypt(%q) reported no error", malformed)
		}
	}
}

// scriptedColumn is a Rewriter whose pages and replacements are scripted.
type scriptedColumn struct {
	table, column string
	pages         func(after string, limit int) ([]encryption.Record, error)
	replace       func(encryption.Update) (bool, error)
}

func (s scriptedColumn) Table() string  { return s.table }
func (s scriptedColumn) Column() string { return s.column }
func (s scriptedColumn) Page(_ context.Context, after string, limit int) ([]encryption.Record, error) {
	return s.pages(after, limit)
}
func (s scriptedColumn) Replace(_ context.Context, u encryption.Update) (bool, error) {
	return s.replace(u)
}

func TestReencryptStopsOnARewriterThatMisbehaves(t *testing.T) {
	t.Parallel()
	oldKey, newKey := testKey(t, "k1", 1), testKey(t, "k2", 2)
	before, after := newCipher(t, oldKey), newCipher(t, newKey, oldKey)
	ctx := context.Background()
	sealed, err := before.Encrypt([]byte("v"), encryption.AAD{Table: "people", Column: "ssn", RecordID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	one := func(string, int) ([]encryption.Record, error) {
		return []encryption.Record{{ID: "a", Sealed: sealed}}, nil
	}

	if _, err := after.Reencrypt(ctx, nil, encryption.RotateOptions{}); err == nil {
		t.Error("a nil rewriter was accepted")
	}

	// A page that repeats the checkpoint would loop forever.
	var pages int
	repeating := scriptedColumn{table: "people", column: "ssn", pages: func(string, int) ([]encryption.Record, error) {
		pages++
		return []encryption.Record{{ID: "a", Sealed: sealed}}, nil
	}, replace: func(encryption.Update) (bool, error) { return true, nil }}
	report, err := after.Reencrypt(ctx, repeating, encryption.RotateOptions{BatchSize: 1})
	if err == nil || !strings.Contains(err.Error(), "did not advance") || pages != 2 {
		t.Errorf("a repeating rewriter: %+v, %v after %d pages", report, err, pages)
	}

	// A value that is not a ciphertext stops the run, naming the row only.
	malformed := scriptedColumn{table: "people", column: "ssn", pages: func(string, int) ([]encryption.Record, error) {
		return []encryption.Record{{ID: "a", Sealed: "plain secret value"}}, nil
	}}
	if _, err := after.Reencrypt(ctx, malformed, encryption.RotateOptions{}); !errors.Is(err, encryption.ErrMalformed) || !strings.Contains(err.Error(), `row "a"`) || strings.Contains(err.Error(), "plain secret value") {
		t.Errorf("a malformed value: %v", err)
	}

	// A rewriter that refuses the replacement stops the run.
	refusing := scriptedColumn{table: "people", column: "ssn", pages: one,
		replace: func(encryption.Update) (bool, error) { return false, errors.New("database refused") }}
	if _, err := after.Reencrypt(ctx, refusing, encryption.RotateOptions{}); err == nil || !strings.Contains(err.Error(), `row "a"`) || !strings.Contains(err.Error(), "database refused") {
		t.Errorf("a refused replacement: %v", err)
	}

	// A row that cannot be bound to its table and column (an unnamed table)
	// cannot be decrypted, so nothing is rewritten.
	unnamed := scriptedColumn{column: "ssn", pages: one,
		replace: func(encryption.Update) (bool, error) { t.Error("a row was rewritten"); return true, nil }}
	if _, err := after.Reencrypt(ctx, unnamed, encryption.RotateOptions{BlindIndex: true}); err == nil {
		t.Error("a column without a table was rotated")
	}
}
