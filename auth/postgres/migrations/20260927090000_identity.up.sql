-- IDs are UUIDv7 values the application generates (package uuid), never
-- the database: the id columns have no default, so PostgreSQL 16 and 17
-- apply these migrations too.
CREATE TABLE forge_users (
    id uuid PRIMARY KEY,
    email text NOT NULL,
    email_normalized text NOT NULL,
    password_hash text NOT NULL,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    session_version bigint NOT NULL DEFAULT 1 CHECK (session_version > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT forge_users_email_normalized_key UNIQUE (email_normalized)
);

CREATE TABLE forge_organizations (
    id uuid PRIMARY KEY,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE forge_tenants (
    id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES forge_organizations(id),
    slug text NOT NULL CHECK (slug ~ '^[a-z][a-z0-9-]{1,62}$'),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT forge_tenants_slug_key UNIQUE (slug)
);

CREATE TABLE forge_memberships (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES forge_users(id),
    tenant_id uuid NOT NULL REFERENCES forge_tenants(id),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT forge_memberships_user_tenant_key UNIQUE (user_id, tenant_id),
    CONSTRAINT forge_memberships_id_tenant_key UNIQUE (id, tenant_id)
);

CREATE TABLE forge_roles (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES forge_tenants(id),
    name text NOT NULL CHECK (name ~ '^[a-z][a-z0-9_.-]{0,62}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT forge_roles_tenant_name_key UNIQUE (tenant_id, name),
    CONSTRAINT forge_roles_id_tenant_key UNIQUE (id, tenant_id)
);

CREATE TABLE forge_permissions (
    name text PRIMARY KEY CHECK (name ~ '^[a-z][a-z0-9_.-]{0,62}:[a-z][a-z0-9_.-]{0,62}$')
);

CREATE TABLE forge_role_permissions (
    role_id uuid NOT NULL REFERENCES forge_roles(id) ON DELETE CASCADE,
    permission text NOT NULL REFERENCES forge_permissions(name) ON DELETE RESTRICT,
    PRIMARY KEY (role_id, permission)
);

CREATE TABLE forge_membership_roles (
    membership_id uuid NOT NULL,
    role_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    PRIMARY KEY (membership_id, role_id),
    CONSTRAINT forge_membership_roles_membership_tenant_fk
        FOREIGN KEY (membership_id, tenant_id)
        REFERENCES forge_memberships(id, tenant_id)
        ON DELETE CASCADE,
    CONSTRAINT forge_membership_roles_role_tenant_fk
        FOREIGN KEY (role_id, tenant_id)
        REFERENCES forge_roles(id, tenant_id)
        ON DELETE CASCADE
);

CREATE TABLE forge_sessions (
    id uuid PRIMARY KEY,
    token_digest bytea NOT NULL CHECK (octet_length(token_digest) = 32),
    membership_id uuid NOT NULL REFERENCES forge_memberships(id) ON DELETE CASCADE,
    credential_version bigint NOT NULL CHECK (credential_version > 0),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT forge_sessions_token_digest_key UNIQUE (token_digest)
);

CREATE INDEX forge_memberships_tenant_user_idx
    ON forge_memberships (tenant_id, user_id);

CREATE INDEX forge_membership_roles_role_idx
    ON forge_membership_roles (role_id);

CREATE INDEX forge_sessions_membership_idx
    ON forge_sessions (membership_id);

CREATE INDEX forge_sessions_active_expiry_idx
    ON forge_sessions (expires_at)
    WHERE revoked_at IS NULL;
