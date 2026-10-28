// Package sqlite provides SQLite persistence for Forge authentication and
// identity (decision D30): the same tables, rules and results as
// auth/postgres, over the forge-go/sqlite adapter. It is an adapter over the
// auth core package; auth/authtest is the contract suite it shares with
// auth/postgres.
//
// SQLite has no row-level security. The identity store's own queries are keyed
// by user, membership or token and join the membership's tenant explicitly;
// the generated repositories of an application filter every
// organization-scoped query by the tenant in the request context and refuse
// a query without one (ADR 0017).
package sqlite

import (
	"context"
	"io/fs"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/auth/authsetup"
	"github.com/rocketinsidecorp/forge-go/auth/authsql"
	"github.com/rocketinsidecorp/forge-go/command"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/sqldb"
	forgesqlite "github.com/rocketinsidecorp/forge-go/sqlite"
)

// Repository is the identity store: password identities, sessions, password
// recovery, MFA factors and challenges, the security audit trail and setup.
// It implements the same auth ports as auth/postgres.Repository.
type Repository = authsql.Repository

// LoginThrottler is an auth.LoginThrottler whose counters live in the
// database, so every process of an application shares them.
type LoginThrottler = authsql.LoginThrottler

// Option configures a Repository.
type Option = authsql.Option

// OwnerAccount is the first organization and its owner (ProvisionOwner).
type OwnerAccount = authsql.OwnerAccount

// ProvisionedOwner describes what ProvisionOwner created.
type ProvisionedOwner = authsql.ProvisionedOwner

// The errors and limits of auth/postgres, with the same meaning.
var (
	ErrMembershipRequired       = authsql.ErrMembershipRequired
	ErrExpiryRequired           = authsql.ErrExpiryRequired
	ErrRecoveryIdentityRequired = authsql.ErrRecoveryIdentityRequired
	ErrMFAIdentityRequired      = authsql.ErrMFAIdentityRequired
	ErrAlreadyProvisioned       = authsql.ErrAlreadyProvisioned
	ErrOwnerEmailTaken          = authsql.ErrOwnerEmailTaken
)

const (
	// DefaultSessionIdleTimeout is how long a session may go unused by
	// default (WithSessionIdleTimeout).
	DefaultSessionIdleTimeout = authsql.DefaultSessionIdleTimeout
	// OwnerRole is the role setup creates, holding every permission.
	OwnerRole = authsetup.OwnerRole
	// SetupPasswordVariable is the variable setup reads the password from.
	SetupPasswordVariable = authsetup.PasswordVariable
	// MinOwnerPasswordRunes is the shortest owner password setup accepts.
	MinOwnerPasswordRunes = authsetup.MinPasswordRunes
)

// WithSessionIdleTimeout ends a session nobody used for timeout (between
// five minutes and 30 days; DefaultSessionIdleTimeout without it). Pass
// config.Auth.SessionIdleTimeout (FORGE_AUTH_SESSION_IDLE_TIMEOUT).
func WithSessionIdleTimeout(timeout time.Duration) Option {
	return authsql.WithSessionIdleTimeout(timeout)
}

// New returns the identity repository on db, which forge-go/sqlite.Open
// returns. The tables come from Migrations.
func New(db *sqldb.DB, options ...Option) (*Repository, error) {
	return authsql.New(db, dialect, options...)
}

// NewLoginThrottler returns a LoginThrottler for config, which must be valid.
func NewLoginThrottler(db *sqldb.DB, config auth.LoginThrottleConfig) (*LoginThrottler, error) {
	return authsql.NewLoginThrottler(db, dialect, config)
}

// NewRecoveryThrottler returns a LoginThrottler with
// auth.DefaultRecoveryThrottleConfig.
func NewRecoveryThrottler(db *sqldb.DB) (*LoginThrottler, error) {
	return NewLoginThrottler(db, auth.DefaultRecoveryThrottleConfig())
}

// SetupOption configures SetupCommand.
type SetupOption func(*authsetup.Config)

// SetupOrganizations says whether the application has organizations
// (`forge new --tenants`, or converted by `forge generate tenancy`): setup
// then requires --org. Without them (`forge new --auth`) setup takes --email
// only and creates the one hidden organization (auth.DefaultTenantSlug).
// Without this option setup requires --org.
func SetupOrganizations(enabled bool) SetupOption {
	return func(config *authsetup.Config) { config.Organizations = enabled }
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
	config := authsetup.Config{Organizations: true, Provision: authsql.Provision(open, dialect)}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	return authsetup.Command(config)
}

// Migrations returns the identity migrations rooted at "." so they can be
// passed to forge-go/sqlite.CommandsFromSets or sqlitetest.NewMigrated.
func Migrations() fs.FS {
	return authsql.Migrations(migrations)
}

// open opens the database setup provisions.
func open(ctx context.Context, cfg config.Database) (*sqldb.DB, error) {
	return forgesqlite.Open(ctx, cfg)
}
