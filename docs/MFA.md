# Multi-Factor Authentication

Forge Phase 3 includes a TOTP MFA foundation in `auth` with PostgreSQL
persistence in `auth/postgres`. It is intentionally fail-closed: an account
whose password identity says MFA is enabled never receives a session from the
password step alone.

## TOTP profile

Forge uses the interoperable RFC 6238 profile commonly supported by
authenticator applications:

- HMAC-SHA-1;
- 6 decimal digits;
- 30-second periods;
- a 160-bit random seed;
- verification window of the current period plus one period before/after.

SHA-1 here is the HMAC algorithm required for broad TOTP interoperability; it
is not used for password hashing. Passwords continue to use the versioned
PBKDF2-HMAC-SHA256 format documented in [AUTHENTICATION.md](AUTHENTICATION.md).

Every accepted TOTP counter is persisted. A code from the same or an older
counter is rejected, preventing replay even when requests race.

## Secret protection

TOTP seeds must not be stored as plaintext. `MFAService` requires a
`SecretCipher`. Forge provides `NewAESGCMSecretCipher`, which uses
AES-256-GCM from the Go standard library and requires an exact 32-byte key.

The encryption key must come from an external secret source and must not be
stored in the identity database. Applications that need key IDs, managed KMS,
or online key rotation can implement the narrow `SecretCipher` port instead
of using the built-in single-key cipher.

PostgreSQL stores both authenticated ciphertext and a SHA-256 seed digest. The
digest is checked after decryption so a wrong key/corrupt ciphertext cannot be
silently accepted.

## Enrollment

`MFAService.BeginTOTPEnrollment` creates a pending seed and returns:

- the Base32 seed;
- an `otpauth://` provisioning URI;
- the enrollment expiry.

The PostgreSQL adapter never replaces an already-active factor through this
initial enrollment path. Rotation and disabling use separate, step-up
authorized lifecycle operations described below.

The framework HTTP helper also requires an explicit
`MFAEnrollmentAuthorizer`. This is deliberate: merely possessing an existing
session is not sufficient proof for changing MFA. Applications should use the
authorizer for step-up policy such as recent password verification or another
strong reauthentication rule.

Confirmation verifies a live TOTP code, installs the factor atomically,
increments the user's `session_version`, revokes all current sessions, and
invalidates outstanding MFA challenges. The confirmation code's counter is
recorded, so that same code cannot immediately be replayed for login.

## Password login with MFA

`PasswordIdentity.MFARequired` tells `LoginService` that a live TOTP factor
exists. Configure the login service with the same MFA service:

```go
login, err := auth.NewLoginService(
    repository,
    repository,
    auth.WithMFAChallengeIssuer(mfa),
)
```

After a correct password, an MFA account receives HTTP `202`:

```json
{
  "mfa_required": true,
  "challenge_token": "<opaque 256-bit challenge>",
  "expires_at": "..."
}
```

No session is created at this point.

The challenge is digest-only in persistence and is bound to:

- the exact membership selected by the tenant-scoped password login;
- the user's current `session_version`;
- the exact active TOTP factor digest;
- a short challenge expiry;
- the session expiry originally selected by the password login.

Password reset/global revocation, factor replacement, membership/tenant
deactivation, or organization deactivation therefore makes an outstanding
challenge unusable.

Session creation is credential-version-bound as well. The password lookup
returns the exact `session_version` it verified; both direct password sessions
and MFA challenge issuance require that version to still be current. MFA
completion carries the challenge's version into final session creation. This
closes the race where a password reset could otherwise occur after credential
verification but before session persistence.

Submit the challenge and current TOTP code to
`NewMFACompletionHandler` for bearer clients. Browser-cookie clients use
`NewCookieMFACompletionHandler`: password login returns the same `202`
challenge without setting a session cookie, and only successful MFA completion
sets the secure session and CSRF cookies. The cookie completion endpoint
requires the same HTTPS same-origin `Origin` check as cookie login.

Successful completion atomically consumes the challenge and advances the factor
counter before creating the normal opaque session. If session creation later
fails, the challenge remains consumed; the user starts a fresh password login
rather than replaying it.

MFA attempts are rate-limited by domain-separated challenge and source hashes.
The default in-memory limiter allows five attempts per challenge and 60 per
source in a five-minute window. Multi-replica applications should supply a
shared throttler.

## PostgreSQL tables

`auth/postgres.Migrations()` adds:

- `forge_totp_enrollments` for short-lived pending seeds;
- `forge_totp_factors` for the active encrypted seed and replay counter;
- `forge_mfa_backup_codes` for `SHA-256("mfa-backup\0" || code)` digests of
  one-time backup codes;
- `forge_mfa_challenges` for digest-only one-time password-login challenges.

`Repository.PruneMFA(ctx, limit)` removes expired enrollments and
consumed/expired challenges in bounded `SKIP LOCKED` batches.

Every MFA transaction locks the account's `forge_users` row first and only
then its enrollment, factor, backup code, membership and challenge rows, so a
sign-in and a factor change of the same account wait for each other instead
of deadlocking. A pending key is a rotation's while the account has a factor
and an enrollment's otherwise (`TOTPSecretRecord.FactorActive`, read in the
same statement), so a key of one kind is never confirmed or shown as the
other. Custom stores should keep both rules.

## Factor rotation and disable

