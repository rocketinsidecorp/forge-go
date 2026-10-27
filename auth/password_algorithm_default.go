//go:build !forge_pbkdf2

package auth

// pbkdf2Build is false: the default algorithm is Argon2id once an
// implementation is registered (password_algorithm_pbkdf2.go has the
// forge_pbkdf2 build).
const pbkdf2Build = false
