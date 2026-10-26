//go:build forge_pbkdf2

package auth

// defaultPasswordAlgorithm is PBKDF2-HMAC-SHA256 in a binary built with
// -tags forge_pbkdf2, for FIPS 140 deployments: Argon2id is not an approved
// algorithm there. Argon2id hashes still verify, and a successful sign-in
// rewrites them as PBKDF2.
const defaultPasswordAlgorithm = PasswordAlgorithmPBKDF2SHA256
