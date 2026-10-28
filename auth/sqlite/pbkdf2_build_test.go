//go:build forge_pbkdf2

package sqlite_test

import (
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/auth"
	_ "github.com/rocketinsidecorp/forge-go/auth/sqlite"
)

// A binary built with -tags forge_pbkdf2 (FIPS 140) writes PBKDF2 even
// though linking this identity store registered Argon2id, and still
// verifies Argon2id hashes, marking them for a rehash to PBKDF2 (ADR 0048,
// ADR 0053). CI runs it: go test -tags forge_pbkdf2 ./auth/...
func TestForgePBKDF2BuildWritesPBKDF2WithArgon2idRegistered(t *testing.T) {
	t.Parallel()
	if got := auth.DefaultPasswordAlgorithm(); got != auth.PasswordAlgorithmPBKDF2SHA256 {
		t.Fatalf("default algorithm %q, want pbkdf2-sha256 in a forge_pbkdf2 build", got)
	}
	hash, err := auth.HashPassword("correct horse battery staple")
	if err != nil || !strings.HasPrefix(hash, "$pbkdf2-sha256$v=1$i=600000$") {
		t.Fatalf("HashPassword = %q, %v; want PBKDF2", hash, err)
	}
	// Argon2id is registered: an explicit Argon2id hasher works, and the
	// default one verifies its hash and asks for a rehash to PBKDF2.
	argon, err := auth.NewPasswordHasher(auth.WithPasswordAlgorithm(auth.PasswordAlgorithmArgon2id))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := argon.Hash(t.Context(), "correct horse battery staple")
	if err != nil || !strings.HasPrefix(stored, "$argon2id$") {
		t.Fatalf("Argon2id hasher: %q, %v", stored, err)
	}
	if match, rehash, err := auth.VerifyPassword(stored, "correct horse battery staple"); err != nil || !match || !rehash {
		t.Fatalf("VerifyPassword(Argon2id) = (%v, %v, %v), want a match marked for rehash", match, rehash, err)
	}
}
