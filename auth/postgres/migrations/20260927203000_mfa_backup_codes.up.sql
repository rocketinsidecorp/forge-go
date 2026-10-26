CREATE TABLE forge_mfa_backup_codes (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES forge_users(id) ON DELETE CASCADE,
    code_digest bytea NOT NULL CHECK (octet_length(code_digest) = 32),
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT forge_mfa_backup_codes_user_digest_key UNIQUE (user_id, code_digest)
);

CREATE INDEX forge_mfa_backup_codes_active_user_idx
    ON forge_mfa_backup_codes (user_id)
    WHERE consumed_at IS NULL;
