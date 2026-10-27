-- Forge identity on MySQL 8.4 and MariaDB 11.8: users, organizations,
-- tenants, memberships, roles and permissions, sessions, the login throttle,
-- password recovery, TOTP MFA and backup codes, and the security audit
-- trail. It is the shape auth/postgres reaches through its migrations, in one
-- migration with the same version (the first identity migration on every
-- database).
--
-- IDs are UUIDv7 text the application generates (package uuid), never the
-- database, stored as CHAR(36) ascii. Timestamps are DATETIME(6) in UTC (every
-- Forge session runs with time_zone '+00:00'). Identifiers and other
-- machine values use the binary ascii collation, so comparisons and the
-- REGEXP checks are exact and case-sensitive. There is no row-level security
-- on MySQL: the generated repositories filter every tenant-scoped query by the
-- tenant in the request context (ADR 0017).
CREATE TABLE forge_users (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    email VARCHAR(320) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    email_normalized VARCHAR(320) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    password_hash VARCHAR(1024) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'active',
    session_version BIGINT NOT NULL DEFAULT 1,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY forge_users_email_normalized_key (email_normalized),
    CONSTRAINT forge_users_status_check CHECK (status IN ('active', 'disabled')),
    CONSTRAINT forge_users_session_version_check CHECK (session_version > 0)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE forge_organizations (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(200) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'active',
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    CONSTRAINT forge_organizations_name_check CHECK (CHAR_LENGTH(name) BETWEEN 1 AND 200),
    CONSTRAINT forge_organizations_status_check CHECK (status IN ('active', 'disabled'))
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE forge_tenants (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    organization_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    slug VARCHAR(63) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'active',
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY forge_tenants_slug_key (slug),
    CONSTRAINT forge_tenants_organization_fk FOREIGN KEY (organization_id) REFERENCES forge_organizations (id),
    CONSTRAINT forge_tenants_slug_check CHECK (slug REGEXP '^[a-z][a-z0-9-]{1,62}$'),
    CONSTRAINT forge_tenants_status_check CHECK (status IN ('active', 'disabled'))
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE forge_memberships (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'active',
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY forge_memberships_user_tenant_key (user_id, tenant_id),
    UNIQUE KEY forge_memberships_id_tenant_key (id, tenant_id),
    KEY forge_memberships_tenant_user_idx (tenant_id, user_id),
    CONSTRAINT forge_memberships_user_fk FOREIGN KEY (user_id) REFERENCES forge_users (id),
    CONSTRAINT forge_memberships_tenant_fk FOREIGN KEY (tenant_id) REFERENCES forge_tenants (id),
    CONSTRAINT forge_memberships_status_check CHECK (status IN ('active', 'suspended'))
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE forge_roles (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(63) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY forge_roles_tenant_name_key (tenant_id, name),
    UNIQUE KEY forge_roles_id_tenant_key (id, tenant_id),
    CONSTRAINT forge_roles_tenant_fk FOREIGN KEY (tenant_id) REFERENCES forge_tenants (id),
    CONSTRAINT forge_roles_name_check CHECK (name REGEXP '^[a-z][a-z0-9_.-]{0,62}$')
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE forge_permissions (
    name VARCHAR(127) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (name),
    CONSTRAINT forge_permissions_name_check CHECK (name REGEXP '^[a-z][a-z0-9_.-]{0,62}:[a-z][a-z0-9_.-]{0,62}$')
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE forge_role_permissions (
    role_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    permission VARCHAR(127) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (role_id, permission),
    KEY forge_role_permissions_permission_idx (permission),
    CONSTRAINT forge_role_permissions_role_fk FOREIGN KEY (role_id) REFERENCES forge_roles (id) ON DELETE CASCADE,
    CONSTRAINT forge_role_permissions_permission_fk FOREIGN KEY (permission) REFERENCES forge_permissions (name) ON DELETE RESTRICT
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE forge_membership_roles (
    membership_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    role_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (membership_id, role_id),
    KEY forge_membership_roles_role_idx (role_id),
    KEY forge_membership_roles_membership_tenant_idx (membership_id, tenant_id),
    KEY forge_membership_roles_role_tenant_idx (role_id, tenant_id),
    CONSTRAINT forge_membership_roles_membership_tenant_fk
        FOREIGN KEY (membership_id, tenant_id) REFERENCES forge_memberships (id, tenant_id) ON DELETE CASCADE,
    CONSTRAINT forge_membership_roles_role_tenant_fk
        FOREIGN KEY (role_id, tenant_id) REFERENCES forge_roles (id, tenant_id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE forge_sessions (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    token_digest BINARY(32) NOT NULL,
    membership_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    credential_version BIGINT NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    revoked_at DATETIME(6) NULL,
    -- When the session was last used, for the idle timeout.
    last_seen_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY forge_sessions_token_digest_key (token_digest),
    KEY forge_sessions_membership_idx (membership_id),
    KEY forge_sessions_expiry_idx (revoked_at, expires_at),
    CONSTRAINT forge_sessions_membership_fk FOREIGN KEY (membership_id) REFERENCES forge_memberships (id) ON DELETE CASCADE,
    CONSTRAINT forge_sessions_credential_version_check CHECK (credential_version > 0)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

-- Login, MFA and recovery throttle counters: scope 1 account, 2 source,
-- 3 account and source together (auth.LoginThrottleKey). A counter is
-- created empty (attempt_count 0) before it is incremented.
CREATE TABLE forge_login_throttle (
    scope SMALLINT NOT NULL,
    key_hash BINARY(32) NOT NULL,
    attempt_count INT NOT NULL,
    reset_at DATETIME(6) NOT NULL,
    PRIMARY KEY (scope, key_hash),
    KEY forge_login_throttle_reset_at_idx (reset_at),
    CONSTRAINT forge_login_throttle_scope_check CHECK (scope IN (1, 2, 3)),
    CONSTRAINT forge_login_throttle_attempt_count_check CHECK (attempt_count >= 0)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE forge_password_recovery (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    token_digest BINARY(32) NOT NULL,
    user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    consumed_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY forge_password_recovery_token_digest_key (token_digest),
    KEY forge_password_recovery_user_active_idx (user_id, consumed_at, expires_at),
    CONSTRAINT forge_password_recovery_user_fk FOREIGN KEY (user_id) REFERENCES forge_users (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE forge_totp_enrollments (
    user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    secret_digest BINARY(32) NOT NULL,
    secret_ciphertext VARBINARY(1024) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (user_id),
    KEY forge_totp_enrollments_expiry_idx (expires_at),
    CONSTRAINT forge_totp_enrollments_user_fk FOREIGN KEY (user_id) REFERENCES forge_users (id) ON DELETE CASCADE,
    CONSTRAINT forge_totp_enrollments_ciphertext_check CHECK (LENGTH(secret_ciphertext) > 0)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE forge_totp_factors (
    user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    secret_digest BINARY(32) NOT NULL,
    secret_ciphertext VARBINARY(1024) NOT NULL,
    last_counter BIGINT NOT NULL DEFAULT -1,
    enabled_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (user_id),
    CONSTRAINT forge_totp_factors_user_fk FOREIGN KEY (user_id) REFERENCES forge_users (id) ON DELETE CASCADE,
    CONSTRAINT forge_totp_factors_ciphertext_check CHECK (LENGTH(secret_ciphertext) > 0),
    CONSTRAINT forge_totp_factors_counter_check CHECK (last_counter >= -1)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE forge_mfa_challenges (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    token_digest BINARY(32) NOT NULL,
    membership_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    credential_version BIGINT NOT NULL,
    factor_secret_digest BINARY(32) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    session_expires_at DATETIME(6) NOT NULL,
    consumed_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY forge_mfa_challenges_token_digest_key (token_digest),
    KEY forge_mfa_challenges_membership_idx (membership_id),
    KEY forge_mfa_challenges_expiry_idx (consumed_at, expires_at),
    CONSTRAINT forge_mfa_challenges_membership_fk FOREIGN KEY (membership_id) REFERENCES forge_memberships (id) ON DELETE CASCADE,
    CONSTRAINT forge_mfa_challenges_credential_version_check CHECK (credential_version > 0)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE forge_mfa_backup_codes (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    code_digest BINARY(32) NOT NULL,
    consumed_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY forge_mfa_backup_codes_user_digest_key (user_id, code_digest),
    CONSTRAINT forge_mfa_backup_codes_user_fk FOREIGN KEY (user_id) REFERENCES forge_users (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE forge_security_audit_events (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    kind VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    outcome VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    actor_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    subject_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    membership_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    account_digest BINARY(32) NULL,
    source_digest BINARY(32) NULL,
    credential_digest BINARY(32) NULL,
    request_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
    occurred_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    KEY forge_security_audit_occurred_idx (occurred_at, id),
    KEY forge_security_audit_subject_idx (subject_id, occurred_at),
    KEY forge_security_audit_membership_idx (membership_id, occurred_at),
    KEY forge_security_audit_kind_idx (kind, occurred_at),
    CONSTRAINT forge_security_audit_kind_check CHECK (kind REGEXP '^[a-z][a-z0-9_.-]{2,95}$'),
    CONSTRAINT forge_security_audit_outcome_check CHECK (outcome IN ('succeeded', 'denied', 'failed'))
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;
