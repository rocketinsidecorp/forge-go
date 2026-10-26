// Package encryption provides authenticated attribute encryption with key
// rotation, built only from the standard library (AES-256-GCM, HKDF-SHA256 and
// HMAC-SHA256). It is the shared foundation of encrypted credentials
// (package credentials) and of encrypted database columns.
//
// Every ciphertext is bound to its location through associated data (table,
// column and record id), so a value copied to another row or column fails to
// decrypt instead of being accepted. Encryption is always randomized.
// Equality lookups use a keyed blind index (HMAC-SHA256), never deterministic
// encryption, and only when the application asks for one explicitly.
package encryption

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// KeySize is the size of a master key in bytes.
const KeySize = 32

// maxKeyIDLength bounds key identifiers, which are stored beside every
// ciphertext.
const maxKeyIDLength = 32

var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

const redacted = "[REDACTED]"

// ErrInvalidKey reports key material or an identifier that cannot be used.
// Messages never include the key.
var ErrInvalidKey = errors.New("encryption: invalid key")

// GenerateKey returns a new random 256-bit master key.
func GenerateKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("encryption: generate key: %w", err)
	}
	return key, nil
}

// EncodeKey returns the canonical text form of a key: unpadded base64url.
func EncodeKey(key []byte) string { return base64.RawURLEncoding.EncodeToString(key) }

// ParseKey decodes the canonical text form of a key. It accepts exactly 32
// bytes of unpadded base64url and never echoes the input.
func ParseKey(encoded string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) != KeySize || EncodeKey(raw) != encoded {
		return nil, fmt.Errorf("%w: must be canonical unpadded base64url for exactly %d bytes", ErrInvalidKey, KeySize)
	}
	return raw, nil
}

// ValidKeyID reports whether id is a usable key identifier: 1 to 32 letters,
// digits, underscores or hyphens.
func ValidKeyID(id string) bool { return keyIDPattern.MatchString(id) }

// DeriveKey derives a 256-bit key for one purpose from a master key with
// HKDF-SHA256. Different purposes yield unrelated keys, so one master key can
// safely serve encryption and blind indexing.
func DeriveKey(master []byte, purpose string) ([]byte, error) {
	if len(master) != KeySize {
		return nil, fmt.Errorf("%w: master key must be %d bytes", ErrInvalidKey, KeySize)
	}
	if purpose == "" {
		return nil, errors.New("encryption: key purpose is required")
	}
	return hkdf.Key(sha256.New, master, nil, "forge/v1/"+purpose, KeySize)
}

// Key is a master key and its identifier. The identifier is stored beside
// ciphertexts and is not secret. Formatting and logging print [REDACTED].
type Key struct {
	ID       string
	Material []byte
}

func (Key) String() string { return "encryption.Key(" + redacted + ")" }

// GoString prints [REDACTED] in %#v output.
func (Key) GoString() string { return "encryption.Key(" + redacted + ")" }

// LogValue logs [REDACTED].
func (Key) LogValue() slog.Value { return slog.StringValue(redacted) }

// KeyRing holds the active key, used for every new ciphertext, and previous
// keys, which remain usable for decryption only. It is immutable. Identifiers
// are unique ignoring ASCII letter case (K1 and k1 cannot share a ring), so a
// count of values by key id prefix means the same under a case-insensitive
// database collation (MySQL and MariaDB defaults) as under a binary one.
type KeyRing struct {
	active Key
	keys   map[string]Key
	ids    []string
}

// NewKeyRing builds a ring. Identifiers must be valid and unique ignoring
// letter case, and every key must be 32 bytes. Key material is copied.
func NewKeyRing(active Key, previous ...Key) (*KeyRing, error) {
	ring := &KeyRing{keys: make(map[string]Key, 1+len(previous))}
	for i, key := range append([]Key{active}, previous...) {
		if !ValidKeyID(key.ID) {
			return nil, fmt.Errorf("%w: identifier must be 1-%d letters, digits, '_' or '-'", ErrInvalidKey, maxKeyIDLength)
		}
		if len(key.Material) != KeySize {
			return nil, fmt.Errorf("%w: key %q must be %d bytes", ErrInvalidKey, key.ID, KeySize)
		}
		for _, existing := range ring.ids {
			if strings.EqualFold(existing, key.ID) {
				return nil, fmt.Errorf("%w: duplicate key identifier %q (identifiers are compared ignoring letter case)", ErrInvalidKey, key.ID)
			}
		}
		copied := Key{ID: key.ID, Material: append([]byte(nil), key.Material...)}
		ring.keys[key.ID] = copied
		ring.ids = append(ring.ids, key.ID)
		if i == 0 {
			ring.active = copied
		}
	}
	return ring, nil
}

// ParseKeyRing builds a ring from an active identifier and a map of
// identifier to canonical text keys, the shape stored in credentials.
// Previous keys are ordered by identifier; identifiers equal ignoring letter
// case are refused.
//
// Errors never echo an identifier or the active value: one swapped with a key
// by mistake would otherwise print the key. They name a key by its position
// among the identifiers in sorted order, and the active value by its length.
func ParseKeyRing(active string, keys map[string]string) (*KeyRing, error) {
	if _, ok := keys[active]; !ok {
		return nil, fmt.Errorf("%w: the active identifier (%d characters) is not among the %d keys",
			ErrInvalidKey, utf8.RuneCountInString(active), len(keys))
	}
	ids := make([]string, 0, len(keys))
	for id := range keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var activeKey Key
	previous := make([]Key, 0, len(ids)-1)
	for position, id := range ids {
		material, err := ParseKey(keys[id])
		if err != nil {
			return nil, fmt.Errorf("key #%d: %w", position+1, err)
		}
		if !ValidKeyID(id) {
			return nil, fmt.Errorf("key #%d: %w: identifier must be 1-%d letters, digits, '_' or '-'",
				position+1, ErrInvalidKey, maxKeyIDLength)
		}
		for earlier, other := range ids[:position] {
			if strings.EqualFold(other, id) {
				return nil, fmt.Errorf("keys #%d and #%d: %w: duplicate key identifier (identifiers are compared ignoring letter case)",
					earlier+1, position+1, ErrInvalidKey)
			}
		}
		if id == active {
			activeKey = Key{ID: id, Material: material}
		} else {
			previous = append(previous, Key{ID: id, Material: material})
		}
	}
	return NewKeyRing(activeKey, previous...)
}

// ActiveID returns the identifier of the key used for new ciphertexts.
func (r *KeyRing) ActiveID() string { return r.active.ID }

// IDs returns every identifier, the active one first.
func (r *KeyRing) IDs() []string { return append([]string(nil), r.ids...) }
