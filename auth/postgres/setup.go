package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/auth/internal/setupcmd"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// OwnerRole is the role ProvisionOwner creates and assigns. It is defined as
// holding every permission: setup grants every permission registered at that
// moment, and the migrations `forge generate resource` writes grant their new
// permissions to every role with this name.
const OwnerRole = setupcmd.OwnerRole

// SetupPasswordVariable is the environment variable the setup command reads
// the owner's password from when it is set. Prefer typing it at the prompt
// or piping it with --password-stdin; never pass it as an argument.
const SetupPasswordVariable = setupcmd.PasswordVariable

// MinOwnerPasswordRunes is the shortest owner password setup accepts.
const MinOwnerPasswordRunes = setupcmd.MinPasswordRunes

var (
	// ErrAlreadyProvisioned refuses setup on a database that already has a
	// tenant: setup creates the first tenant and owner only, so it can never
	// add an account to, or take over, a running installation.
	ErrAlreadyProvisioned = errors.New("identity is already set up: a tenant exists, and setup only provisions an empty identity database")
	// ErrOwnerEmailTaken means a user with the owner's email already exists.
	ErrOwnerEmailTaken = errors.New("a user with this email already exists")

	setupLockKey = postgres.LockKey("forge:identity_setup")
)

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
// It holds an advisory lock and returns ErrAlreadyProvisioned when any tenant
// exists, so concurrent or repeated runs create nothing. Identifiers are
// UUIDv7 values generated in Go.
func (r *Repository) ProvisionOwner(ctx context.Context, account OwnerAccount) (ProvisionedOwner, error) {
	email, tenant, ok := auth.NormalizeIdentity(account.Email, account.TenantSlug)
	if !ok {
		return ProvisionedOwner{}, errors.New("owner email or tenant slug is invalid (email: ASCII only; slug: lowercase letter, then 1-62 lowercase letters, digits or hyphens)")
	}
	organization := strings.TrimSpace(account.Organization)
	if err := validOrganizationName(organization); err != nil {
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
	err = r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := postgres.AdvisoryXactLock(ctx, tx, setupLockKey); err != nil {
			return err
		}
		var provisioned bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM forge_tenants)`).Scan(&provisioned); err != nil {
			return postgres.Translate(err)
		}
		if provisioned {
			return ErrAlreadyProvisioned
		}
		statements := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO forge_organizations (id, name) VALUES ($1, $2)`, []any{owner.OrganizationID, organization}},
			{`INSERT INTO forge_tenants (id, organization_id, slug) VALUES ($1, $2, $3)`, []any{owner.TenantID, owner.OrganizationID, tenant}},
			{`INSERT INTO forge_users (id, email, email_normalized, password_hash) VALUES ($1, $2, $3, $4)`, []any{owner.UserID, owner.Email, email, passwordHash}},
			{`INSERT INTO forge_memberships (id, user_id, tenant_id) VALUES ($1, $2, $3)`, []any{owner.MembershipID, owner.UserID, owner.TenantID}},
			{`INSERT INTO forge_roles (id, tenant_id, name) VALUES ($1, $2, $3)`, []any{owner.RoleID, owner.TenantID, OwnerRole}},
			{`INSERT INTO forge_membership_roles (membership_id, role_id, tenant_id) VALUES ($1, $2, $3)`, []any{owner.MembershipID, owner.RoleID, owner.TenantID}},
		}
		for _, statement := range statements {
			if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
				if postgres.ConstraintName(err) == "forge_users_email_normalized_key" {
					return ErrOwnerEmailTaken
				}
				return postgres.Translate(err)
			}
		}
		rows, err := tx.Query(ctx, `
			INSERT INTO forge_role_permissions (role_id, permission)
			SELECT $1, name FROM forge_permissions
			RETURNING permission
		`, owner.RoleID)
		if err != nil {
			return postgres.Translate(err)
		}
		permissions, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return postgres.Translate(err)
		}
		owner.Permissions = permissions
		return recordSecurityEvent(ctx, tx, auth.SecurityEvent{
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

func validOrganizationName(name string) error { return setupcmd.ValidOrganizationName(name) }

// SetupOption configures SetupCommand.
type SetupOption func(*setupcmd.Config)

// SetupOrganizations says whether the application has organizations
// (`forge new --tenants`, or converted by `forge generate tenancy`): setup
// then requires --org. Without them (`forge new --auth`) setup takes --email
// only and creates the one hidden organization (auth.DefaultTenantSlug) the
// sign-in page signs everyone in to. Without this option setup requires
// --org, as before single-tenant applications existed.
func SetupOrganizations(enabled bool) SetupOption {
	return func(config *setupcmd.Config) { config.Organizations = enabled }
}

// SetupCommand returns the "setup" subcommand of a PostgreSQL application
// binary. It creates the first organization and owner account with
// ProvisionOwner, once:
//
//	app setup --org acme --email owner@example.com [--org-name "Acme Inc"] [--password-stdin]
//	app setup --email owner@example.com [--password-stdin]   # SetupOrganizations(false)
//
// --tenant and --organization are accepted as aliases of --org and
// --org-name. The password is read, in order, from the FORGE_SETUP_PASSWORD
// variable, from one line of standard input with --password-stdin, or from
// an interactive terminal prompt (twice, without echo). It is never accepted
// as an argument, printed or logged, and must be at least
// MinOwnerPasswordRunes characters. Running setup again refuses with
// ErrAlreadyProvisioned.
func SetupCommand(options ...SetupOption) forge.Command {
	config := setupcmd.Config{Organizations: true, Provision: provision}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	return setupcmd.Command(config)
}

func provision(ctx context.Context, env forge.CommandEnv, account setupcmd.Account) (setupcmd.Provisioned, error) {
	database, err := postgres.Open(ctx, env.Config.Database)
	if err != nil {
		return setupcmd.Provisioned{}, err
	}
	defer database.Close()
	repository, err := New(database)
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
