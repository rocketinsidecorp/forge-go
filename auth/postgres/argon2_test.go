//go:build !forge_pbkdf2

package postgres_test

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"

	"github.com/rocketinsidecorp/forge-go/auth"
	_ "github.com/rocketinsidecorp/forge-go/auth/postgres"
)

// A binary built with -tags forge_pbkdf2 writes PBKDF2 by choice, so this
// file is not part of it.

// Linking the PostgreSQL identity store registers the real Argon2id: the default
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

// Written by Forge-Rust's generated Argon2idPasswordHasher (argon2 0.6.0,
// Argon2id v19, m=19456, t=2, p=1, 32-byte output, a random 16-byte salt)
// for a test-only password.
const (
	forgeRustArgon2idFixture         = "$argon2id$v=19$m=19456,t=2,p=1$66DIwPvWLVzp/qkTyDQLqw$AMsFRwhoUEWdrArkL3sE7DfMOkuFGT8kuA6RIuNqZdU" // #nosec G101 -- the hash of a public test-only password
	forgeRustArgon2idFixturePassword = "forge-cross-framework-fixture"                                                                     // #nosec G101 -- a public test-only password
)

// The real Argon2id this adapter registers reads Forge-Rust's hashes and
// writes the same shape (moved from auth, which no longer links Argon2).
func TestArgon2idHashWrittenByForgeRustVerifies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hasher, err := auth.NewPasswordHasher(auth.WithPasswordAlgorithm(auth.PasswordAlgorithmArgon2id))
	if err != nil {
		t.Fatal(err)
	}
	match, rehash, err := hasher.Verify(ctx, forgeRustArgon2idFixture, forgeRustArgon2idFixturePassword)
	if err != nil || !match || rehash {
		t.Fatalf("Forge-Rust hash: (%v, %v, %v), want a match in the current parameters", match, rehash, err)
	}
	if match, _, err := hasher.Verify(ctx, forgeRustArgon2idFixture, forgeRustArgon2idFixturePassword+"x"); err != nil || match {
		t.Fatalf("wrong password: match=%v err=%v", match, err)
	}
	// Forge-Go writes the same shape, so Forge-Rust reads it back.
	ours, err := hasher.Hash(ctx, forgeRustArgon2idFixturePassword)
	if err != nil {
		t.Fatal(err)
	}
	if len(ours) != len(forgeRustArgon2idFixture) || !strings.HasPrefix(ours, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("Forge-Go hash %q does not have Forge-Rust's shape", ours)
	}
}

// argon2.Key, Argon2i with IDKey's signature, is refused: its hashes would
// be labelled $argon2id$ and verify nowhere else. So is the real Argon2id
// behind a wrapper that clamps the cost: its hashes would claim
// m=19456,t=2,p=1 and cost next to nothing to crack. A second, valid
// registration reports that it did not take effect.
func TestRegisterArgon2idRefusesArgon2iAndClampedCosts(t *testing.T) {
	t.Parallel()
	for name, kdf := range map[string]func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte{
		"argon2.Key (Argon2i)": argon2.Key,
		"cost clamped to the minimum": func(password, salt []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
			return argon2.IDKey(password, salt, 1, 8, 1, keyLen)
		},
		"cost clamped to t=2, m=32, p=2": func(password, salt []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
			return argon2.IDKey(password, salt, 2, 32, 2, keyLen)
		},
		"memory ignored": func(password, salt []byte, time, _ uint32, threads uint8, keyLen uint32) []byte {
			return argon2.IDKey(password, salt, time, 8*uint32(threads), threads, keyLen)
		},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("auth.RegisterArgon2id(%s) did not panic", name)
				}
			}()
			auth.RegisterArgon2id(kdf)
		}()
	}
	if auth.RegisterArgon2id(argon2.IDKey) {
		t.Fatal("a second registration reported that it took effect")
	}
}
