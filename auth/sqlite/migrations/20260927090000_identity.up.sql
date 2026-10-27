-- Forge identity on SQLite: users, organizations, tenants, memberships,
-- roles and permissions, sessions, the login throttle, password recovery,
-- TOTP MFA and backup codes, and the security audit trail. It is the shape
-- auth/postgres reaches through its migrations, in one migration with the
-- same version (the first identity migration on every database).
--
-- IDs are UUIDv7 text the application generates (package uuid), never the
-- database. Timestamps are UTC text, "YYYY-MM-DD HH:MM:SS.ffffff", so they
-- compare and sort chronologically (as in queue/sqlite). Tables are STRICT.
-- There is no row-level security on SQLite: the generated repositories
-- filter every tenant-scoped query by the tenant in the request context
-- (ADR 0017).
CREATE TABLE forge_users (
    id TEXT NOT NULL PRIMARY KEY CHECK (length(id) = 36),
    email TEXT NOT NULL CHECK (length(email) BETWEEN 1 AND 320),
    email_normalized TEXT NOT NULL CHECK (length(email_normalized) BETWEEN 1 AND 320),
    password_hash TEXT NOT NULL CHECK (length(password_hash) BETWEEN 1 AND 1024),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    session_version INTEGER NOT NULL DEFAULT 1 CHECK (session_version > 0),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now')),
    CONSTRAINT forge_users_email_normalized_key UNIQUE (email_normalized)
) STRICT;

CREATE TABLE forge_organizations (
    id TEXT NOT NULL PRIMARY KEY CHECK (length(id) = 36),
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now'))
) STRICT;

CREATE TABLE forge_tenants (
    id TEXT NOT NULL PRIMARY KEY CHECK (length(id) = 36),
    organization_id TEXT NOT NULL REFERENCES forge_organizations (id),
    slug TEXT NOT NULL CHECK (length(slug) BETWEEN 2 AND 63 AND slug GLOB '[a-z]*' AND slug NOT GLOB '*[^a-z0-9-]*'),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now')),
    CONSTRAINT forge_tenants_slug_key UNIQUE (slug)
) STRICT;

CREATE TABLE forge_memberships (
    id TEXT NOT NULL PRIMARY KEY CHECK (length(id) = 36),
    user_id TEXT NOT NULL REFERENCES forge_users (id),
    tenant_id TEXT NOT NULL REFERENCES forge_tenants (id),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now')),
    CONSTRAINT forge_memberships_user_tenant_key UNIQUE (user_id, tenant_id),
    CONSTRAINT forge_memberships_id_tenant_key UNIQUE (id, tenant_id)
) STRICT;

CREATE TABLE forge_roles (
    id TEXT NOT NULL PRIMARY KEY CHECK (length(id) = 36),
    tenant_id TEXT NOT NULL REFERENCES forge_tenants (id),
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 63 AND name GLOB '[a-z]*' AND name NOT GLOB '*[^a-z0-9_.-]*'),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now')),
    CONSTRAINT forge_roles_tenant_name_key UNIQUE (tenant_id, name),
    CONSTRAINT forge_roles_id_tenant_key UNIQUE (id, tenant_id)
) STRICT;

-- resource:action, each part a lowercase letter then up to 62 of
-- [a-z0-9_.-] (auth.NewPermission).
CREATE TABLE forge_permissions (
    name TEXT NOT NULL PRIMARY KEY CHECK (
        name GLOB '[a-z]*:[a-z]*'
        AND name NOT GLOB '*[^a-z0-9_.:-]*'
        AND length(name) - length(replace(name, ':', '')) = 1
        AND instr(name, ':') BETWEEN 2 AND 64
        AND length(name) - instr(name, ':') BETWEEN 1 AND 63
    )
) STRICT;

CREATE TABLE forge_role_permissions (
    role_id TEXT NOT NULL REFERENCES forge_roles (id) ON DELETE CASCADE,
    permission TEXT NOT NULL REFERENCES forge_permissions (name) ON DELETE RESTRICT,
    PRIMARY KEY (role_id, permission)
) STRICT;

CREATE TABLE forge_membership_roles (
    membership_id TEXT NOT NULL,
    role_id TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    PRIMARY KEY (membership_id, role_id),
    CONSTRAINT forge_membership_roles_membership_tenant_fk
        FOREIGN KEY (membership_id, tenant_id)
        REFERENCES forge_memberships (id, tenant_id)
        ON DELETE CASCADE,
    CONSTRAINT forge_membership_roles_role_tenant_fk
        FOREIGN KEY (role_id, tenant_id)
        REFERENCES forge_roles (id, tenant_id)
        ON DELETE CASCADE
) STRICT;

CREATE TABLE forge_sessions (
    id TEXT NOT NULL PRIMARY KEY CHECK (length(id) = 36),
    token_digest BLOB NOT NULL CHECK (length(token_digest) = 32),
    membership_id TEXT NOT NULL REFERENCES forge_memberships (id) ON DELETE CASCADE,
    credential_version INTEGER NOT NULL CHECK (credential_version > 0),
    expires_at TEXT NOT NULL,
    revoked_at TEXT,
    -- When the session was last used, for the idle timeout.
    last_seen_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now')),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now')),
    CONSTRAINT forge_sessions_token_digest_key UNIQUE (token_digest)
) STRICT;

CREATE INDEX forge_memberships_tenant_user_idx ON forge_memberships (tenant_id, user_id);
CREATE INDEX forge_membership_roles_role_idx ON forge_membership_roles (role_id);
CREATE INDEX forge_roles_tenant_idx ON forge_roles (tenant_id);
CREATE INDEX forge_sessions_membership_idx ON forge_sessions (membership_id);
CREATE INDEX forge_sessions_active_expiry_idx ON forge_sessions (expires_at) WHERE revoked_at IS NULL;

