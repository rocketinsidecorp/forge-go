package credentials

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rocketinsidecorp/forge-go/encryption"
)

// sealUnchecked produces a file Seal would never write: authenticated under
// key, but holding whatever bytes it is given.
func sealUnchecked(t *testing.T, env string, plaintext []byte, key encryption.Key) []byte {
	t.Helper()
	aead, err := newAEAD(key)
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := encryption.NewNonce(aead)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(envelope{
		Version:    FormatVersion,
		Algorithm:  algorithm,
		KeyID:      key.ID,
		Nonce:      base64.RawURLEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawURLEncoding.EncodeToString(aead.Seal(nil, nonce, plaintext, additionalData(env, FormatVersion, key.ID))),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A file that authenticates but does not hold a valid document (whoever has
// the key can write one) is refused by Open in every environment, without
// echoing what it holds.
func TestOpenRefusesAnAuthenticFileWithAnInvalidDocument(t *testing.T) {
	t.Parallel()
	material := bytes.Repeat([]byte{9}, encryption.KeySize)
	key, err := NewKey(material)
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(string) (string, bool) { return encryption.EncodeKey(material), true }
	for _, env := range []string{"development", "production"} {
		for name, plaintext := range map[string]string{
			"not JSON":      `hunter2`,
			"duplicate key": `{"a": 1, "a": "hunter2"}`,
			"an array":      `["hunter2"]`,
		} {
			fsys := fstest.MapFS{"config/credentials/" + env + ".json.enc": {Data: sealUnchecked(t, env, []byte(plaintext), key)}}
			store, err := Open(env, Options{FS: fsys, Lookup: lookup})
			if err == nil || store != nil || strings.Contains(err.Error(), "hunter2") {
				t.Errorf("%s, %s: Open = %v, %v", env, name, store, err)
			}
		}
	}
}

func TestJSONTypeNamesEveryKindOfValue(t *testing.T) {
	t.Parallel()
	for want, values := range map[string][]any{
		"null":            {nil},
		"a string":        {"x"},
		"a number":        {json.Number("1"), 1.5},
		"a boolean":       {true, false},
		"an array":        {[]any{}},
		"an object":       {map[string]any{}},
		"an unknown type": {struct{}{}, 3},
	} {
		for _, value := range values {
			if got := jsonType(value); got != want {
				t.Errorf("jsonType(%#v) = %q, want %q", value, got, want)
			}
		}
	}
}

func TestIsEmptyOnlyCountsNothingAndEmptyContainers(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		value any
		empty bool
	}{
		{"nil", nil, true},
		{"empty string", "", true},
		{"empty object", map[string]any{}, true},
		{"empty array", []any{}, true},
		{"text", "x", false},
		{"zero", json.Number("0"), false},
		{"false", false, false},
		{"object", map[string]any{"k": 1}, false},
		{"array", []any{1}, false},
	} {
		if got := isEmpty(tt.value); got != tt.empty {
			t.Errorf("isEmpty(%s) = %v, want %v", tt.name, got, tt.empty)
		}
	}
}

// %#v goes through Format, but the GoStringer is part of the interface and
// must not reveal anything either.
func TestGoStringIsRedacted(t *testing.T) {
	t.Parallel()
	store, err := Parse("production", []byte(`{"k": "hunter2"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := store.GoString(); got != "credentials.Store(production, [REDACTED])" {
		t.Errorf("GoString = %q", got)
	}
	var zero Store
	if got := zero.GoString(); strings.Contains(got, "hunter2") || zero.Exists() || zero.Environment() != "" {
		t.Errorf("zero Store: %q", got)
	}
	if _, err := Parse("production", nil); err == nil || errors.Is(err, ErrInvalidEnvironment) {
		t.Errorf("Parse of an empty document: %v", err)
	}
}
