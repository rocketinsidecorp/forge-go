package auth

import "crypto/rand"

// cheapPasswordHash hashes with the lowest accepted work factor. Tests that
// verify a password many times, MFA step-ups above all, use it so the auth
// package stays inside the test timeout under -race; HashPassword's default
// work factor is covered by password_test.go.
func cheapPasswordHash(password string) (string, error) {
	return hashPassword(password, minAcceptedPasswordIterations, rand.Reader)
}
