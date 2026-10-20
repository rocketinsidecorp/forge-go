package encryption_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/encryption"
)

func testKey(t testing.TB, id string, fill byte) encryption.Key {
	t.Helper()
	return encryption.Key{ID: id, Material: bytes.Repeat([]byte{fill}, encryption.KeySize)}
}

func newCipher(t testing.TB, active encryption.Key, previous ...encryption.Key) *encryption.Cipher {
	t.Helper()
	ring, err := encryption.NewKeyRing(active, previous...)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := encryption.NewCipher(ring)
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

var rowAAD = encryption.AAD{Table: "people", Column: "ssn", RecordID: "0190f1c0-0000-7000-8000-000000000001"}

func TestRoundTripIsRandomized(t *testing.T) {
	t.Parallel()
	c := newCipher(t, testKey(t, "k1", 1))
	a, err := c.Encrypt([]byte("123-45-6789"), rowAAD)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Encrypt([]byte("123-45-6789"), rowAAD)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two encryptions of one value are identical: nonce reuse or deterministic mode")
	}
	if !strings.HasPrefix(a, "fg1.k1.") || strings.Contains(a, "123-45") {
		t.Fatalf("unexpected ciphertext form %q", a)
	}
	got, err := c.Decrypt(a, rowAAD)
	if err != nil || string(got) != "123-45-6789" {
		t.Fatalf("Decrypt = %q, %v", got, err)
	}
	empty, err := c.Encrypt(nil, rowAAD)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.Decrypt(empty, rowAAD); err != nil || len(got) != 0 {
		t.Fatalf("empty round trip = %q, %v", got, err)
	}
}

func TestWrongKeyAndTamperingAreRejected(t *testing.T) {
	t.Parallel()
	c := newCipher(t, testKey(t, "k1", 1))
	sealed, err := c.Encrypt([]byte("secret"), rowAAD)
	if err != nil {
		t.Fatal(err)
	}
	other := newCipher(t, testKey(t, "k1", 2)) // same id, different material
	if _, err := other.Decrypt(sealed, rowAAD); !errors.Is(err, encryption.ErrDecrypt) {
		t.Fatalf("wrong key: %v", err)
	}
	unknown := newCipher(t, testKey(t, "k2", 1))
	if _, err := unknown.Decrypt(sealed, rowAAD); !errors.Is(err, encryption.ErrUnknownKey) {
		t.Fatalf("unknown key id: %v", err)
	}

	tests := map[string]encryption.AAD{
		"another row":    {Table: "people", Column: "ssn", RecordID: "0190f1c0-0000-7000-8000-000000000002"},
		"another column": {Table: "people", Column: "tax_id", RecordID: rowAAD.RecordID},
		"another table":  {Table: "customers", Column: "ssn", RecordID: rowAAD.RecordID},
		"shifted fields": {Table: "peoplessn", Column: "", RecordID: rowAAD.RecordID},
	}
	for name, aad := range tests {
		if _, err := c.Decrypt(sealed, aad); err == nil {
			t.Errorf("%s: ciphertext was accepted", name)
		}
	}
	// A ciphertext sealed under another purpose of the same ring must not open
	// with a tampered payload: flip every character position class.
	for _, i := range []int{len("fg1.k1.") + 1, len(sealed) / 2, len(sealed) - 1} {
		tampered := []byte(sealed)
		if tampered[i] == 'A' {
			tampered[i] = 'B'
		} else {
			tampered[i] = 'A'
		}
		if _, err := c.Decrypt(string(tampered), rowAAD); err == nil {
			t.Errorf("tampered byte %d accepted", i)
		}
	}
	for _, bad := range []string{"", "fg1", "fg1.k1", "fg1.k1.", "fg2.k1.AAAA", "fg1.k!.AAAA", "fg1.k1.***", "plain text"} {
		if _, err := c.Decrypt(bad, rowAAD); err == nil {
			t.Errorf("malformed %q accepted", bad)
		}
	}
	if _, err := c.Encrypt([]byte("x"), encryption.AAD{Table: "people", Column: "ssn"}); err == nil {
		t.Error("an empty record id was accepted")
	}
}