Active TOTP factors are changed only through explicit lifecycle operations.
`BeginTOTPRotation` creates a pending replacement seed while the current
factor stays active. `ConfirmTOTPRotation` proves the new seed, replaces the
factor and backup-code set atomically, increments `session_version`, revokes
all sessions, and invalidates outstanding MFA challenges.

`DisableMFA` removes the active factor, pending enrollment and backup codes,
then also increments `session_version`, revokes sessions, and invalidates
challenges.

The HTTP adapters require an application-supplied `MFAChangeAuthorizer` for
starting rotation, confirming rotation, and disabling MFA. Applications should
use it for strong step-up authorization such as recent password verification or
another independently verified factor. An ordinary authenticated session alone
is deliberately insufficient.

See [ADR 0015](adr/0015-mfa-factor-lifecycle.md).

## Generated settings pages

PostgreSQL applications from `forge new` (and `forge generate
authentication`) include the two-factor settings of the signed-in account
([ADR 0045](adr/0045-generated-mfa-settings-pages.md); the same routes as
Forge-Rust):

```sh
forge new myapp && cd myapp          # .env.local already holds a development FORGE_AUTH_MFA_KEY
docker compose up -d postgres
forge migrate
forge setup --tenant acme --email you@example.com
forge dev
# sign in at http://localhost:8080/session/new, then open http://localhost:8080/mfa
```

For production, generate the key once and keep it in a secret manager:

```sh
export FORGE_AUTH_MFA_KEY=$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')
```

| Method | Path | Page |
| --- | --- | --- |
| `GET` | `/mfa` | on or off, backup codes left (or "not available" without `FORGE_AUTH_MFA_KEY`) |
| `POST` | `/mfa` | turn on: the password, then the key (`otpauth://` link and Base32 text) |
| `POST` | `/mfa/confirm` | the password and a code of the new key: 10 backup codes, shown once; signs out everywhere |
| `POST` | `/mfa/rotation` | replace the authenticator: the password and a current code, then the new key |
| `POST` | `/mfa/rotation/confirm` | the password, a current code (`current_code`) and a code of the new key: new backup codes; signs out everywhere |
| `POST` | `/mfa/backup_codes` | new backup codes (the password and a current code); the old ones stop working |
| `DELETE` | `/mfa` | turn off (the password and a current code); signs out everywhere |

A "current code" is a code of the active authenticator or an unused backup
code; either works once. An authenticator code is used up at once, a backup
code by the change it authorizes, in the same transaction: a change that
fails (a wrong code of the new key, an expired rotation, an account changed
meanwhile) keeps the backup code, and a backup code used elsewhere in the
meantime refuses the change (`ErrMFAStepUpInvalid`). Starting a rotation
therefore uses up the current code of the authenticator app, and confirming
it needs a current code again: wait for the next code or use a backup code
(the pages say so). Every one of these requests:

- needs the CSRF token (`403` without it) and a session (signed-out browsers
  go to `/session/new`);
- asks for the password again, also when confirming a new key, so a stolen
  session can neither see a pending key again nor guess its code;
- counts one attempt on the MFA throttler (five per account and 60 per source
  in five minutes) before anything is checked: `422` with one message for a
  wrong password or code, `429` with `Retry-After` when throttled. A wrong
  code of a new key counts too. The counters are cleared only after a change
  succeeds, and a password alone never clears them for an account that has a
  factor. `POST /mfa` and `POST /mfa/confirm`, which need no code, answer an
  account that already has a factor the same way (a `303` back to `/mfa`)
  before the password is checked, so they cannot tell a right password from
  a wrong one;
- is audited, refusals as the target action with outcome `denied`
  ([AUDIT.md](AUDIT.md)).

Keys and backup codes appear only on `no-store` pages and are never logged.
The pages live in `app/adapters/htmlauth` and `app/views/mfa`; their text is
under `mfa.*` in `config/locales/authentication.*.json`. Change them freely;
the security steps are in `auth.Browser` (`BeginMFAEnrollment`,
`ConfirmMFAEnrollment`, `BeginMFARotation`, `ConfirmMFARotation`,
`RegenerateMFABackupCodes`, `DisableMFA`, `MFAStatus`), which needs
`BrowserOptions.Passwords` (an `auth.SubjectPasswordStore`, implemented by
`auth/postgres.Repository`) besides the MFA service.

In code, `MFAService.VerifyMFAStepUp` checks a current code before a factor
change and returns the proof to pass to it with `auth.WithMFAStepUp(proof)`
(`BeginTOTPRotation`, `ConfirmTOTPRotation`, `RegenerateMFABackupCodes`,
`DisableMFA`), which uses a backup code up only with the change;
`MFAService.VerifyMFACode` checks and uses up a current code at once, for a
step-up that changes no factor; `RegenerateMFABackupCodes` replaces the backup codes (event
`auth.mfa.backup_codes_regenerated`) and `MFAStatus` reports the state; they
need a store implementing `auth.MFASettingsStore`. `auth.TOTPCodeAt(key,
time)` computes the code an authenticator app shows, for end-to-end tests.

## Remaining MFA work

Recovery delivery is application-specific and uses the separate recovery
service and sender interface. When composing these services manually, configure
the security auditor explicitly. A QR code image is not rendered (as in
Forge-Rust): the page shows the `otpauth://` link, which phones open in their
authenticator app, and the Base32 key.

See also [ADR 0014](adr/0014-totp-mfa.md).
