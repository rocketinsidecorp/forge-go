//go:build forge_pbkdf2

package auth

// pbkdf2Build is true in a binary built with -tags forge_pbkdf2, for FIPS
// 140 deployments: Argon2id is not an approved algorithm there, so the
// default algorithm is PBKDF2-HMAC-SHA256 even with an Argon2id
// implementation registered. Argon2id hashes still verify through that
// implementation, and a successful sign-in rewrites them as PBKDF2.
const pbkdf2Build = true