func TestKeyIDIsAuthenticated(t *testing.T) {
	t.Parallel()
	// Same material under two ids: renaming the id in the ciphertext must fail,
	// because the id is part of the associated data.
	c := newCipher(t, testKey(t, "k1", 7), testKey(t, "k2", 7))
	sealed, err := c.Encrypt([]byte("v"), rowAAD)
	if err != nil {
		t.Fatal(err)
	}
	renamed := strings.Replace(sealed, ".k1.", ".k2.", 1)
	if _, err := c.Decrypt(renamed, rowAAD); !errors.Is(err, encryption.ErrDecrypt) {
		t.Fatalf("renamed key id: %v", err)
	}
}

func TestRotationKeepsOldValuesReadable(t *testing.T) {
	t.Parallel()
	oldKey, newKey := testKey(t, "2025", 1), testKey(t, "2026", 2)
	before := newCipher(t, oldKey)
	sealedOld, err := before.Encrypt([]byte("legacy"), rowAAD)
	if err != nil {
		t.Fatal(err)
	}
	after := newCipher(t, newKey, oldKey)
	if got, err := after.Decrypt(sealedOld, rowAAD); err != nil || string(got) != "legacy" {
		t.Fatalf("old value with ring: %q, %v", got, err)
	}
	sealedNew, err := after.Encrypt([]byte("fresh"), rowAAD)
	if err != nil || !strings.HasPrefix(sealedNew, "fg1.2026.") {
		t.Fatalf("new value must use the active key: %q, %v", sealedNew, err)
	}
	if stale, err := after.NeedsRotation(sealedOld); err != nil || !stale {
		t.Fatalf("NeedsRotation(old) = %v, %v", stale, err)
	}
	if stale, err := after.NeedsRotation(sealedNew); err != nil || stale {
		t.Fatalf("NeedsRotation(new) = %v, %v", stale, err)
	}
	// Dropping the old key from the ring makes old values unreadable.
	retired := newCipher(t, newKey)
	if _, err := retired.Decrypt(sealedOld, rowAAD); !errors.Is(err, encryption.ErrUnknownKey) {
		t.Fatalf("retired key: %v", err)
	}
}

func TestBlindIndex(t *testing.T) {
	t.Parallel()
	oldKey, newKey := testKey(t, "k1", 1), testKey(t, "k2", 2)
	c := newCipher(t, oldKey)
	a, err := c.BlindIndex("people", "ssn", []byte("ssn#123"))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := c.BlindIndex("people", "ssn", []byte("ssn#123")); a != b {
		t.Fatal("blind index is not stable for equal input")
	}
	for name, other := range map[string]func() (string, error){
		"another value":  func() (string, error) { return c.BlindIndex("people", "ssn", []byte("ssn#124")) },
		"another column": func() (string, error) { return c.BlindIndex("people", "tax_id", []byte("ssn#123")) },
		"another table":  func() (string, error) { return c.BlindIndex("customers", "ssn", []byte("ssn#123")) },
		"another key":    func() (string, error) { return newCipher(t, newKey).BlindIndex("people", "ssn", []byte("ssn#123")) },
	} {
		got, err := other()
		if err != nil || got == a {
			t.Errorf("%s: index %q equals the original (err %v)", name, got, err)
		}
	}
	// The index key is separate from the encryption key: the digest of a
	// value is not derivable from, nor equal to, anything in the ciphertext.
	if strings.Contains(a, "ssn#123") {
		t.Fatal("index leaks the value")
	}
	rotated := newCipher(t, newKey, oldKey)
	all, err := rotated.BlindIndexes("people", "ssn", []byte("ssn#123"))
	if err != nil || len(all) != 2 || all[1] != a || all[0] == a {
		t.Fatalf("BlindIndexes = %v, %v; want [new, old=%s]", all, err, a)
	}
	// Encryption stays randomized even though a blind index is available.
	x, _ := c.Encrypt([]byte("ssn#123"), rowAAD)
	y, _ := c.Encrypt([]byte("ssn#123"), rowAAD)
	if x == y {
		t.Fatal("encryption became deterministic")
	}
}

