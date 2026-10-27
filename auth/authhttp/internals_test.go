package authhttp

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/auth/internal/backupcode"
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

// backupCode is a backup code as auth.MFAService shows it.
type backupCode string

func (c backupCode) Reveal() string { return string(c) }

// newMFABackupCodes returns count backup codes and their digests, in auth's
// format (auth/internal/backupcode).
func newMFABackupCodes(count int) ([]backupCode, []auth.Digest, error) {
	codes := make([]backupCode, 0, count)
	digests := make([]auth.Digest, 0, count)
	for range count {
		code, digest, err := backupcode.Generate(rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		codes = append(codes, backupCode(code))
		digests = append(digests, digest)
	}
	return codes, digests, nil
}

// parseMFABackupCode returns the digest auth stores for a backup code, and
// whether value is one.
func parseMFABackupCode(value string) (auth.Digest, bool) {
	digest, ok := backupcode.Parse(value)
	return auth.Digest(digest), ok
}
