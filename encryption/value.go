package encryption

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNoRecordID is returned when a Value is used before ForRecord.
var ErrNoRecordID = errors.New("encryption: record id is not set; call ForRecord first")

// Value is one encrypted attribute as it crosses the database boundary. As a
// driver.Valuer it encrypts the plaintext into a TEXT value; as a sql.Scanner
// it keeps the stored ciphertext, which Get decrypts. The ciphertext is bound
// to the table, column and record id, so ForRecord must name the row before
// Value or Get is used. Bind one Value per column and row.
//
//	ssn := encryption.NewValue[string](cipher, "people", "ssn").ForRecord(id)
//	ssn.Set("123-45-6789")
//	_, err := db.ExecContext(ctx, `INSERT INTO people (id, ssn) VALUES ($1, $2)`, id, ssn)
//
//	var id string
//	ssn := encryption.NewValue[string](cipher, "people", "ssn")
//	err := db.QueryRowContext(ctx, `SELECT id, ssn FROM people WHERE id = $1`, key).Scan(&id, ssn)
//	plain, present, err := ssn.ForRecord(id).Get()
//
// String and []byte values are stored as they are; every other type is
// stored as JSON. NULL columns scan to an absent value and an absent value
// is written as NULL.
type Value[T any] struct {
	cipher   *Cipher
	table    string
	column   string
	recordID string
	plain    T
	sealed   string
	present  bool
	loaded   bool // plain holds the decrypted value of sealed
}

var (
	_ driver.Valuer = (*Value[string])(nil)
	_ sql.Scanner   = (*Value[string])(nil)
)

// NewValue returns an empty Value for table.column.
func NewValue[T any](cipher *Cipher, table, column string) *Value[T] {
	return &Value[T]{cipher: cipher, table: table, column: column}
}

// ForRecord sets the id of the row the value belongs to and returns v.
func (v *Value[T]) ForRecord(id string) *Value[T] {
	if v.recordID != id {
		v.recordID = id
		// A scanned ciphertext is authenticated for one row; decrypt it again
		// for the new one (and fail if it does not belong there).
		if v.sealed != "" {
			v.loaded = false
		}
	}
	return v
}

// Set stores a plaintext to be encrypted by Value.
func (v *Value[T]) Set(plain T) {
	v.plain, v.present, v.loaded, v.sealed = plain, true, true, ""
}

// Clear marks the value absent; Value then yields NULL.
func (v *Value[T]) Clear() {
	var zero T
	v.plain, v.present, v.loaded, v.sealed = zero, false, true, ""
}

func (v *Value[T]) aad() (AAD, error) {
	if v.cipher == nil {
		return AAD{}, errors.New("encryption: cipher is required")
	}
	if v.recordID == "" {
		return AAD{}, ErrNoRecordID
	}
	return AAD{Table: v.table, Column: v.column, RecordID: v.recordID}, nil
}

// Get returns the plaintext and whether the column held a value. It decrypts
// the scanned ciphertext once and caches the result.
func (v *Value[T]) Get() (T, bool, error) {
	var zero T
	if !v.present {
		return zero, false, nil
	}
	if v.loaded {
		return v.plain, true, nil
	}
	aad, err := v.aad()
	if err != nil {
		return zero, false, err
	}
	raw, err := v.cipher.Decrypt(v.sealed, aad)
	if err != nil {
		return zero, false, fmt.Errorf("decrypt %s.%s: %w", v.table, v.column, err)
	}
	plain, err := decodeValue[T](raw)
	clear(raw)
	if err != nil {
		return zero, false, fmt.Errorf("decode %s.%s: %w", v.table, v.column, err)
	}
	v.plain, v.loaded = plain, true
	return plain, true, nil
}

// Value implements driver.Valuer.
func (v *Value[T]) Value() (driver.Value, error) {
	if !v.present {
		return nil, nil
	}
	// A scanned value is decrypted first, which also proves it belongs to
	// this row before it is re-encrypted for it.
	if _, _, err := v.Get(); err != nil {
		return nil, err
	}
	aad, err := v.aad()
	if err != nil {
		return nil, err
	}
	raw, err := encodeValue(v.plain)
	if err != nil {
		return nil, err
	}
	text, err := v.cipher.Encrypt(raw, aad)
	clear(raw)
	if err != nil {
		return nil, err
	}
	return text, nil
}

// Scan implements sql.Scanner. It stores the ciphertext without decrypting.
func (v *Value[T]) Scan(src any) error {
	var zero T
	switch s := src.(type) {
	case nil:
		v.plain, v.sealed, v.present, v.loaded = zero, "", false, true
	case string:
		v.plain, v.sealed, v.present, v.loaded = zero, s, true, false
	case []byte:
		v.plain, v.sealed, v.present, v.loaded = zero, string(s), true, false
	default:
		return fmt.Errorf("encryption: cannot scan %T into an encrypted value", src)
	}
	return nil
}

func encodeValue[T any](v T) ([]byte, error) {
	switch x := any(v).(type) {
	case string:
		return []byte(x), nil
	case []byte:
		return append([]byte(nil), x...), nil
	}
	return json.Marshal(v)
}

func decodeValue[T any](raw []byte) (T, error) {
	var out T
	switch p := any(&out).(type) {
	case *string:
		*p = string(raw)
		return out, nil
	case *[]byte:
		*p = append([]byte(nil), raw...)
		return out, nil
	}
	err := json.Unmarshal(raw, &out)
	return out, err
}
