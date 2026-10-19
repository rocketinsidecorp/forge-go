package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/encryption"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func ringCipher(t *testing.T, active byte, previous ...byte) *encryption.Cipher {
	t.Helper()
	key := func(fill byte) encryption.Key {
		m := make([]byte, encryption.KeySize)
		for i := range m {
			m[i] = fill
		}
		return encryption.Key{ID: "k" + string(rune('0'+fill)), Material: m}
	}
	var old []encryption.Key
	for _, fill := range previous {
		old = append(old, key(fill))
	}
	ring, err := encryption.NewKeyRing(key(active), old...)
	if err != nil {
		t.Fatal(err)
	}
	c, err := encryption.NewCipher(ring)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestEncryptedColumnRotation stores attributes through encryption.Value in a
// uuid-keyed table, finds a row by blind index, and rotates the column to a new
// key with the pgx rewriter.
func TestEncryptedColumnRotation(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE people (id uuid PRIMARY KEY, ssn text, ssn_idx text)`); err != nil {
		t.Fatal(err)
	}
	before := ringCipher(t, 1)
	ids := make([]uuid.UUID, 7)
	for i := range ids {
		ids[i] = uuid.MustNew()
		ssn := encryption.NewValue[string](before, "people", "ssn").ForRecord(ids[i].String())
		ssn.Set("ssn-" + string(rune('a'+i)))
		index, err := before.BlindIndex("people", "ssn", []byte("ssn-"+string(rune('a'+i))))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO people (id, ssn, ssn_idx) VALUES ($1, $2, $3)`, ids[i], ssn, index); err != nil {
			t.Fatal(err)
		}
	}
	var raw string
	if err := db.QueryRow(ctx, `SELECT ssn FROM people WHERE id = $1`, ids[0]).Scan(&raw); err != nil || strings.Contains(raw, "ssn-a") || !strings.HasPrefix(raw, "fg1.k1.") {
		t.Fatalf("stored value %q, %v", raw, err)
	}
	// A ciphertext moved to another row is rejected on read.
	if _, err := db.Exec(ctx, `UPDATE people SET ssn = (SELECT ssn FROM people WHERE id = $1) WHERE id = $2`, ids[0], ids[1]); err != nil {
		t.Fatal(err)
	}
	swapped := encryption.NewValue[string](before, "people", "ssn")
	if err := db.QueryRow(ctx, `SELECT ssn FROM people WHERE id = $1`, ids[1]).Scan(swapped); err != nil {
		t.Fatal(err)
	}
	if _, _, err := swapped.ForRecord(ids[1].String()).Get(); err == nil {
		t.Fatal("a ciphertext swapped between rows was accepted")
	}
	// Restore row 1 so the rotation below has only valid rows.
	restored := encryption.NewValue[string](before, "people", "ssn").ForRecord(ids[1].String())
	restored.Set("ssn-b")
	if _, err := db.Exec(ctx, `UPDATE people SET ssn = $1 WHERE id = $2`, restored, ids[1]); err != nil {
		t.Fatal(err)
	}

	after := ringCipher(t, 2, 1)
	rewriter, err := postgres.EncryptedColumn(db, encryption.SQLColumn{Table: "people", Column: "ssn", IndexColumn: "ssn_idx"})
	if err != nil {
		t.Fatal(err)
	}
	report, err := after.Reencrypt(ctx, rewriter, encryption.RotateOptions{BatchSize: 3, BlindIndex: true})
	if err != nil || report.Rotated != 7 || report.Reindexed != 7 || report.Conflicts != 0 {
		t.Fatalf("report %+v, %v", report, err)
	}
	retired := ringCipher(t, 2) // the old key is no longer needed
	for i, id := range ids {
		var found string
		index, _ := retired.BlindIndex("people", "ssn", []byte("ssn-"+string(rune('a'+i))))
		if err := db.QueryRow(ctx, `SELECT id::text FROM people WHERE ssn_idx = $1`, index).Scan(&found); err != nil || found != id.String() {
			t.Fatalf("blind index lookup %d: %q %v", i, found, err)
		}
		v := encryption.NewValue[string](retired, "people", "ssn")
		if err := db.QueryRow(ctx, `SELECT ssn FROM people WHERE id = $1`, id).Scan(v); err != nil {
			t.Fatal(err)
		}
		if got, _, err := v.ForRecord(id.String()).Get(); err != nil || got != "ssn-"+string(rune('a'+i)) {
			t.Fatalf("row %d after rotation: %q %v", i, got, err)
		}
	}
}
