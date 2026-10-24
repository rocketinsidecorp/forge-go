# Security Audit Events

Forge Phase 3 provides a structured security-audit foundation in `auth` and
PostgreSQL persistence through `auth/postgres.Repository`.

## Event shape

`auth.SecurityEvent` intentionally has a closed set of fields:

- event kind and outcome;
- optional actor, subject, and membership UUIDv7 identifiers;
- optional account/source/credential SHA-256 digests;
- optional occurrence time.

There is no arbitrary metadata map. Passwords, bearer tokens, password-recovery
links, TOTP seeds, MFA challenge tokens, and backup codes must never be placed
in audit records.

The event kinds defined by the framework cover login, session revocation,
password recovery/reset, MFA enrollment/challenge/backup-code use, factor
rotation, and factor disable. Applications may add names that follow the same
lowercase dotted syntax, but should keep event payloads within the fixed safe
field set.

## PostgreSQL storage

`auth/postgres.Migrations()` adds `forge_security_audit_events`.

The table stores:

- UUIDv7 event ID;
- kind and outcome;
- actor/subject/membership identifiers without foreign keys;
- account/source/credential digests;
- request ID;
- occurrence timestamp.

Identity IDs deliberately have no foreign-key constraints. Audit history must
survive identity deletion or later schema cleanup.

`Repository.RecordSecurityEvent` validates the event before insert and derives
the request ID from `web.RequestID(ctx)`. When `OccurredAt` is omitted, the
PostgreSQL database clock records the event time.

The repository exposes insertion only; it has no update/delete API for audit
events. Database owners can still modify the table, so this is append-only by
application contract rather than tamper-proof storage.

## Operational guidance

Treat the audit table as security-sensitive data. Grant the serving role insert
and the minimum read access required by operations. Administrative reporting
should use a separate read path/role where practical.

Audit events are not a place for debugging payloads. Use request-correlated
application logs for detailed diagnostics and keep secrets out of both systems.

The Phase 3 identity services now wire this contract through password login,
logout, password recovery/reset, MFA enrollment, challenge completion, backup
code use, factor rotation and factor disable. Application-specific identity
operations should use the same closed event shape.

See [ADR 0016](adr/0016-structured-security-audit.md).


## Login and logout instrumentation

`LoginService` can receive a `SecurityAuditor` with
`auth.WithSecurityAuditor`.

For syntactically valid attempts it records only hashed account/source keys and,
when the identity is known, subject and membership IDs:

- `auth.login.failed` / `denied` for an unknown identity, wrong password,
  or credential-upgrade race;
- `auth.login.throttled` / `denied` when throttling blocks the attempt;
- `auth.login.succeeded` / `succeeded` after the opaque session is created.

Audit failure never turns a denied login into a successful login. The returned
error keeps the original credentials/throttling classification and also carries
a typed `SecurityAuditError`, allowing HTTP adapters to log the subsystem
failure while preserving the public `401` or `429`.

For successful login, the token is not returned to the caller until the success
event is persisted. If audit persistence fails and the session backend also
implements `SessionRevoker`, Forge revokes the just-created session before
returning the audit error.

`NewAuditedLogoutHandler` records `auth.session.revoked` using only the
SHA-256 credential digest. Once revocation succeeds, an audit-storage failure
is logged but does not change the idempotent `204` response: the security
action already happened and retrying logout does not improve its result.


## Recovery and MFA instrumentation

`RecoveryService` accepts `WithRecoveryAuditor`. Recovery-request events use
the domain-separated account/source digests already produced for throttling;
known identities may additionally carry the subject ID, and issued links are
represented only by their SHA-256 credential digest. Unknown identities are
audited as denied without introducing a different public response.

Password-reset attempts audit the recovery-token digest and source digest. A
successful reset has already changed the password, incremented the credential
version, revoked sessions and consumed recovery links before the append occurs.
If only that audit append fails, `SecurityAuditError.OperationApplied` is true;
the HTTP adapter logs the audit failure and still returns `204` so clients do
not retry a reset that already committed.

`MFAService` accepts `WithMFAAuditor` and emits lifecycle events for
enrollment start/enable, rotation start/complete and disable. MFA login failures
emit `auth.mfa.challenge_failed`; successful backup-code use emits
`auth.mfa.backup_code_used`; a completed MFA login also emits
`auth.login.succeeded`. `RegenerateMFABackupCodes` emits
`auth.mfa.backup_codes_regenerated`, and a backup code used up by a step-up
(`VerifyMFACode`, or a change given a `VerifyMFAStepUp` proof once it
commits) emits `auth.mfa.backup_code_used`.

The two-factor settings (`auth.Browser`, the generated `/mfa` pages) record a
refused, throttled or wrong-code change as its target action with outcome
`denied`: `auth.mfa.enrollment_started`, `auth.mfa.enabled`,
`auth.mfa.rotation_started`, `auth.mfa.rotated`,
`auth.mfa.backup_codes_regenerated` or `auth.mfa.disabled`, with the account
as actor and subject and the source digest
([ADR 0045](adr/0045-generated-mfa-settings-pages.md)).

A completed MFA login does not release its bearer token until required audit
events are persisted. If a success audit append fails, Forge revokes the
newly-created session when the session adapter supports revocation and returns
an error instead of exposing the token.

Some MFA lifecycle operations cannot be safely rolled back after the database
transition commits, especially confirmation because backup codes are generated
once. Those operations return their result together with a typed audit failure
marked `OperationApplied`; the HTTP adapters log the audit failure and preserve
the successful response, including one-time backup codes.
