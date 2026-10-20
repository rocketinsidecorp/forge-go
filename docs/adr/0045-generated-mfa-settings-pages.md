# ADR 0045: Generated two-factor settings pages with step-up

- **Status:** Accepted
- **Date:** 2026-10-20
- **Parity with:** Forge-Rust ADR 0040 (TOTP multi-factor authentication)

## Context

[ADR 0014](0014-totp-mfa.md) and [ADR 0015](0015-mfa-factor-lifecycle.md)
gave Forge-Go replay-safe TOTP, encrypted seeds, backup codes and a
two-phase rotation, but `forge new` generated only the sign-in code step
(`/session/mfa`): enrollment, rotation and disable were left to an
application-supplied `MFAChangeAuthorizer`, and there was no way to replace
the backup codes. Forge-Rust's port (ADR 0040) generates the settings pages
with a fixed step-up policy, and the owner decided (2026-10-20) that both
frameworks ship the same capabilities with the same routes.

Review of the Rust pages found two ways a session could weaken a factor that
the Go pages must not repeat:

1. A confirmation step guarded only by the session and the CSRF token lets a
   stolen session see the pending key again or guess its 6-digit code.
2. A step-up throttle that a password-only request resets lets a session
   holding the password interleave password-only requests with wrong codes
   and guess a code without limit.

## Decision

1. **Routes, as in Forge-Rust.** `forge new` (PostgreSQL) and `forge generate
   authentication` write `GET /mfa` (status, backup codes left), `POST /mfa`
   (turn on), `POST /mfa/confirm`, `POST /mfa/rotation`,
   `POST /mfa/rotation/confirm`, `POST /mfa/backup_codes` and `DELETE /mfa`
   into `app/adapters/htmlauth` and `app/views/mfa`, behind
   `Middleware.AuthenticateBrowser`. `htmlauth.Register` takes the session
   middleware. Text is under `mfa.*` in `config/locales/authentication.*.json`
   (`en`, `es`, `pt-BR`), the same keys as Forge-Rust.
2. **Security decisions stay in `auth.Browser`** (ADR 0026): `MFAStatus`,
   `BeginMFAEnrollment`, `ConfirmMFAEnrollment`, `BeginMFARotation`,
   `ConfirmMFARotation`, `RegenerateMFABackupCodes` and `DisableMFA`. Each
   requires `csrf.Verified`, an authenticated principal and a
   `SubjectPasswordStore` (`BrowserOptions.Passwords`); without MFA or that
   store they fail closed with `ErrMFAUnavailable`.
3. **Step-up on every change, including confirmation.** Turning on and
   confirming it need the password; rotating, confirming a rotation (field
   `current_code`), new backup codes and turning off need the password and a
   current authenticator or backup code, which is used up (its TOTP period
   is recorded as accepted, a backup code is consumed). A wrong password or
   code is one answer, `ErrMFAStepUpInvalid` (a right password with a wrong
   code takes one more database round trip: a throttled timing residual).
   For an account that already has a factor, turning on and confirming are
   refused with `ErrMFAAlreadyEnabled` before the password is checked, so
   their answer does not depend on it; the refusal is audited as denied. The pending key is shown again
   only after a confirmation whose step-up succeeded but whose new code was
   wrong (`MFAPendingCodeError`).
4. **Throttling.** Every step-up counts one attempt on the MFA throttler
   (`DefaultMFAThrottleConfig`: five per account, 60 per source in five
   minutes), under the `mfa-step-up` key domain, before the password is
   checked. A wrong code of a new key also counts. The counters are cleared
   only after the whole change succeeds, and only when a code was verified or
   the account had no factor: a password-only step-up never clears the
   counters of an account with a factor.
5. **Core additions.** `MFASettingsStore` (status, active factor, atomic
   counter advance, atomic backup-code use, backup-code replacement),
   implemented by `auth/postgres.Repository` on the existing tables (no
   migration); `MFAService.MFAStatus`, `PendingTOTP`, `VerifyMFACode`,
   `RegenerateMFABackupCodes`; `auth.TOTPCodeAt` for end-to-end tests. No
   new dependency or cryptography: the existing HMAC-SHA-1 TOTP, AES-256-GCM
   cipher and backup-code digests are reused.
6. **Audit.** Changes keep their ADR 0015 events; new backup codes record
   `auth.mfa.backup_codes_regenerated` (Forge-Rust's name). A refused
   step-up, a throttled one and a wrong code of a new key record the target
   action (`auth.mfa.enrollment_started`, `auth.mfa.enabled`,
   `auth.mfa.rotation_started`, `auth.mfa.rotated`,
   `auth.mfa.backup_codes_regenerated`, `auth.mfa.disabled`) as `denied`. A
   backup code used for step-up records `auth.mfa.backup_code_used`.
7. **Sessions.** Confirming an enrollment or a rotation and turning MFA off
   revoke every session of the account in the store (ADR 0015); the browser's
   session and CSRF cookies are cleared. The backup codes render in that
   response, on a `no-store` page with no form, then the user signs in again.
   New backup codes keep the sessions.

## Consequences

- Generated applications have the same two-factor pages, routes and
  messages as Forge-Rust's. The existing JSON lifecycle handlers and
  `MFAChangeAuthorizer` stay for applications that build their own flow.
- Applications that generated `htmlauth` before this change keep compiling:
  they own their copy, and `auth.BrowserOptions.Passwords` is optional. To
  add the pages, regenerate `app/adapters/htmlauth` and `app/views/mfa` and
  pass the session middleware to `htmlauth.Register`.
- Counters of the in-memory throttler are per process; the generated
  application uses the PostgreSQL throttler for MFA, shared by replicas.
- No QR image is rendered, as in Forge-Rust: the page shows the `otpauth://`
  link and the Base32 key.
