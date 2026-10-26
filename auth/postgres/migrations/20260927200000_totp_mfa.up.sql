CREATE TABLE forge_totp_enrollments (
    user_id uuid PRIMARY KEY REFERENCES forge_users(id) ON DELETE CASCADE,
    secret_digest bytea NOT NULL CHECK (octet_length(secret_digest) = 32),
    secret_ciphertext bytea NOT NULL CHECK (octet_length(secret_ciphertext) > 0),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE forge_totp_factors (
    user_id uuid PRIMARY KEY REFERENCES forge_users(id) ON DELETE CASCADE,
    secret_digest bytea NOT NULL CHECK (octet_length(secret_digest) = 32),
    secret_ciphertext bytea NOT NULL CHECK (octet_length(secret_ciphertext) > 0),
    last_counter bigint NOT NULL DEFAULT -1 CHECK (last_counter >= -1),
    enabled_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE forge_mfa_challenges (
    id uuid PRIMARY KEY,
    token_digest bytea NOT NULL CHECK (octet_length(token_digest) = 32),
    membership_id uuid NOT NULL REFERENCES forge_memberships(id) ON DELETE CASCADE,
    credential_version bigint NOT NULL CHECK (credential_version > 0),
    factor_secret_digest bytea NOT NULL CHECK (octet_length(factor_secret_digest) = 32),
    expires_at timestamptz NOT NULL,
    session_expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT forge_mfa_challenges_token_digest_key UNIQUE (token_digest)
);

CREATE INDEX forge_mfa_challenges_membership_idx
    ON forge_mfa_challenges (membership_id);

CREATE INDEX forge_mfa_challenges_active_expiry_idx
    ON forge_mfa_challenges (expires_at)
    WHERE consumed_at IS NULL;
