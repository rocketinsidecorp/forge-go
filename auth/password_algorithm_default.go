//go:build !forge_pbkdf2

package auth

// defaultPasswordAlgorithm is Argon2id unless the binary is built with the
// forge_pbkdf2 tag (password_algorithm_pbkdf2.go).
const defaultPasswordAlgorithm = PasswordAlgorithmArgon2id
