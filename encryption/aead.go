package encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// NewAEAD returns AES-256-GCM for a 32-byte key.
func NewAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: key must be %d bytes", ErrInvalidKey, KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// NewNonce returns a random nonce for aead. Random 96-bit nonces stay safe
// for up to 2^32 messages per key; rotate keys well before that.
func NewNonce(aead cipher.AEAD) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("encryption: read random nonce: %w", err)
	}
	return nonce, nil
}

// SealPrefixed encrypts plaintext with a fresh random nonce and returns
// nonce||ciphertext||tag.
func SealPrefixed(aead cipher.AEAD, plaintext, additionalData []byte) ([]byte, error) {
	nonce, err := NewNonce(aead)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plaintext, additionalData), nil
}

// OpenPrefixed reverses SealPrefixed. Every failure is ErrDecrypt.
func OpenPrefixed(aead cipher.AEAD, sealed, additionalData []byte) ([]byte, error) {
	nonceSize := aead.NonceSize()
	if len(sealed) < nonceSize+aead.Overhead() {
		return nil, ErrDecrypt
	}
	plaintext, err := aead.Open(nil, sealed[:nonceSize], sealed[nonceSize:], additionalData)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// ErrDecrypt is returned for every authentication failure: wrong key,
// modified ciphertext or mismatched associated data. It deliberately says no
// more, so callers cannot learn which.
var ErrDecrypt = errors.New("encryption: decryption failed")

// AssociatedData encodes parts unambiguously for use as AEAD associated data:
// each part is prefixed with its 4-byte big-endian length, so ("ab","c") and
// ("a","bc") differ.
func AssociatedData(parts ...string) []byte {
	size := 0
	for _, part := range parts {
		size += 4 + len(part)
	}
	out := make([]byte, 0, size)
	for _, part := range parts {
		out = binary.BigEndian.AppendUint32(out, uint32(len(part))) // #nosec G115 -- parts are short identifiers
		out = append(out, part...)
	}
	return out
}
