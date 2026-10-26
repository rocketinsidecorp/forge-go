CREATE TABLE forge_security_audit_events (
    id uuid PRIMARY KEY,
    kind text NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_.-]{2,95}$'),
    outcome text NOT NULL CHECK (outcome IN ('succeeded', 'denied', 'failed')),
    actor_id uuid,
    subject_id uuid,
    membership_id uuid,
    account_digest bytea CHECK (account_digest IS NULL OR octet_length(account_digest) = 32),
    source_digest bytea CHECK (source_digest IS NULL OR octet_length(source_digest) = 32),
    credential_digest bytea CHECK (credential_digest IS NULL OR octet_length(credential_digest) = 32),
    request_id varchar(128),
    occurred_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX forge_security_audit_occurred_idx
    ON forge_security_audit_events (occurred_at DESC, id DESC);

CREATE INDEX forge_security_audit_subject_idx
    ON forge_security_audit_events (subject_id, occurred_at DESC)
    WHERE subject_id IS NOT NULL;

CREATE INDEX forge_security_audit_membership_idx
    ON forge_security_audit_events (membership_id, occurred_at DESC)
    WHERE membership_id IS NOT NULL;

CREATE INDEX forge_security_audit_kind_idx
    ON forge_security_audit_events (kind, occurred_at DESC);
