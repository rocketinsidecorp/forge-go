# ADR 0013: One-time password recovery with global session revocation

Status: accepted (Phase 3)

## Decision

Forge password recovery uses a distinct opaque 256-bit recovery token. The
plaintext token is delivered once through an application-supplied
`RecoverySender`; persistence stores only its SHA-256 digest.

Recovery requests remain tenant-scoped so an application can require a current
active membership in the named tenant before issuing a reset. The password
credential itself remains user-global: consuming a reset token changes the
user's password for every tenant membership.

The public recovery-request path is non-enumerating. Unknown identities and
syntactically invalid identifiers return the same accepted result as known
identities. Valid requests are throttled independently by domain-separated
tenant+account and source hashes. Reset attempts are also throttled by
domain-separated token and source hashes before password hashing.

`RecoverySender` implementations should enqueue delivery and return promptly
rather than perform slow SMTP or other network delivery inline. Delivery
failures are surfaced to non-HTTP callers as a typed error, but the public
recovery-request handler deliberately masks them as the normal accepted
response and invalidates the undelivered token. This prevents a provider outage
from becoming an account-existence oracle. The framework deliberately does not
choose an email provider.

The PostgreSQL adapter stores recovery rows in
`forge_password_recovery`. Creating a new recovery token serializes on the
user row and invalidates every previous unused token for that user.

Token consumption is one transaction:

1. identify the active user for the digest;
2. lock that user row;
3. revalidate that the token is unused and unexpired;
4. replace the password hash and increment `session_version`;
5. revoke every active session for the user;
6. mark every outstanding recovery token for the user consumed.

The user-row lock is the serialization point for creation, invalidation and
consumption, so two valid reset links racing for the same account cannot both
win.

Expired or consumed rows can be removed in bounded batches with
`Repository.PruneRecovery`, which uses `SKIP LOCKED` so multiple cleanup
workers can run concurrently.

## Consequences

- A database disclosure does not reveal usable password-reset links.
- A password reset signs the user out everywhere by revoking sessions and
  incrementing the credential version.
- Issuing a new recovery link supersedes previous unused links.
- Successful use invalidates every other outstanding recovery link.
- Delivery infrastructure remains application-owned through a narrow port.
- Recovery request timing used to depend on the application's sender
  behavior; see the addendum: the account-dependent work now runs after the
  answer.
- Phase 4 scheduling can automate bounded cleanup later.

## Addendum (2026-10-20): off the request path

`RecoveryService.Request` no longer does account-dependent work inline when a
`RecoveryDispatcher` is configured: it canonicalizes the login
(`auth.CanonicalLogin`; a non-ASCII address is an unknown account), counts the
request (3 per login and source, 10 per login, 30 per source per hour; IPv6
per `/64`), dispatches it and answers no sooner than 100 ms. `Process` looks the
account up, issues the token and delivers it to the stored address. The
dispatcher is the bounded, lifecycle-owned `auth.RecoveryQueue` or the
persistent-queue adapter `auth/recoveryqueue` (docs/ACCOUNT_RECOVERY.md).
`Reset` asks a `RecoveryTokenChecker` store (PostgreSQL implements it) whether
the token exists, is unused and unexpired before hashing the new password.
