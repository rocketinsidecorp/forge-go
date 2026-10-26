package auth

import (
	"crypto/rand"
	"testing"
)

// cheapPasswordHash hashes with the lowest accepted work factor. Tests that
// verify a password many times, MFA step-ups above all, use it so the auth
// package stays inside the test timeout under -race; HashPassword's default
// work factor is covered by password_test.go.
func cheapPasswordHash(password string) (string, error) {
	return hashPassword(password, minAcceptedPasswordIterations, rand.Reader)
}

// testArgon2Params is the test-only Argon2id cost, the smallest the algorithm
// allows (Forge-Rust's tests likewise use a low m). Production parameters are
// asserted by TestPasswordHashDefaultsToArgon2idWithForgeRustParameters.
var testArgon2Params = argon2Params{memoryKiB: 8, iterations: 1, parallelism: 1}

// cheapPasswordHasher is NewPasswordHasher(options...) writing Argon2id at
// testArgon2Params.
func cheapPasswordHasher(t testing.TB, options ...PasswordHasherOption) *PasswordHasher {
	t.Helper()
	hasher, err := NewPasswordHasher(append([]PasswordHasherOption{WithPasswordAlgorithm(PasswordAlgorithmArgon2id)}, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	hasher.argon2 = testArgon2Params
	hasher.setDummy()
	return hasher
}
