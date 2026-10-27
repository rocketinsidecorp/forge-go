// Package setupcmd is the command line of the "setup" subcommand every Forge
// identity store ships (auth/postgres, auth/sqlite, auth/mysql): its flags,
// how it reads the owner's password and what it prints. The store provisions.
package setupcmd

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

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/internal/terminal"
)

// PasswordVariable is the environment variable setup reads the owner's
// password from when it is set.
const PasswordVariable = "FORGE_SETUP_PASSWORD" // #nosec G101 -- the name of a variable, not a credential

// MinPasswordRunes is the shortest owner password setup accepts.
const MinPasswordRunes = 12

// OwnerRole is the role setup creates and assigns, holding every permission.
const OwnerRole = "owner"

// DefaultOrganizationName names the one organization of a single-tenant
// application, which nobody sees.
const DefaultOrganizationName = "Default"

// Account is what setup provisions: the first organization (its display name
// and the slug users type at sign-in) and its owner.
type Account struct {
	OrganizationName string
	OrgSlug          string
	Email            string
	Password         string
}

// Provisioned is what the store created.
type Provisioned struct {
	Email       string
	OrgSlug     string
	Permissions int
}

// Config is one store's setup command.
type Config struct {
	// Organizations is set for a multi-tenant application (forge new
	// --tenants): setup then requires --org. Otherwise it creates the
	// hidden default organization (auth.DefaultTenantSlug) and refuses
	// --org.
	Organizations bool
	// Provision creates the account, once, in the database env configures.
	Provision func(ctx context.Context, env forge.CommandEnv, account Account) (Provisioned, error)
}

// Command returns the "setup" subcommand.
func Command(config Config) forge.Command {
	summary := "create the owner account (refuses once one exists)"
	if config.Organizations {
		summary = "create the first organization and its owner account (refuses once one exists)"
	}
	return forge.Command{
		Name:    "setup",
		Summary: summary,
		Run: func(ctx context.Context, env forge.CommandEnv, args []string) error {
			return run(ctx, config, env, args)
		},
	}
}

// Usage is the command line setup accepts.
func Usage(organizations bool) string {
	if organizations {
		return "usage: setup --org SLUG --email EMAIL [--org-name NAME] [--password-stdin]"
	}
	return "usage: setup --email EMAIL [--password-stdin]"
}

func run(ctx context.Context, config Config, env forge.CommandEnv, args []string) error {
	if config.Provision == nil {
		return errors.New("setup has no identity store")
	}
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	org := flags.String("org", "", "organization slug users type at sign-in, e.g. acme")
	flags.StringVar(org, "tenant", "", "alias of --org")
	email := flags.String("email", "", "owner's email")
	name := flags.String("org-name", "", "organization name (default: the slug)")
	flags.StringVar(name, "organization", "", "alias of --org-name")
	passwordStdin := flags.Bool("password-stdin", false, "read the password from the first line of standard input")
	usage := Usage(config.Organizations)
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %w; %s", forge.ErrUsage, err, usage)
	}
	if flags.NArg() != 0 || *email == "" {
		return fmt.Errorf("%w: %s", forge.ErrUsage, usage)
	}
	account := Account{Email: *email}
	if config.Organizations {
		if *org == "" {
			return fmt.Errorf("%w: --org is required in an application with organizations; %s", forge.ErrUsage, usage)
		}
		account.OrgSlug, account.OrganizationName = *org, *name
	} else {
		if *org != "" || *name != "" {
			return fmt.Errorf("%w: this application has a single organization (it was created without --tenants), so setup takes no --org: run it with --email only, or convert the application with `forge generate tenancy`", forge.ErrUsage)
		}
		account.OrgSlug, account.OrganizationName = auth.DefaultTenantSlug, DefaultOrganizationName
	}
	normalizedEmail, normalizedOrg, ok := auth.NormalizeIdentity(account.Email, account.OrgSlug)
	if !ok {
		return fmt.Errorf("%w: --email must be an ASCII email address and --org a slug (lowercase letter, then 1-62 lowercase letters, digits or hyphens)", forge.ErrUsage)
	}
	account.OrgSlug = normalizedOrg
	if account.OrganizationName == "" {
		account.OrganizationName = normalizedOrg
	}
	account.OrganizationName = strings.TrimSpace(account.OrganizationName)
	if err := ValidOrganizationName(account.OrganizationName); err != nil {
		return fmt.Errorf("%w: --org-name (--organization): %w", forge.ErrUsage, err)
	}
	password, err := Password(env, *passwordStdin, normalizedEmail)
	if err != nil {
		return err
	}
	account.Password = password
	owner, err := config.Provision(ctx, env, account)
	if err != nil {
		return err
	}
	if config.Organizations {
		fmt.Fprintf(env.Stdout, "Created organization %q with sign-in slug %q.\n", account.OrganizationName, owner.OrgSlug)
	}
	fmt.Fprintf(env.Stdout, "Created owner %s with the %q role (%d permissions).\n", owner.Email, OwnerRole, owner.Permissions)
	if config.Organizations {
		fmt.Fprintf(env.Stdout, "Sign in at /session/new with organization %q.\n", owner.OrgSlug)
	} else {
		fmt.Fprintln(env.Stdout, "Sign in at /session/new.")
	}
	return nil
}

// ValidOrganizationName checks an organization's display name: 1 to 200
// characters without control characters.
func ValidOrganizationName(name string) error {
	if name == "" || utf8.RuneCountInString(name) > 200 || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return errors.New("organization name must be 1 to 200 characters without control characters")
	}
	return nil
}

// Password returns the owner password from the environment variable, piped
// standard input or an interactive prompt (twice, without echo), and checks
// its length. It is never accepted as an argument, printed or logged.
func Password(env forge.CommandEnv, fromStdin bool, email string) (string, error) {
	var password []byte
	value, fromEnv := "", false
	if env.LookupEnv != nil {
		value, fromEnv = env.LookupEnv(PasswordVariable)
		fromEnv = fromEnv && value != ""
	}
	switch {
	case fromEnv && fromStdin:
		return "", fmt.Errorf("%w: use either %s or --password-stdin, not both", forge.ErrUsage, PasswordVariable)
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
			return "", fmt.Errorf("%w: no password: type it at a terminal, pipe it with --password-stdin, or set %s", forge.ErrUsage, PasswordVariable)
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
	if !utf8.Valid(password) || utf8.RuneCount(password) < MinPasswordRunes {
		return "", fmt.Errorf("the owner password must be valid UTF-8 of at least %d characters", MinPasswordRunes)
	}
	return string(password), nil
}
