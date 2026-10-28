//go:build !forge_pbkdf2

package sqlite_test

import (
	"regexp"
	"testing"

	"github.com/rocketinsidecorp/forge-go/auth"
	_ "github.com/rocketinsidecorp/forge-go/auth/sqlite"
)

// A binary built with -tags forge_pbkdf2 writes PBKDF2 by choice, so this
// file is not part of it.

// Linking the SQLite identity store registers the real Argon2id: the default
// hasher writes $argon2id$ hashes at Forge-Rust's cost and verifies them, as
// generated --auth applications rely on (ADR 0053).
func TestLinkingTheIdentityStoreRegistersArgon2id(t *testing.T) {
	t.Parallel()
	if got := auth.DefaultPasswordAlgorithm(); got != auth.PasswordAlgorithmArgon2id {
		t.Fatalf("default algorithm %q, want argon2id", got)
	}
	hash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`).MatchString(hash) {
		t.Fatalf("hash %q", hash)
	}
	if match, rehash, err := auth.VerifyPassword(hash, "correct horse battery staple"); err != nil || !match || rehash {
		t.Fatalf("VerifyPassword = (%v, %v, %v)", match, rehash, err)
	}
	if match, _, err := auth.VerifyPassword(hash, "wrong"); err != nil || match {
		t.Fatalf("wrong password: match=%v err=%v", match, err)
	}
}
