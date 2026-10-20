package encryption

import (
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	// MaxPlaintextBytes bounds one encrypted attribute.
	MaxPlaintextBytes = 1 << 20

	textPrefix = "fg1"

	purposeAttributes = "attributes/aead"
	purposeBlindIndex = "attributes/blind-index"
	adDomain          = "forge/attribute/v1"
	indexDomain       = "forge/blind-index/v1"
)

var (
	// ErrMalformed reports a value that is not a Forge ciphertext.
	ErrMalformed = errors.New("encryption: malformed ciphertext")
	// ErrUnknownKey reports a ciphertext sealed with a key identifier the
	// ring does not hold, such as a retired key that was removed too early.
	ErrUnknownKey = errors.New("encryption: unknown key identifier")
	// ErrTooLarge reports a plaintext or ciphertext over MaxPlaintextBytes.
	ErrTooLarge = errors.New("encryption: value is too large")
)

// AAD identifies where a value lives. It is authenticated with the value, so
// a ciphertext copied to another table, column or row fails to decrypt.
// Every field is required; RecordID should be the row's immutable primary key
// (UUIDv7 in Forge applications).
type AAD struct {
	Table    string
	Column   string
	RecordID string
}

func (a AAD) valid() error {
	if a.Table == "" || a.Column == "" || a.RecordID == "" {
		return errors.New("encryption: table, column and record id are required")
	}
	return nil
}

type keyset struct {
	id    string
	aead  cipher.AEAD
	index []byte
}

// Cipher encrypts and decrypts attribute values with a key ring. It is
// immutable and safe for concurrent use.
type Cipher struct {
	ring   *KeyRing
	active *keyset
	sets   map[string]*keyset
}

// NewCipher derives the per-purpose keys of every ring key with HKDF and
// returns a Cipher that writes with the active key and reads with any.
func NewCipher(ring *KeyRing) (*Cipher, error) {
	if ring == nil {
		return nil, errors.New("encryption: key ring is required")
	}
	c := &Cipher{ring: ring, sets: make(map[string]*keyset, len(ring.keys))}
	for _, id := range ring.ids {
		key := ring.keys[id]
		aeadKey, err := DeriveKey(key.Material, purposeAttributes)
		if err != nil {
			return nil, err
		}
		aead, err := NewAEAD(aeadKey)
		clear(aeadKey)
		if err != nil {
			return nil, err
		}
		indexKey, err := DeriveKey(key.Material, purposeBlindIndex)
		if err != nil {
			return nil, err
		}
		set := &keyset{id: id, aead: aead, index: indexKey}
		c.sets[id] = set
		if id == ring.active.ID {
			c.active = set
		}
	}
	return c, nil
}

// ActiveKeyID returns the identifier used for new ciphertexts.
func (c *Cipher) ActiveKeyID() string { return c.active.id }

func associatedData(a AAD, kid string) []byte {
	return AssociatedData(adDomain, kid, a.Table, a.Column, a.RecordID)
}

// Encrypt seals plaintext with the active key and a fresh random nonce. The
// result is text of the form fg1.<key id>.<base64url(nonce||ciphertext||tag)>
// that fits any TEXT column. Two encryptions of the same value differ.
func (c *Cipher) Encrypt(plaintext []byte, aad AAD) (string, error) {
	if err := aad.valid(); err != nil {
		return "", err
	}
	if len(plaintext) > MaxPlaintextBytes {
		return "", ErrTooLarge
	}
	sealed, err := SealPrefixed(c.active.aead, plaintext, associatedData(aad, c.active.id))
	if err != nil {
		return "", err
	}
	return textPrefix + "." + c.active.id + "." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c *Cipher) parse(text string) (set *keyset, sealed []byte, err error) {
	prefix, rest, ok := strings.Cut(text, ".")
	if !ok || prefix != textPrefix {
		return nil, nil, ErrMalformed
	}
	kid, payload, ok := strings.Cut(rest, ".")
	if !ok || !ValidKeyID(kid) || len(payload) > base64.RawURLEncoding.EncodedLen(MaxPlaintextBytes+64) {
		return nil, nil, ErrMalformed
	}
	sealed, err = base64.RawURLEncoding.Strict().DecodeString(payload)
	if err != nil {
		return nil, nil, ErrMalformed
	}
	set = c.sets[kid]
	if set == nil {
		return nil, nil, fmt.Errorf("%w %q", ErrUnknownKey, kid)
	}
	return set, sealed, nil
}

// Decrypt opens a value produced by Encrypt for the same AAD. It picks the key
// by the identifier stored in the ciphertext, so values written before a
// rotation stay readable while the previous key is in the ring. A wrong key,
// modified ciphertext or different table, column or record id all fail with
// ErrDecrypt.
func (c *Cipher) Decrypt(text string, aad AAD) ([]byte, error) {
	if err := aad.valid(); err != nil {
		return nil, err
	}
	set, sealed, err := c.parse(text)
	if err != nil {
		return nil, err
	}
	return OpenPrefixed(set.aead, sealed, associatedData(aad, set.id))
}

// NeedsRotation reports whether text was sealed with a key other than the
// active one. Malformed values report an error.
func (c *Cipher) NeedsRotation(text string) (bool, error) {
	set, _, err := c.parse(text)
	if err != nil {
		return false, err
	}
	return set != c.active, nil
}

// BlindIndex returns a keyed digest of plaintext for equality lookups on an
// encrypted column: HMAC-SHA256 under a key derived for blind indexing, bound
// to the table and column. Equal plaintexts in one column give equal digests;
// nothing else is revealed without the key. This is an explicit opt-in: use
// it only for columns that must be searched by exact value, normalize the
// input (case, whitespace) before calling, and store the result in a separate
// indexed column.
func (c *Cipher) BlindIndex(table, column string, plaintext []byte) (string, error) {
	return c.blindIndex(c.active, table, column, plaintext)
}

// BlindIndexes returns the digests under every ring key, the active key first.
// Query with `WHERE index_column = ANY(...)` while a rotation is in flight, so
// rows indexed with the previous key are still found.
func (c *Cipher) BlindIndexes(table, column string, plaintext []byte) ([]string, error) {
	out := make([]string, 0, len(c.ring.ids))
	for _, id := range c.ring.ids {
		digest, err := c.blindIndex(c.sets[id], table, column, plaintext)
		if err != nil {
			return nil, err
		}
		out = append(out, digest)
	}
	return out, nil
}

func (c *Cipher) blindIndex(set *keyset, table, column string, plaintext []byte) (string, error) {
	if table == "" || column == "" {
		return "", errors.New("encryption: table and column are required")
	}
	if len(plaintext) > MaxPlaintextBytes {
		return "", ErrTooLarge
	}
	mac := hmac.New(sha256.New, set.index)
	mac.Write(AssociatedData(indexDomain, table, column))
	mac.Write(plaintext)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