-- Login, MFA and recovery throttle counters: scope 1 account, 2 source,
-- 3 account and source together (auth.LoginThrottleKey). A counter is
-- created empty (attempt_count 0) before it is incremented.
CREATE TABLE forge_login_throttle (
    scope INTEGER NOT NULL CHECK (scope IN (1, 2, 3)),
    key_hash BLOB NOT NULL CHECK (length(key_hash) = 32),
    attempt_count INTEGER NOT NULL CHECK (attempt_count >= 0),
    reset_at TEXT NOT NULL,
    PRIMARY KEY (scope, key_hash)
) STRICT;

CREATE INDEX forge_login_throttle_reset_at_idx ON forge_login_throttle (reset_at);

CREATE TABLE forge_password_recovery (
    id TEXT NOT NULL PRIMARY KEY CHECK (length(id) = 36),
    token_digest BLOB NOT NULL CHECK (length(token_digest) = 32),
    user_id TEXT NOT NULL REFERENCES forge_users (id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,
    consumed_at TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now')),
    CONSTRAINT forge_password_recovery_token_digest_key UNIQUE (token_digest)
) STRICT;

CREATE INDEX forge_password_recovery_user_active_idx
    ON forge_password_recovery (user_id, expires_at)
    WHERE consumed_at IS NULL;

CREATE TABLE forge_totp_enrollments (
    user_id TEXT NOT NULL PRIMARY KEY REFERENCES forge_users (id) ON DELETE CASCADE,
    secret_digest BLOB NOT NULL CHECK (length(secret_digest) = 32),
    secret_ciphertext BLOB NOT NULL CHECK (length(secret_ciphertext) > 0),
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now'))
) STRICT;

CREATE TABLE forge_totp_factors (
    user_id TEXT NOT NULL PRIMARY KEY REFERENCES forge_users (id) ON DELETE CASCADE,
    secret_digest BLOB NOT NULL CHECK (length(secret_digest) = 32),
    secret_ciphertext BLOB NOT NULL CHECK (length(secret_ciphertext) > 0),
    last_counter INTEGER NOT NULL DEFAULT -1 CHECK (last_counter >= -1),
    enabled_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now'))
) STRICT;

CREATE TABLE forge_mfa_challenges (
    id TEXT NOT NULL PRIMARY KEY CHECK (length(id) = 36),
    token_digest BLOB NOT NULL CHECK (length(token_digest) = 32),
    membership_id TEXT NOT NULL REFERENCES forge_memberships (id) ON DELETE CASCADE,
    credential_version INTEGER NOT NULL CHECK (credential_version > 0),
    factor_secret_digest BLOB NOT NULL CHECK (length(factor_secret_digest) = 32),
    expires_at TEXT NOT NULL,
    session_expires_at TEXT NOT NULL,
    consumed_at TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now')),
    CONSTRAINT forge_mfa_challenges_token_digest_key UNIQUE (token_digest)
) STRICT;

CREATE INDEX forge_mfa_challenges_membership_idx ON forge_mfa_challenges (membership_id);
CREATE INDEX forge_mfa_challenges_active_expiry_idx ON forge_mfa_challenges (expires_at) WHERE consumed_at IS NULL;

CREATE TABLE forge_mfa_backup_codes (
    id TEXT NOT NULL PRIMARY KEY CHECK (length(id) = 36),
    user_id TEXT NOT NULL REFERENCES forge_users (id) ON DELETE CASCADE,
    code_digest BLOB NOT NULL CHECK (length(code_digest) = 32),
    consumed_at TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now')),
    CONSTRAINT forge_mfa_backup_codes_user_digest_key UNIQUE (user_id, code_digest)
) STRICT;

CREATE INDEX forge_mfa_backup_codes_active_user_idx ON forge_mfa_backup_codes (user_id) WHERE consumed_at IS NULL;

CREATE TABLE forge_security_audit_events (
    id TEXT NOT NULL PRIMARY KEY CHECK (length(id) = 36),
    kind TEXT NOT NULL CHECK (length(kind) BETWEEN 3 AND 96 AND kind GLOB '[a-z]*' AND kind NOT GLOB '*[^a-z0-9_.-]*'),
    outcome TEXT NOT NULL CHECK (outcome IN ('succeeded', 'denied', 'failed')),
    actor_id TEXT CHECK (actor_id IS NULL OR length(actor_id) = 36),
    subject_id TEXT CHECK (subject_id IS NULL OR length(subject_id) = 36),
    membership_id TEXT CHECK (membership_id IS NULL OR length(membership_id) = 36),
    account_digest BLOB CHECK (account_digest IS NULL OR length(account_digest) = 32),
    source_digest BLOB CHECK (source_digest IS NULL OR length(source_digest) = 32),
    credential_digest BLOB CHECK (credential_digest IS NULL OR length(credential_digest) = 32),
    request_id TEXT CHECK (request_id IS NULL OR length(request_id) <= 128),
    occurred_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now'))
) STRICT;

CREATE INDEX forge_security_audit_occurred_idx ON forge_security_audit_events (occurred_at DESC, id DESC);
CREATE INDEX forge_security_audit_subject_idx ON forge_security_audit_events (subject_id, occurred_at DESC) WHERE subject_id IS NOT NULL;
CREATE INDEX forge_security_audit_membership_idx ON forge_security_audit_events (membership_id, occurred_at DESC) WHERE membership_id IS NOT NULL;
CREATE INDEX forge_security_audit_kind_idx ON forge_security_audit_events (kind, occurred_at DESC);
