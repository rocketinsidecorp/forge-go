package postgres

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/internal/terminal"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// OwnerRole is the role ProvisionOwner creates and assigns. It is defined as
// holding every permission: setup grants every permission registered at that
// moment, and the migrations `forge generate resource` writes grant their new
// permissions to every role with this name.
const OwnerRole = "owner"

// SetupPasswordVariable is the environment variable the setup command reads
// the owner's password from when it is set. Prefer typing it at the prompt
// or piping it with --password-stdin; never pass it as an argument.
const SetupPasswordVariable = "FORGE_SETUP_PASSWORD" // #nosec G101 -- the name of a variable, not a credential

// MinOwnerPasswordRunes is the shortest owner password setup accepts.
const MinOwnerPasswordRunes = 12

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
	// Password is hashed with auth.HashPassword before the transaction
	// starts; it is never stored or logged.
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
	passwordHash, err := auth.HashPassword(account.Password)
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

func validOrganizationName(name string) error {
	if name == "" || utf8.RuneCountInString(name) > 200 || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return errors.New("organization name must be 1 to 200 characters without control characters")
	}
	return nil
}

// SetupCommand returns the "setup" subcommand of a PostgreSQL application
// binary. It creates the first organization, tenant and owner account with
// ProvisionOwner, once:
//
//	app setup --tenant acme --email owner@example.com [--organization "Acme Inc"] [--password-stdin]
//
// The password is read, in order, from the FORGE_SETUP_PASSWORD variable,
// from one line of standard input with --password-stdin, or from an
// interactive terminal prompt (twice, without echo). It is never accepted as
// an argument, printed or logged, and must be at least MinOwnerPasswordRunes
// characters. Running setup again refuses with ErrAlreadyProvisioned.
func SetupCommand() forge.Command {
	return forge.Command{
		Name:    "setup",
		Summary: "create the first tenant and its owner account (refuses once a tenant exists)",
		Run:     runSetup,
	}
}

func runSetup(ctx context.Context, env forge.CommandEnv, args []string) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	tenant := flags.String("tenant", "", "tenant slug users type at sign-in, e.g. acme")
	email := flags.String("email", "", "owner's email")
	organization := flags.String("organization", "", "organization name (default: the tenant slug)")
	passwordStdin := flags.Bool("password-stdin", false, "read the password from the first line of standard input")
	usage := "usage: setup --tenant SLUG --email EMAIL [--organization NAME] [--password-stdin]"
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %v; %s", forge.ErrUsage, err, usage)
	}
	if flags.NArg() != 0 || *tenant == "" || *email == "" {
		return fmt.Errorf("%w: %s", forge.ErrUsage, usage)
	}
	normalizedEmail, normalizedTenant, ok := auth.NormalizeIdentity(*email, *tenant)
	if !ok {
		return fmt.Errorf("%w: --email must be an ASCII email address and --tenant a slug (lowercase letter, then 1-62 lowercase letters, digits or hyphens)", forge.ErrUsage)
	}
	if *organization == "" {
		*organization = normalizedTenant
	}
	if err := validOrganizationName(strings.TrimSpace(*organization)); err != nil {
		return fmt.Errorf("%w: --organization: %v", forge.ErrUsage, err)
	}
	password, err := setupPassword(env, *passwordStdin, normalizedEmail)
	if err != nil {
		return err
	}

	database, err := postgres.Open(ctx, env.Config.Database)
	if err != nil {
		return err
	}
	defer database.Close()
	repository, err := New(database)
	if err != nil {
		return err
	}
	owner, err := repository.ProvisionOwner(ctx, OwnerAccount{
		Organization: *organization,
		TenantSlug:   normalizedTenant,
		Email:        *email,
		Password:     password,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Created organization %q with tenant %q.\n", strings.TrimSpace(*organization), owner.TenantSlug)
	fmt.Fprintf(env.Stdout, "Created owner %s with the %q role (%d permissions).\n", owner.Email, OwnerRole, len(owner.Permissions))
	fmt.Fprintf(env.Stdout, "Sign in at /session/new with tenant %q.\n", owner.TenantSlug)
	return nil
}

// setupPassword returns the owner password from the environment variable,
// piped standard input or an interactive prompt, and checks its length.
func setupPassword(env forge.CommandEnv, fromStdin bool, email string) (string, error) {
	var password []byte
	value, fromEnv := "", false
	if env.LookupEnv != nil {
		value, fromEnv = env.LookupEnv(SetupPasswordVariable)
		fromEnv = fromEnv && value != ""
	}
	switch {
	case fromEnv && fromStdin:
		return "", fmt.Errorf("%w: use either %s or --password-stdin, not both", forge.ErrUsage, SetupPasswordVariable)
	case fromEnv:
		password = []byte(value)
	case fromStdin:
		if env.Stdin == nil {
			return "", errors.New("--password-stdin: no standard input")
		}
		line, err := terminal.ReadLine(env.Stdin)
		if err != nil {
			return "", fmt.Errorf("read the password from standard input: %w", err)
		}
		password = line
	default:
		input, ok := env.Stdin.(*os.File)
		if !ok || !terminal.IsTerminal(input) {
			return "", fmt.Errorf("%w: no password: type it at a terminal, pipe it with --password-stdin, or set %s", forge.ErrUsage, SetupPasswordVariable)
		}
		fmt.Fprintf(env.Stderr, "Password for %s: ", email)
		first, err := terminal.ReadSecret(input)
		fmt.Fprintln(env.Stderr)
		if err != nil {
			return "", fmt.Errorf("read the password: %w", err)
		}
		fmt.Fprint(env.Stderr, "Confirm password: ")
		second, err := terminal.ReadSecret(input)
		fmt.Fprintln(env.Stderr)
		if err != nil {
			return "", fmt.Errorf("read the password: %w", err)
		}
		if !bytes.Equal(first, second) {
			return "", errors.New("the passwords do not match")
		}
		password = first
	}
	if !utf8.Valid(password) || utf8.RuneCount(password) < MinOwnerPasswordRunes {
		return "", fmt.Errorf("the owner password must be valid UTF-8 of at least %d characters", MinOwnerPasswordRunes)
	}
	return string(password), nil
}