func TestKeyRingValidationAndRedaction(t *testing.T) {
	t.Parallel()
	good := testKey(t, "k1", 1)
	cases := map[string][]encryption.Key{
		"short key":    {{ID: "k1", Material: []byte("short")}},
		"bad id":       {{ID: "bad id", Material: good.Material}},
		"empty id":     {{ID: "", Material: good.Material}},
		"duplicate id": {good, good},
	}
	for name, keys := range cases {
		if _, err := encryption.NewKeyRing(keys[0], keys[1:]...); !errors.Is(err, encryption.ErrInvalidKey) {
			t.Errorf("%s: %v", name, err)
		}
	}
	material := testKey(t, "k1", 9).Material
	text := encryption.EncodeKey(material)
	if _, err := encryption.ParseKey(text + "="); err == nil {
		t.Error("padded key accepted")
	}
	if _, err := encryption.ParseKey("short"); err == nil || strings.Contains(err.Error(), "short") {
		t.Errorf("error must not echo the input: %v", err)
	}
	if got, err := encryption.ParseKey(text); err != nil || !bytes.Equal(got, material) {
		t.Fatalf("ParseKey = %v, %v", got, err)
	}
	key := encryption.Key{ID: "k1", Material: material}
	var buf strings.Builder
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "key", key)
	for _, out := range []string{buf.String(), fmt.Sprintf("%v %+v %#v %s", key, key, key, key)} {
		if strings.Contains(out, text) || strings.Contains(out, string(material)) {
			t.Errorf("key material leaked: %s", out)
		}
	}
	ring, err := encryption.ParseKeyRing("b", map[string]string{"a": encryption.EncodeKey(good.Material), "b": text})
	if err != nil || ring.ActiveID() != "b" || strings.Join(ring.IDs(), ",") != "b,a" {
		t.Fatalf("ParseKeyRing: %v %v", ring, err)
	}
	if _, err := encryption.ParseKeyRing("missing", map[string]string{"a": text}); err == nil {
		t.Error("an active key that is not in the map was accepted")
	}
}

func TestDeriveKeySeparatesPurposes(t *testing.T) {
	t.Parallel()
	master := bytes.Repeat([]byte{3}, encryption.KeySize)
	a, _ := encryption.DeriveKey(master, "credentials")
	b, _ := encryption.DeriveKey(master, "attributes/aead")
	again, _ := encryption.DeriveKey(master, "credentials")
	if bytes.Equal(a, b) || !bytes.Equal(a, again) || bytes.Equal(a, master) {
		t.Fatal("purpose separation is broken")
	}
	if _, err := encryption.DeriveKey(master[:16], "x"); err == nil {
		t.Error("short master accepted")
	}
}

func TestAssociatedDataIsUnambiguous(t *testing.T) {
	t.Parallel()
	if bytes.Equal(encryption.AssociatedData("ab", "c"), encryption.AssociatedData("a", "bc")) {
		t.Fatal("AssociatedData is ambiguous")
	}
}

func TestSizeLimits(t *testing.T) {
	t.Parallel()
	c := newCipher(t, testKey(t, "k1", 1))
	if _, err := c.Encrypt(make([]byte, encryption.MaxPlaintextBytes+1), rowAAD); !errors.Is(err, encryption.ErrTooLarge) {
		t.Fatalf("oversized plaintext: %v", err)
	}
	huge := "fg1.k1." + strings.Repeat("A", 4<<20)
	if _, err := c.Decrypt(huge, rowAAD); !errors.Is(err, encryption.ErrMalformed) {
		t.Fatalf("oversized ciphertext: %v", err)
	}
}

