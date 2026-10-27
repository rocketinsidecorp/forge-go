package auth

import (
	"crypto/rand"
	"errors"
	"log/slog"

	"github.com/rocketinsidecorp/forge-go/auth/internal/backupcode"
)

const (
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
		code, digest, err := backupcode.Generate(rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		if _, duplicate := seen[digest]; duplicate {
			continue
		}
		seen[digest] = struct{}{}
		codes = append(codes, MFABackupCode{value: code})
		digests = append(digests, digest)
	}
	return codes, digests, nil
}

func parseMFABackupCode(value string) (Digest, bool) {
	digest, ok := backupcode.Parse(value)
	return Digest(digest), ok
}
