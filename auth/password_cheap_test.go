package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
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

// fakeArgon2idKDF stands in for golang.org/x/crypto/argon2.IDKey, which the
// core does not link: a fast, deterministic function of every input (the
// password, the salt and all four parameters) with IDKey's signature, so the
// PHC format, the parser, the rehash and floor logic and the services are
// tested without Argon2's cost. The real Argon2id, Forge-Rust's hashes among
// its tests, is tested where the identity adapters register it.
func fakeArgon2idKDF(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
	// RegisterArgon2id checks one known answer; the fake knows it.
	if bytes.Equal(password, argon2idKnownPassword) && bytes.Equal(salt, argon2idKnownSalt) && time == 1 && memory == 8 && threads == 1 && keyLen == 32 {
		return append([]byte(nil), argon2idKnownKey...)
	}
	digest := sha256.New()
	var header [17]byte
	binary.BigEndian.PutUint32(header[0:], time)
	binary.BigEndian.PutUint32(header[4:], memory)
	header[8] = threads
	binary.BigEndian.PutUint32(header[9:], keyLen)
	binary.BigEndian.PutUint32(header[13:], uint32(len(salt))) // #nosec G115 -- salts are at most 64 bytes
	digest.Write(header[:])
	digest.Write(salt)
	digest.Write(password)
	seed := digest.Sum(nil)
	key := make([]byte, 0, keyLen+sha256.Size)
	for block := byte(0); uint32(len(key)) < keyLen; block++ { // #nosec G115 -- keys are 32 bytes
		sum := sha256.Sum256(append([]byte{block}, seed...))
		key = append(key, sum[:]...)
	}
	return key[:keyLen]
}

// The package's tests write and verify Argon2id hashes through the fake.
// Registration is first-wins and permanent, so tests of the unregistered
// state build hashers on an empty registry (unregisteredHasher).
func init() {
	RegisterArgon2id(fakeArgon2idKDF)
}

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

// unregisteredHasher is NewPasswordHasher(options...) looking Argon2id up in
// an empty registry, as in a binary that links no identity adapter and never
// calls RegisterArgon2id.
func unregisteredHasher(t testing.TB, options ...PasswordHasherOption) *PasswordHasher {
	t.Helper()
	hasher, err := NewPasswordHasher(options...)
	if err != nil {
		t.Fatal(err)
	}
	hasher.kdfs = &argon2Registry{}
	return hasher
}
