package sqlidentity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/auth/internal/setupcmd"
	"github.com/rocketinsidecorp/forge-go/command"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

var (
	// ErrAlreadyProvisioned refuses setup on a database that already has a
	// tenant: setup creates the first tenant and owner only, so it can never
	// add an account to, or take over, a running installation.
	ErrAlreadyProvisioned = errors.New("identity is already set up: a tenant exists, and setup only provisions an empty identity database")
	// ErrOwnerEmailTaken means a user with the owner's email already exists.
	ErrOwnerEmailTaken = errors.New("a user with this email already exists")
)

// SetupLockName names the lock that serializes setup runs.
const SetupLockName = "forge:identity_setup"

// OwnerAccount is the first tenant and its owner.
type OwnerAccount struct {
	// Organization is the display name of the organization that owns the
	// tenant: 1 to 200 characters, no control characters.
	Organization string
	// TenantSlug names the tenant at sign-in: ^[a-z][a-z0-9-]{1,62}$.
	TenantSlug string
	// Email is the owner's sign-in email.
	Email string
	// Password is hashed with auth.DefaultPasswordHasher (Argon2id) before
	// the transaction starts; it is never stored or logged.
	Password string
}

// ProvisionedOwner describes what ProvisionOwner created.
type ProvisionedOwner struct {
	OrganizationID uuid.UUID
	TenantID       uuid.UUID
	TenantSlug     string
	UserID         uuid.UUID
	MembershipID   uuid.UUID
	RoleID         uuid.UUID
	Email          string
	// Permissions are the permissions granted to the owner role.
	Permissions []string
}

// ProvisionOwner creates the first organization, tenant, owner user, active
// membership and owner role (granted every permission in forge_permissions)
// in one transaction, together with an auth.identity.provisioned audit event.
// It runs under the dialect's setup lock and returns ErrAlreadyProvisioned
// when any tenant exists, so concurrent or repeated runs create nothing.
// Identifiers are UUIDv7 values generated in Go.
func (r *Repository) ProvisionOwner(ctx context.Context, account OwnerAccount) (ProvisionedOwner, error) {
	email, tenant, ok := auth.NormalizeIdentity(account.Email, account.TenantSlug)
	if !ok {
		return ProvisionedOwner{}, errors.New("owner email or tenant slug is invalid (email: ASCII only; slug: lowercase letter, then 1-62 lowercase letters, digits or hyphens)")
	}
	organization := strings.TrimSpace(account.Organization)
	if err := setupcmd.ValidOrganizationName(organization); err != nil {
		return ProvisionedOwner{}, err
	}
	passwordHash, err := auth.DefaultPasswordHasher().Hash(ctx, account.Password)
	if err != nil {
		return ProvisionedOwner{}, fmt.Errorf("owner password: %w", err)
	}
	owner := ProvisionedOwner{TenantSlug: tenant, Email: strings.TrimSpace(account.Email)}
	for _, id := range []*uuid.UUID{&owner.OrganizationID, &owner.TenantID, &owner.UserID, &owner.MembershipID, &owner.RoleID} {
		if *id, err = uuid.New(); err != nil {
			return ProvisionedOwner{}, err
		}
	}
	err = r.dialect.SetupLock(ctx, r.db, SetupLockName, r.dialect.Tx, func(tx *sql.Tx) error {
		owner.Permissions = nil
		var provisioned bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM forge_tenants)`).Scan(&provisioned); err != nil {
			return r.translate(err)
		}
		if provisioned {
			return ErrAlreadyProvisioned
		}
		statements := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO forge_organizations (id, name, created_at, updated_at) VALUES (?, ?, {now}, {now})`, []any{owner.OrganizationID, organization}},
			{`INSERT INTO forge_tenants (id, organization_id, slug, created_at, updated_at) VALUES (?, ?, ?, {now}, {now})`, []any{owner.TenantID, owner.OrganizationID, tenant}},
			{`INSERT INTO forge_users (id, email, email_normalized, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?, {now}, {now})`, []any{owner.UserID, owner.Email, email, passwordHash}},
			{`INSERT INTO forge_memberships (id, user_id, tenant_id, created_at, updated_at) VALUES (?, ?, ?, {now}, {now})`, []any{owner.MembershipID, owner.UserID, owner.TenantID}},
			{`INSERT INTO forge_roles (id, tenant_id, name, created_at) VALUES (?, ?, ?, {now})`, []any{owner.RoleID, owner.TenantID, setupcmd.OwnerRole}},
			{`INSERT INTO forge_membership_roles (membership_id, role_id, tenant_id) VALUES (?, ?, ?)`, []any{owner.MembershipID, owner.RoleID, owner.TenantID}},
			{`INSERT INTO forge_role_permissions (role_id, permission) SELECT ?, name FROM forge_permissions`, []any{owner.RoleID}},
		}
		for i, statement := range statements {
			if _, err := tx.ExecContext(ctx, r.q(statement.sql), statement.args...); err != nil {
				// The users insert can only collide on the email: the id is
				// new.
				if i == 2 && r.dialect.IsUniqueViolation(err) {
					return ErrOwnerEmailTaken
				}
				return r.translate(err)
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT permission FROM forge_role_permissions WHERE role_id = ? ORDER BY permission`, owner.RoleID)
		if err != nil {
			return r.translate(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var permission string
			if err := rows.Scan(&permission); err != nil {
				return r.translate(err)
			}
			owner.Permissions = append(owner.Permissions, permission)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return r.translate(err)
		}
		return r.recordSecurityEvent(ctx, tx, auth.SecurityEvent{
			Kind:         auth.SecurityIdentityProvisioned,
			Outcome:      auth.SecurityOutcomeSucceeded,
			SubjectID:    owner.UserID,
			MembershipID: owner.MembershipID,
		})
	})
	if err != nil {
		return ProvisionedOwner{}, err
	}
	return owner, nil
}

// Provision is setup's Provision for dialect: it opens the database env
// configures with open, creates the first organization and owner with
// ProvisionOwner and closes the database.
func Provision(open func(context.Context, config.Database) (*sqldb.DB, error), dialect *Dialect) func(context.Context, command.Env, setupcmd.Account) (setupcmd.Provisioned, error) {
	return func(ctx context.Context, env command.Env, account setupcmd.Account) (_ setupcmd.Provisioned, err error) {
		database, err := open(ctx, env.Config.Database)
		if err != nil {
			return setupcmd.Provisioned{}, err
		}
		defer func() { err = errors.Join(err, database.Close()) }()
		repository, err := New(database, dialect)
		if err != nil {
			return setupcmd.Provisioned{}, err
		}
		owner, err := repository.ProvisionOwner(ctx, OwnerAccount{
			Organization: account.OrganizationName,
			TenantSlug:   account.OrgSlug,
			Email:        account.Email,
			Password:     account.Password,
		})
		if err != nil {
			return setupcmd.Provisioned{}, err
		}
		return setupcmd.Provisioned{Email: owner.Email, OrgSlug: owner.TenantSlug, Permissions: len(owner.Permissions)}, nil
	}
}

// Migrations returns the "migrations" directory of an adapter's embedded
// files, rooted at ".".
func Migrations(embedded fs.FS) fs.FS {
	sub, err := fs.Sub(embedded, "migrations")
	if err != nil {
		panic(err) // unreachable: the adapters embed a "migrations" directory
	}
	return sub
}
