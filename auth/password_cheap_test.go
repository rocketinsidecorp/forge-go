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

// The package's tests fail sign-ins and step-ups hundreds of times through
// services on DefaultPasswordHasher. Under -race one PBKDF2 calibration of
// the legacy timing floor takes over a second, which every failure would
// then wait, so the default hasher has no floor here. The floor's tests
// (password_floor_test.go) build hashers with it on, in synctest bubbles.
func init() {
	DefaultPasswordHasher().floor = nil
}

// cheapPasswordHasher is NewPasswordHasher(options...) writing Argon2id at
// testArgon2Params, without the legacy timing floor unless options turn it
// on.
func cheapPasswordHasher(t testing.TB, options ...PasswordHasherOption) *PasswordHasher {
	t.Helper()
	defaults := []PasswordHasherOption{WithPasswordAlgorithm(PasswordAlgorithmArgon2id), WithLegacyHashTimingFloor(false)}
	hasher, err := NewPasswordHasher(append(defaults, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	hasher.argon2 = testArgon2Params
	hasher.setDummy()
	return hasher
}
