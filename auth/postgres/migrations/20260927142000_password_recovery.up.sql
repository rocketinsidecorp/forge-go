CREATE TABLE forge_password_recovery (
    id uuid PRIMARY KEY,
    token_digest bytea NOT NULL CHECK (octet_length(token_digest) = 32),
    user_id uuid NOT NULL REFERENCES forge_users(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT forge_password_recovery_token_digest_key UNIQUE (token_digest)
);

CREATE INDEX forge_password_recovery_user_active_idx
    ON forge_password_recovery (user_id, expires_at)
    WHERE consumed_at IS NULL;
