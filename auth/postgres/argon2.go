package postgres

import (
	"golang.org/x/crypto/argon2"

	"github.com/rocketinsidecorp/forge-go/auth"
)

// Linking this identity store gives the application Argon2id password
// hashing: the core imports no third-party module, so the adapter registers
// golang.org/x/crypto/argon2 with auth (ADR 0053). Registration is
// first-wins, so linking several identity adapters is harmless, and hashers
// look the implementation up when they hash, so initialization order does
// not matter.
func init() {
	auth.RegisterArgon2id(argon2.IDKey)
}
