package credentials

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"

	"github.com/rocketinsidecorp/forge-go/encryption"
)

const (
	// FormatVersion is the version of the encrypted file format. It is
	// authenticated, so a file cannot be downgraded or reinterpreted.
	FormatVersion = 1

	algorithm = "AES-256-GCM"

	// MaxPlaintextBytes bounds a credentials document; MaxFileBytes bounds the
	// encrypted file.
	MaxPlaintextBytes = 1 << 20
	MaxFileBytes      = 2 << 20

	purposeAEAD  = "credentials/aead"
	purposeKeyID = "credentials/key-id"
	adDomain     = "forge/credentials"
)

var environmentPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

var (
	// ErrInvalidEnvironment reports an environment name that cannot name a file.
	ErrInvalidEnvironment = errors.New("credentials: invalid environment name")
	// ErrDecrypt reports a file that cannot be authenticated: wrong key, a
	// modified file, or a file belonging to another environment or format
	// version. The cases are deliberately indistinguishable.
	ErrDecrypt = errors.New("credentials: cannot decrypt (wrong key, modified file, or file from another environment)")
	// ErrNoMatchingKey reports that none of the available keys has the key id
	// recorded in the file.
	ErrNoMatchingKey = errors.New("credentials: no available key matches the file's key id")
	// ErrFormat reports a file that is not a Forge credentials file.
	ErrFormat = errors.New("credentials: invalid file format")
)

// ValidateEnvironment accepts 1 to 32 lowercase letters, digits, hyphens or
// underscores, starting with a letter. The name becomes part of a file name
// and of the authenticated data, so it must be unambiguous.
func ValidateEnvironment(env string) error {
	if !environmentPattern.MatchString(env) {
		return ErrInvalidEnvironment
	}
	return nil
}

// envelope is the stored form: a small JSON document that diffs and merges
// as one line per field. Only the nonce and ciphertext change between
// edits, and nothing in it is secret.
type envelope struct {
	Version    int    `json:"version"`
	Algorithm  string `json:"algorithm"`
	KeyID      string `json:"key_id"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// NewKey wraps 32 bytes of key material. Its identifier is a keyed
// fingerprint derived with HKDF, which names the key in the file header
// without revealing it.
func NewKey(material []byte) (encryption.Key, error) {
	idBytes, err := encryption.DeriveKey(material, purposeKeyID)
	if err != nil {
		return encryption.Key{}, err
	}
	return encryption.Key{ID: hex.EncodeToString(idBytes[:8]), Material: append([]byte(nil), material...)}, nil
}

// ParseKeys parses key text: canonical unpadded base64url keys separated by
// commas or newlines, the active key first. Blank lines and lines starting
// with '#' are ignored. Errors never echo the input.
func ParseKeys(text string) ([]encryption.Key, error) {
	var keys []encryption.Key
	for _, field := range splitKeys(text) {
		material, err := encryption.ParseKey(field)
		if err != nil {
			return nil, fmt.Errorf("credentials: key %d: %w", len(keys)+1, err)
		}
		key, err := NewKey(material)
		clear(material)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, errors.New("credentials: no key found")
	}
	if len(keys) > maxKeys {
		return nil, fmt.Errorf("credentials: at most %d keys are accepted", maxKeys)
	}
	return keys, nil
}

const maxKeys = 8

func splitKeys(text string) []string {
	var out []string
	for _, line := range bytes.Split([]byte(text), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		for _, field := range bytes.Split(line, []byte(",")) {
			if field = bytes.TrimSpace(field); len(field) > 0 {
				out = append(out, string(field))
			}
		}
	}
	return out
}

func additionalData(env string, version int, keyID string) []byte {
	return encryption.AssociatedData(adDomain, strconv.Itoa(version), env, keyID)
}

// Seal encrypts a credentials document with key for environment env and
// returns the file contents. The environment name, format version and key id
// are authenticated: the result decrypts only for the same environment, so a
// staging file cannot be installed as production's.
func Seal(env string, plaintext []byte, key encryption.Key) ([]byte, error) {
	if err := ValidateEnvironment(env); err != nil {
		return nil, err
	}
	if _, err := parseDocument(plaintext); err != nil {
		return nil, err
	}
	aeadKey, err := encryption.DeriveKey(key.Material, purposeAEAD)
	if err != nil {
		return nil, err
	}
	defer clear(aeadKey)
	aead, err := encryption.NewAEAD(aeadKey)
	if err != nil {
		return nil, err
	}
	nonce, err := encryption.NewNonce(aead)
	if err != nil {
		return nil, err
	}
	ciphertext := aead.Seal(nil, nonce, plaintext, additionalData(env, FormatVersion, key.ID))
	out, err := json.MarshalIndent(envelope{
		Version:    FormatVersion,
		Algorithm:  algorithm,
		KeyID:      key.ID,
		Nonce:      base64.RawURLEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawURLEncoding.EncodeToString(ciphertext),
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// KeyID returns the key id recorded in a credentials file without decrypting
// it, for diagnostics and key selection.
func KeyID(file []byte) (string, error) {
	env, err := parseEnvelope(file)
	if err != nil {
		return "", err
	}
	return env.KeyID, nil
}

func parseEnvelope(file []byte) (envelope, error) {
	var env envelope
	if len(file) > MaxFileBytes {
		return env, fmt.Errorf("%w: file is too large", ErrFormat)
	}
	dec := json.NewDecoder(bytes.NewReader(file))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return env, fmt.Errorf("%w: not a credentials file", ErrFormat)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return env, fmt.Errorf("%w: trailing data", ErrFormat)
	}
	if env.Version != FormatVersion {
		return env, fmt.Errorf("%w: unsupported version %d (this Forge reads version %d)", ErrFormat, env.Version, FormatVersion)
	}
	if env.Algorithm != algorithm || !encryption.ValidKeyID(env.KeyID) || env.Nonce == "" || env.Ciphertext == "" {
		return env, fmt.Errorf("%w: unsupported algorithm or missing fields", ErrFormat)
	}
	return env, nil
}

// Unseal decrypts file for environment env with the key whose id the file
// records. Wrong key, modified file, wrong environment and wrong format
// version are all ErrDecrypt.
func Unseal(env string, file []byte, keys []encryption.Key) ([]byte, error) {
	if err := ValidateEnvironment(env); err != nil {
		return nil, err
	}
	header, err := parseEnvelope(file)
	if err != nil {
		return nil, err
	}
	var key *encryption.Key
	for i := range keys {
		if keys[i].ID == header.KeyID {
			key = &keys[i]
			break
		}
	}
	if key == nil {
		return nil, fmt.Errorf("%w (file key id %s)", ErrNoMatchingKey, header.KeyID)
	}
	nonce, nerr := base64.RawURLEncoding.Strict().DecodeString(header.Nonce)
	ciphertext, cerr := base64.RawURLEncoding.Strict().DecodeString(header.Ciphertext)
	if nerr != nil || cerr != nil || len(ciphertext) > MaxPlaintextBytes+64 {
		return nil, fmt.Errorf("%w: invalid encoding", ErrFormat)
	}
	aeadKey, err := encryption.DeriveKey(key.Material, purposeAEAD)
	if err != nil {
		return nil, err
	}
	defer clear(aeadKey)
	aead, err := encryption.NewAEAD(aeadKey)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("%w: invalid nonce", ErrFormat)
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, additionalData(env, header.Version, header.KeyID))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}
