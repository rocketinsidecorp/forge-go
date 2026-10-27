package authhttp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/internal/principalctx"
)

// The tests of this package drive auth's services through its exported API.
// These helpers stand in for the few unexported pieces of auth the tests used
// while they lived there; each follows auth's documented behavior.

// defaultSessionTTL is auth.LoginService's session lifetime without
// auth.WithSessionTTL.
const defaultSessionTTL = 24 * time.Hour

// totpPeriodSeconds is the TOTP time step (RFC 6238's default, auth's).
const totpPeriodSeconds = 30

// withPrincipal puts principal in ctx as Middleware.Authenticate does.
func withPrincipal(ctx context.Context, principal auth.Principal) context.Context {
	return principalctx.With(ctx, principal)
}

// totpCode is the 6-digit code of secret for counter, through
// auth.TOTPCodeAt.
func totpCode(secret []byte, counter uint64) string {
	key := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
	code, err := auth.TOTPCodeAt(key, time.Unix(int64(counter)*totpPeriodSeconds, 0))
	if err != nil {
		panic(err)
	}
	return code
}

// backupCode is a backup code in auth's format: 15 random bytes in base32,
// in groups of four.
type backupCode string

func (c backupCode) Reveal() string { return string(c) }

// newMFABackupCodes returns count backup codes and their digests, as
// auth.MFAService issues them.
func newMFABackupCodes(count int) ([]backupCode, []auth.Digest, error) {
	codes := make([]backupCode, 0, count)
	digests := make([]auth.Digest, 0, count)
	for range count {
		raw := make([]byte, 15)
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, err
		}
		encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
		var grouped strings.Builder
		for index, r := range encoded {
			if index > 0 && index%4 == 0 {
				grouped.WriteByte('-')
			}
			grouped.WriteRune(r)
		}
		codes = append(codes, backupCode(grouped.String()))
		digests = append(digests, backupCodeDigest(raw))
	}
	return codes, digests, nil
}

// parseMFABackupCode returns the digest auth stores for a backup code, and
// whether value is one.
func parseMFABackupCode(value string) (auth.Digest, bool) {
	compact := strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(value)), "-", "")
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(compact)
	if err != nil || len(raw) != 15 {
		return auth.Digest{}, false
	}
	return backupCodeDigest(raw), true
}

func backupCodeDigest(raw []byte) auth.Digest {
	return sha256.Sum256(append([]byte("mfa-backup\x00"), raw...))
}
