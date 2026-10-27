// Package backupcode is the format of two-factor backup codes: 15 random
// bytes, written in unpadded base32 in groups of four, stored only as a
// domain-separated SHA-256 digest. auth issues and checks codes with it; it
// is internal so the tests of auth/authhttp use the same format rather than a
// copy of it.
package backupcode

import (
	"crypto/sha256"
	"encoding/base32"
	"io"
	"strings"
)

// Bytes is the entropy of a code.
const Bytes = 15

var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Generate reads one code from entropy and returns its text and digest.
func Generate(entropy io.Reader) (code string, digest [sha256.Size]byte, err error) {
	raw := make([]byte, Bytes)
	if _, err := io.ReadFull(entropy, raw); err != nil {
		return "", digest, err
	}
	return format(encoding.EncodeToString(raw)), Digest(raw), nil
}

// Parse returns the digest of a code as a person may type it (any case,
// with or without dashes and surrounding space), and whether value is one.
func Parse(value string) ([sha256.Size]byte, bool) {
	compact := strings.ToUpper(strings.TrimSpace(value))
	compact = strings.ReplaceAll(compact, "-", "")
	if len(compact) != Bytes*8/5 {
		return [sha256.Size]byte{}, false
	}
	raw, err := encoding.DecodeString(compact)
	if err != nil || len(raw) != Bytes {
		return [sha256.Size]byte{}, false
	}
	if encoding.EncodeToString(raw) != compact {
		return [sha256.Size]byte{}, false
	}
	return Digest(raw), true
}

// Digest is what a store keeps of the code made of raw.
func Digest(raw []byte) [sha256.Size]byte {
	material := make([]byte, 0, len("mfa-backup\x00")+len(raw))
	material = append(material, "mfa-backup\x00"...)
	material = append(material, raw...)
	return sha256.Sum256(material)
}

func format(encoded string) string {
	var result strings.Builder
	result.Grow(len(encoded) + len(encoded)/4)
	for index, r := range encoded {
		if index > 0 && index%4 == 0 {
			result.WriteByte('-')
		}
		result.WriteRune(r)
	}
	return result.String()
}
