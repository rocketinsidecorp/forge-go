# ADR 0048: Argon2id password hashing in the core

- **Status:** Accepted
- **Date:** 2026-10-26
- **Amends:** [ADR 0006](0006-zero-dependency-core.md) (admits three
  `golang.org/x` packages to the core)
- **Amended by:** [ADR 0053](0053-dependency-free-core.md): `auth` no longer
  imports `golang.org/x/crypto/argon2`. The identity stores register it
  through `auth.RegisterArgon2id`; with nothing registered new hashes are
  PBKDF2 and an Argon2id hash fails closed (`503`). Everything else below
  stands.
- **Parity with:** Forge-Rust's generated `Argon2idPasswordHasher` and
  `forge::auth::PasswordWorkLimiter`

## Context

Forge-Go stored passwords as PBKDF2-HMAC-SHA256 with 600,000 iterations,
because the standard library has PBKDF2 and the core takes no third-party
module (ADR 0006). PBKDF2 needs no memory, so a GPU or ASIC tries guesses far
more cheaply than the server computes them. Forge-Rust stores Argon2id
(RFC 9106, memory-hard), so the two frameworks also wrote different hashes into
the same `password_hash` column. The 2026-10-24 audit (S-9, P-7) asked for
Argon2id by default, PBKDF2 hashes still accepted and upgraded, a bound on
concurrent hashing, and a FIPS 140 escape.

The standard library has no Argon2. Writing it ourselves would be custom
cryptography, which Forge refuses.

## Decision

- `auth` imports `golang.org/x/crypto/argon2` (Go team, BSD-3-Clause,
  covered by `govulncheck`; `golang.org/x/crypto` was already in `go.mod`
  for forge-proxy, ADR 0030). With it the core links exactly three packages
  outside the standard library: `x/crypto/argon2`, `x/crypto/blake2b` and
  `x/sys/cpu`. `architecture_test.go` lists those three packages, not the
  modules, and lets only `auth` import `argon2` (the other two only through
  it), so anything else from `golang.org/x`, or another core package
  importing them, still fails the test.
- New hashes are `$argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>`: Forge-Rust's
  parameters, a 16-byte random salt (`crypto/rand`), a 32-byte key, unpadded
  standard base64, so either framework verifies the other's hashes (a test
  verifies a hash Forge-Rust wrote).
- The parser accepts only that form: version 19, `m`, `t`, `p` in that order
  as canonical decimals, `m` at most 65,536 KiB, `t` at most 10, `p` at most 8,
  `m` at least `8p`, a salt of 8 to 64 bytes, a 32-byte key, at most 1,024
  characters. A stored hash names its own cost, so the bounds limit what a
  planted hash can demand of the process.
- PBKDF2 hashes still verify. A successful sign-in rewrites a hash that is not
  Argon2id with the current parameters (or whose salt is not 16 bytes)
  through the existing conditional `ReplacePasswordHash`; if hashing is
  saturated then, the rewrite waits for the next sign-in.
- While PBKDF2 hashes may remain, verifying one (about 120 ms) is slower than
  an Argon2id one or an unknown account (about 30 ms), which would tell
  dormant accounts from unknown ones. Every failed sign-in or step-up check is
  therefore padded by a timer (no limiter slot, no CPU, cancelled with the
  request) to one PBKDF2-600k verification plus 25%, calibrated once per
  hasher and raised by slower legacy verifications up to twice the
  calibration. The failure is audited before the padding, so hanging up
  during it cannot skip the record. `WithLegacyHashTimingFloor` (on by
  default with Argon2id) turns it off once no `$pbkdf2-sha256$` hash remains,
  or in an application created with this release, which never stored one.
- Every hash and verification runs through a `PasswordWorkLimiter`: one slot
  per CPU (`GOMAXPROCS`), at most 16, a two-second wait, then
  `ErrPasswordHashingUnavailable` (`503`, `Retry-After: 5`), as in Forge-Rust.
- A binary built with `-tags forge_pbkdf2` writes PBKDF2 instead, for FIPS 140
  deployments; `auth.WithPasswordAlgorithm` does the same for one
  `PasswordHasher`. Both algorithms always verify.

## Consequences

- Every application links the three packages above, also those that never
  hash a password, because many core packages import `auth`.
- Existing accounts move to Argon2id on their next sign-in. Accounts that
  never sign in keep PBKDF2 hashes, which stay valid.
- A sign-in costs about 19 MiB for its hash; the limiter bounds the total.
- A `forge_pbkdf2` build still links Argon2 to verify (and then rewrite)
  Argon2id hashes written before the switch, and that verification uses
  `golang.org/x/crypto/argon2`, which is not part of a FIPS 140 validated
  module.
- A legacy account's first sign-in costs PBKDF2 plus Argon2id; failed checks
  take at least one PBKDF2 verification until the floor is turned off.