func TestErrorsNeverContainPlaintext(t *testing.T) {
	t.Parallel()
	c := newCipher(t, testKey(t, "k1", 1))
	secret := "hunter2-plaintext"
	sealed, _ := c.Encrypt([]byte(secret), rowAAD)
	_, err := c.Decrypt(sealed, encryption.AAD{Table: "t", Column: "c", RecordID: "r"})
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), sealed) {
		t.Fatalf("error leaks data: %v", err)
	}
}

type profile struct {
	Name string `json:"name"`
	Tags []string
}

func TestValueRoundTrip(t *testing.T) {
	t.Parallel()
	c := newCipher(t, testKey(t, "k1", 1))
	v := encryption.NewValue[string](c, "people", "ssn")
	if _, err := v.Value(); err != nil {
		t.Fatalf("absent value: %v", err)
	}
	v.Set("ssn#123")
	if _, err := v.Value(); !errors.Is(err, encryption.ErrNoRecordID) {
		t.Fatalf("missing record id: %v", err)
	}
	v.ForRecord(rowAAD.RecordID)
	driverValue, err := v.Value()
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := driverValue.(string)
	if !ok || strings.Contains(stored, "ssn#123") {
		t.Fatalf("driver value = %#v", driverValue)
	}

	scanned := encryption.NewValue[string](c, "people", "ssn")
	if err := scanned.Scan([]byte(stored)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := scanned.Get(); !errors.Is(err, encryption.ErrNoRecordID) {
		t.Fatalf("Get without record id: %v", err)
	}
	got, present, err := scanned.ForRecord(rowAAD.RecordID).Get()
	if err != nil || !present || got != "ssn#123" {
		t.Fatalf("Get = %q, %v, %v", got, present, err)
	}

	// The same ciphertext on another row or column must not decrypt.
	swapped := encryption.NewValue[string](c, "people", "ssn")
	_ = swapped.Scan(stored)
	if _, _, err := swapped.ForRecord("another-row").Get(); !errors.Is(err, encryption.ErrDecrypt) {
		t.Fatalf("row swap: %v", err)
	}
	wrongColumn := encryption.NewValue[string](c, "people", "tax_id")
	_ = wrongColumn.Scan(stored)
	if _, _, err := wrongColumn.ForRecord(rowAAD.RecordID).Get(); !errors.Is(err, encryption.ErrDecrypt) {
		t.Fatalf("column swap: %v", err)
	}

	null := encryption.NewValue[string](c, "people", "ssn")
	if err := null.Scan(nil); err != nil {
		t.Fatal(err)
	}
	if _, present, err := null.Get(); err != nil || present {
		t.Fatalf("NULL: present=%v err=%v", present, err)
	}
	if err := null.Scan(42); err == nil {
		t.Error("an integer column was accepted")
	}
}

func TestValueGenericTypes(t *testing.T) {
	t.Parallel()
	c := newCipher(t, testKey(t, "k1", 1))
	aad := rowAAD
	structured := encryption.NewValue[profile](c, aad.Table, "profile").ForRecord(aad.RecordID)
	structured.Set(profile{Name: "Ada", Tags: []string{"a", "b"}})
	stored, err := structured.Value()
	if err != nil {
		t.Fatal(err)
	}
	back := encryption.NewValue[profile](c, aad.Table, "profile").ForRecord(aad.RecordID)
	if err := back.Scan(stored); err != nil {
		t.Fatal(err)
	}
	if got, _, err := back.Get(); err != nil || got.Name != "Ada" || len(got.Tags) != 2 {
		t.Fatalf("struct round trip: %+v %v", got, err)
	}
	blob := encryption.NewValue[[]byte](c, aad.Table, "blob").ForRecord(aad.RecordID)
	blob.Set([]byte{0, 1, 2, 255})
	stored, _ = blob.Value()
	again := encryption.NewValue[[]byte](c, aad.Table, "blob").ForRecord(aad.RecordID)
	_ = again.Scan(stored)
	if got, _, err := again.Get(); err != nil || !bytes.Equal(got, []byte{0, 1, 2, 255}) {
		t.Fatalf("bytes round trip: %v %v", got, err)
	}
	// Re-saving a scanned value re-encrypts it under the active key.
	rotated := newCipher(t, testKey(t, "k2", 2), testKey(t, "k1", 1))
	old := encryption.NewValue[string](c, aad.Table, "ssn").ForRecord(aad.RecordID)
	old.Set("x")
	oldText, _ := old.Value()
	resave := encryption.NewValue[string](rotated, aad.Table, "ssn").ForRecord(aad.RecordID)
	_ = resave.Scan(oldText)
	newText, err := resave.Value()
	if err != nil || !strings.HasPrefix(newText.(string), "fg1.k2.") {
		t.Fatalf("resave = %v, %v", newText, err)
	}
}

type memoryColumn struct {
	table, column string
	rows          map[string]string // id -> sealed
	pageErr       error
	beforeReplace func(id string)
	replaced      int
}

func (m *memoryColumn) Table() string  { return m.table }
func (m *memoryColumn) Column() string { return m.column }

func (m *memoryColumn) Page(_ context.Context, after string, limit int) ([]encryption.Record, error) {
	if m.pageErr != nil {
		return nil, m.pageErr
	}
	var ids []string
	for id := range m.rows {
		if id > after {
			ids = append(ids, id)
		}
	}
	for i := range ids { // insertion sort keeps the test dependency-free
		for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
	if len(ids) > limit {
		ids = ids[:limit]
	}
	page := make([]encryption.Record, 0, len(ids))
	for _, id := range ids {
		page = append(page, encryption.Record{ID: id, Sealed: m.rows[id]})
	}
	return page, nil
}

func (m *memoryColumn) Replace(_ context.Context, u encryption.Update) (bool, error) {
	if m.beforeReplace != nil {
		m.beforeReplace(u.ID)
	}
	if m.rows[u.ID] != u.OldSealed {
		return false, nil
	}
	m.rows[u.ID] = u.NewSealed
	m.replaced++
	return true, nil
}

func TestReencryptBatchedAndResumable(t *testing.T) {
	t.Parallel()
	oldKey, newKey := testKey(t, "k1", 1), testKey(t, "k2", 2)
	before := newCipher(t, oldKey)
	col := &memoryColumn{table: "people", column: "ssn", rows: map[string]string{}}
	for i := range 25 {
		id := fmt.Sprintf("id-%03d", i)
		sealed, err := before.Encrypt([]byte("v"+id), encryption.AAD{Table: "people", Column: "ssn", RecordID: id})
		if err != nil {
			t.Fatal(err)
		}
		col.rows[id] = sealed
	}
	after := newCipher(t, newKey, oldKey)

	ctx, cancel := context.WithCancel(context.Background())
	col.beforeReplace = func(id string) {
		if id == "id-012" {
			cancel() // interrupt mid-run
		}
	}
	report, err := after.Reencrypt(ctx, col, encryption.RotateOptions{BatchSize: 5})
	if !errors.Is(err, context.Canceled) || report.LastID != "id-012" {
		t.Fatalf("interrupted run: %+v, %v", report, err)
	}
	col.beforeReplace = nil
	resumed, err := after.Reencrypt(context.Background(), col, encryption.RotateOptions{BatchSize: 5, After: report.LastID})
	if err != nil || resumed.Scanned != 12 || resumed.Rotated != 12 || resumed.LastID != "id-024" {
		t.Fatalf("resumed run: %+v, %v", resumed, err)
	}
	for id, sealed := range col.rows {
		if !strings.HasPrefix(sealed, "fg1.k2.") {
			t.Fatalf("row %s still on the old key", id)
		}
		got, err := after.Decrypt(sealed, encryption.AAD{Table: "people", Column: "ssn", RecordID: id})
		if err != nil || string(got) != "v"+id {
			t.Fatalf("row %s: %q %v", id, got, err)
		}
	}
	// Retiring the old key is now safe, and a second run is a no-op.
	retired := newCipher(t, newKey)
	rerun, err := retired.Reencrypt(context.Background(), col, encryption.RotateOptions{})
	if err != nil || rerun.Rotated != 0 || rerun.Skipped != 25 {
		t.Fatalf("idempotent rerun: %+v, %v", rerun, err)
	}
}

func TestReencryptConflictAndFailures(t *testing.T) {
	t.Parallel()
	oldKey, newKey := testKey(t, "k1", 1), testKey(t, "k2", 2)
	before := newCipher(t, oldKey)
	after := newCipher(t, newKey, oldKey)
	aad := encryption.AAD{Table: "people", Column: "ssn", RecordID: "a"}
	sealed, _ := before.Encrypt([]byte("v"), aad)

	col := &memoryColumn{table: "people", column: "ssn", rows: map[string]string{"a": sealed}}
	col.beforeReplace = func(id string) { col.rows[id] = "changed-by-app" }
	report, err := after.Reencrypt(context.Background(), col, encryption.RotateOptions{})
	if err != nil || report.Conflicts != 1 || report.Rotated != 0 || col.rows["a"] != "changed-by-app" {
		t.Fatalf("conflict must not overwrite a concurrent write: %+v %v", report, err)
	}

	// A row sealed for another row (swapped) stops the run, naming the id only.
	other, _ := before.Encrypt([]byte("topsecret"), encryption.AAD{Table: "people", Column: "ssn", RecordID: "zzz"})
	bad := &memoryColumn{table: "people", column: "ssn", rows: map[string]string{"a": other}}
	_, err = after.Reencrypt(context.Background(), bad, encryption.RotateOptions{})
	if !errors.Is(err, encryption.ErrDecrypt) || !strings.Contains(err.Error(), `"a"`) || strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("swapped row: %v", err)
	}
	if _, err := after.Reencrypt(context.Background(), col, encryption.RotateOptions{BatchSize: -1}); err == nil {
		t.Error("negative batch size accepted")
	}
	failing := &memoryColumn{table: "people", column: "ssn", pageErr: errors.New("db down")}
	if _, err := after.Reencrypt(context.Background(), failing, encryption.RotateOptions{}); err == nil {
		t.Error("page error swallowed")
	}
}

type indexedColumn struct {
	memoryColumn
	index map[string]string
}

func (m *indexedColumn) Replace(ctx context.Context, u encryption.Update) (bool, error) {
	ok, err := m.memoryColumn.Replace(ctx, u)
	if ok {
		m.index[u.ID] = u.Index
	}
	return ok, err
}

func TestReencryptWritesBlindIndex(t *testing.T) {
	t.Parallel()
	c := newCipher(t, testKey(t, "k1", 1))
	aad := encryption.AAD{Table: "people", Column: "ssn", RecordID: "a"}
	sealed, _ := c.Encrypt([]byte("123"), aad)
	col := &indexedColumn{memoryColumn: memoryColumn{table: "people", column: "ssn", rows: map[string]string{"a": sealed}}, index: map[string]string{}}
	report, err := c.Reencrypt(context.Background(), col, encryption.RotateOptions{BlindIndex: true})
	if err != nil || report.Reindexed != 1 || report.Rotated != 0 {
		t.Fatalf("report %+v, %v", report, err)
	}
	want, _ := c.BlindIndex("people", "ssn", []byte("123"))
	if col.index["a"] != want || col.rows["a"] != sealed {
		t.Fatalf("index %q, want %q; sealed unchanged=%v", col.index["a"], want, col.rows["a"] == sealed)
	}
}
