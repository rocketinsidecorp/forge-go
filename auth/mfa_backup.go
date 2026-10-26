package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"io"
	"log/slog"
	"strings"
)

const (
	backupCodeBytes = 15

	// DefaultMFABackupCodeCount is the number of one-time codes created when a
	// TOTP factor is first confirmed.
	DefaultMFABackupCodeCount = 10
)

// MFABackupCode is a one-time code that signs in when the authenticator is unavailable. It formats
// and logs as redacted.
type MFABackupCode struct {
	value string
}

// Reveal returns the code text, formatted in groups.
func (c MFABackupCode) Reveal() string { return c.value }

func (MFABackupCode) String() string { return "[REDACTED]" }

// GoString redacts the code in %#v output.
func (MFABackupCode) GoString() string { return "auth.MFABackupCode([REDACTED])" }

// LogValue redacts the code in structured logs.
func (MFABackupCode) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

func newMFABackupCodes(count int) ([]MFABackupCode, []Digest, error) {
	if count < 1 || count > 100 {
		return nil, nil, errors.New("MFA backup code count must be between 1 and 100")
	}
	codes := make([]MFABackupCode, 0, count)
	digests := make([]Digest, 0, count)
	seen := make(map[Digest]struct{}, count)
	for len(codes) < count {
		raw := make([]byte, backupCodeBytes)
		if _, err := io.ReadFull(rand.Reader, raw); err != nil {
			return nil, nil, err
		}
		digest := backupCodeDigest(raw)
		if _, duplicate := seen[digest]; duplicate {
			continue
		}
		seen[digest] = struct{}{}
		encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
		codes = append(codes, MFABackupCode{value: formatMFABackupCode(encoded)})
		digests = append(digests, digest)
	}
	return codes, digests, nil
}

func parseMFABackupCode(value string) (Digest, bool) {
	compact := strings.ToUpper(strings.TrimSpace(value))
	compact = strings.ReplaceAll(compact, "-", "")
	if len(compact) != backupCodeBytes*8/5 {
		return Digest{}, false
	}
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(compact)
	if err != nil || len(raw) != backupCodeBytes {
		return Digest{}, false
	}
	if base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw) != compact {
		return Digest{}, false
	}
	return backupCodeDigest(raw), true
}

func backupCodeDigest(raw []byte) Digest {
	material := make([]byte, 0, len("mfa-backup\x00")+len(raw))
	material = append(material, "mfa-backup\x00"...)
	material = append(material, raw...)
	return sha256.Sum256(material)
}

func formatMFABackupCode(encoded string) string {
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
