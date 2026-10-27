// Package mysql provides MySQL and MariaDB persistence for Forge authentication and
// identity (decision D30): the same tables, rules and results as
// auth/postgres, over the forge-go/mysql adapter. It is an adapter over the
// auth core package; auth/authtest is the contract suite it shares with
// auth/postgres.
//
// MySQL and MariaDB have no row-level security. The identity store's own queries are keyed
// by user, membership or token and join the membership's tenant explicitly;
// the generated repositories of an application filter every
// organization-scoped query by the tenant in the request context and refuse
// a query without one (ADR 0017).
package mysql

import (
	"context"
	"io/fs"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/auth/internal/setupcmd"
	"github.com/rocketinsidecorp/forge-go/auth/internal/sqlidentity"
	"github.com/rocketinsidecorp/forge-go/command"
	"github.com/rocketinsidecorp/forge-go/config"
	forgemysql "github.com/rocketinsidecorp/forge-go/mysql"
	"github.com/rocketinsidecorp/forge-go/sqldb"
)

// Repository is the identity store: password identities, sessions, password
// recovery, MFA factors and challenges, the security audit trail and setup.
// It implements the same auth ports as auth/postgres.Repository.
type Repository = sqlidentity.Repository

// LoginThrottler is an auth.LoginThrottler whose counters live in the
// database, so every process of an application shares them.
type LoginThrottler = sqlidentity.LoginThrottler

// Option configures a Repository.
type Option = sqlidentity.Option

// OwnerAccount is the first organization and its owner (ProvisionOwner).
type OwnerAccount = sqlidentity.OwnerAccount

// ProvisionedOwner describes what ProvisionOwner created.
type ProvisionedOwner = sqlidentity.ProvisionedOwner

// The errors and limits of auth/postgres, with the same meaning.
var (
	ErrMembershipRequired       = sqlidentity.ErrMembershipRequired
	ErrExpiryRequired           = sqlidentity.ErrExpiryRequired
	ErrRecoveryIdentityRequired = sqlidentity.ErrRecoveryIdentityRequired
	ErrMFAIdentityRequired      = sqlidentity.ErrMFAIdentityRequired
	ErrAlreadyProvisioned       = sqlidentity.ErrAlreadyProvisioned
	ErrOwnerEmailTaken          = sqlidentity.ErrOwnerEmailTaken
)

const (
	// DefaultSessionIdleTimeout is how long a session may go unused by
	// default (WithSessionIdleTimeout).
	DefaultSessionIdleTimeout = sqlidentity.DefaultSessionIdleTimeout
	// OwnerRole is the role setup creates, holding every permission.
	OwnerRole = setupcmd.OwnerRole
	// SetupPasswordVariable is the variable setup reads the password from.
	SetupPasswordVariable = setupcmd.PasswordVariable
	// MinOwnerPasswordRunes is the shortest owner password setup accepts.
	MinOwnerPasswordRunes = setupcmd.MinPasswordRunes
)

// WithSessionIdleTimeout ends a session nobody used for timeout (between
// five minutes and 30 days; DefaultSessionIdleTimeout without it). Pass
// config.Auth.SessionIdleTimeout (FORGE_AUTH_SESSION_IDLE_TIMEOUT).
func WithSessionIdleTimeout(timeout time.Duration) Option {
	return sqlidentity.WithSessionIdleTimeout(timeout)
}

// New returns the identity repository on db, which forge-go/mysql.Open
// returns. The tables come from Migrations.
func New(db *sqldb.DB, options ...Option) (*Repository, error) {
	return sqlidentity.New(db, dialect, options...)
}

// NewLoginThrottler returns a LoginThrottler for config, which must be valid.
func NewLoginThrottler(db *sqldb.DB, config auth.LoginThrottleConfig) (*LoginThrottler, error) {
	return sqlidentity.NewLoginThrottler(db, dialect, config)
}

// NewRecoveryThrottler returns a LoginThrottler with
// auth.DefaultRecoveryThrottleConfig.
func NewRecoveryThrottler(db *sqldb.DB) (*LoginThrottler, error) {
	return NewLoginThrottler(db, auth.DefaultRecoveryThrottleConfig())
}

// SetupOption configures SetupCommand.
type SetupOption func(*setupcmd.Config)

// SetupOrganizations says whether the application has organizations
// (`forge new --tenants`, or converted by `forge generate tenancy`): setup
// then requires --org. Without them (`forge new --auth`) setup takes --email
// only and creates the one hidden organization (auth.DefaultTenantSlug).
// Without this option setup requires --org.
func SetupOrganizations(enabled bool) SetupOption {
	return func(config *setupcmd.Config) { config.Organizations = enabled }
}

// SetupCommand returns the "setup" subcommand: it creates the first
// organization and owner account with ProvisionOwner, once:
//
//	app setup --email owner@example.com [--password-stdin]             # SetupOrganizations(false)
//	app setup --org acme --email owner@example.com [--org-name "Acme"]  # with organizations
//
// The password comes from FORGE_SETUP_PASSWORD, one line of standard input
// (--password-stdin) or a terminal prompt, never an argument.
func SetupCommand(options ...SetupOption) command.Command {
	config := setupcmd.Config{Organizations: true, Provision: sqlidentity.Provision(open, dialect)}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	return setupcmd.Command(config)
}

// Migrations returns the identity migrations rooted at "." so they can be
// passed to forge-go/mysql.CommandsFromSets or mysqltest.NewMigrated.
func Migrations() fs.FS {
	return sqlidentity.Migrations(migrations)
}

// open opens the database setup provisions.
func open(ctx context.Context, cfg config.Database) (*sqldb.DB, error) {
	return forgemysql.Open(ctx, cfg)
}
