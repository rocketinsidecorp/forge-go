package cli

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/rocketinsidecorp/forge-go/internal/version"
)

type commandCatalog struct {
	SchemaVersion    int              `json:"schema_version"`
	Name             string           `json:"name"`
	Version          string           `json:"version"`
	ProcessExitCodes []exitCodeDetail `json:"process_exit_codes"`
	GlobalOptions    []flagDetail     `json:"global_options"`
	Commands         []commandDetail  `json:"commands"`
}

type commandDetail struct {
	Name        string           `json:"name"`
	Usage       string           `json:"usage"`
	Summary     string           `json:"summary"`
	SideEffects []string         `json:"side_effects"`
	Examples    []string         `json:"examples"`
	Environment environmentInfo  `json:"environment"`
	ExitCodes   []exitCodeDetail `json:"exit_codes"`
	Flags       []flagDetail     `json:"flags,omitempty"`
}

type flagDetail struct {
	Name        string `json:"name"`
	Value       string `json:"value,omitempty"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description"`
}

type environmentInfo struct {
	Files     []string `json:"files"`
	Variables []string `json:"variables"`
	Notes     string   `json:"notes,omitempty"`
}

type exitCodeDetail struct {
	Code    int    `json:"code"`
	Meaning string `json:"meaning"`
}

func runHelp(args []string, output io.Writer) error {
	if len(args) == 0 {
		usage(output)
		return nil
	}
	if len(args) != 1 || args[0] != "--json" {
		return errors.New("usage: forge help [--json]")
	}
	return json.NewEncoder(output).Encode(currentCommandCatalog())
}

func currentCommandCatalog() commandCatalog {
	commands := []commandDetail{
		{
			Name: "new", Usage: "forge new NAME [flags]",
			Summary:     "Generate a Dockerized application skeleton.",
			SideEffects: []string{"filesystem_write", "subprocess", "network_optional"},
			Examples:    []string{"forge new inventory", "forge new inventory --database sqlite", "forge new inventory --skip-database --skip-deps"},
			Environment: environmentInfo{Files: []string{}, Variables: []string{}, Notes: "Go dependency-resolution subprocesses inherit the caller environment; Forge runtime variables are not consumed by this command."},
			Flags: []flagDetail{
				{Name: "--module", Value: "PATH", Description: "Go module path; defaults to NAME."},
				{Name: "--dir", Value: "DIR", Description: "Target directory; must be absent or empty."},
				{Name: "--database", Value: "DB", Default: "postgresql", Description: "Database: postgresql, mysql, mariadb, sqlite or none."},
				{Name: "-d", Value: "DB", Default: "postgresql", Description: "Shorthand for --database."},
				{Name: "--skip-database", Description: "Generate without a database; conflicts with an explicit --database."},
				{Name: "--framework-path", Value: "DIR", Description: "Use a local Forge checkout and vendor it."},
				{Name: "--skip-deps", Description: "Do not resolve Go dependencies."},
			},
		},
		{Name: "dev", Usage: "forge dev [NAME]", Summary: "Run the application in development mode.", SideEffects: []string{"subprocess", "application_defined"}, Examples: []string{"forge dev", "forge dev api"}, Environment: runtimeEnvironment()},
		{Name: "test", Usage: "forge test [ARGS]", Summary: "Run Go tests with Forge environment files loaded.", SideEffects: []string{"subprocess", "application_defined"}, Examples: []string{"forge test", "forge test ./... -run TestHealth"}, Environment: testEnvironment()},
		{Name: "build", Usage: "forge build [NAME]", Summary: "Build the application binary under bin/.", SideEffects: []string{"filesystem_write", "subprocess"}, Examples: []string{"forge build", "forge build worker"}, Environment: environmentInfo{Files: []string{}, Variables: []string{}, Notes: "The Go build subprocess inherits the caller environment; Forge .env files are not loaded."}},
		{Name: "migrate", Usage: "forge migrate [status]", Summary: "Apply pending migrations or report migration status.", SideEffects: []string{"database_write", "database_read"}, Examples: []string{"forge migrate", "forge migrate status"}, Environment: runtimeEnvironment()},
		{
			Name: "rollback", Usage: "forge rollback [-steps N]",
			Summary:     "Run down migrations; may destroy application data.",
			SideEffects: []string{"database_destructive"},
			Examples:    []string{"forge rollback", "forge rollback -steps 2"},
			Environment: runtimeEnvironment(),
			Flags:       []flagDetail{{Name: "-steps", Value: "N", Default: "1", Description: "Number of migrations to revert."}},
		},
		{
			Name: "setup", Usage: "forge setup --tenant SLUG --email EMAIL [--organization NAME] [--password-stdin]",
			Summary:     "Create the first organization, tenant and owner account (PostgreSQL identity); refuses once a tenant exists.",
			SideEffects: []string{"database_write", "subprocess", "interactive_input"},
			Examples:    []string{"forge setup --tenant acme --email owner@example.com --organization \"Acme Inc\""},
			Environment: environmentInfo{
				Files:     []string{".env.development", ".env.local"},
				Variables: append(runtimeEnvironment().Variables, "FORGE_SETUP_PASSWORD"),
				Notes:     "Runs the application's setup command with the development environment. The owner password is read from FORGE_SETUP_PASSWORD, from one line of standard input with --password-stdin, or from a terminal prompt without echo; it is never an argument and never printed. It must be at least 12 characters.",
			},
			Flags: []flagDetail{
				{Name: "--tenant", Value: "SLUG", Description: "Tenant slug people type at sign-in: a lowercase letter, then 1-62 lowercase letters, digits or hyphens."},
				{Name: "--email", Value: "EMAIL", Description: "The owner's sign-in email."},
				{Name: "--organization", Value: "NAME", Description: "Organization display name; defaults to the tenant slug."},
				{Name: "--password-stdin", Description: "Read the password from the first line of standard input."},
			},
		},
		{Name: "seed", Usage: "forge seed", Summary: "Load the application's idempotent seed data (db.Seed) in one transaction.", SideEffects: []string{"database_write", "subprocess"}, Examples: []string{"forge seed"}, Environment: runtimeEnvironment()},
		{
			Name: "generate", Usage: "forge generate <migration|view|job|resource|scaffold|authentication> NAME [field:type...] [flags]",
			Summary:     "Generate or preview migrations, auto-escaped HTML views, cancellation-aware job stubs, a complete resource (migration, domain, application, persistence, JSON API and tests), or a scaffold: that resource plus server-rendered HTML CRUD pages (forms, validation errors at 422, flash notices, CSRF), without overwriting files.",
			SideEffects: []string{"filesystem_read", "filesystem_write"},
			Examples: []string{
				"forge generate migration create_widgets --dry-run", "forge generate view welcome", "forge generate job cleanup --dry-run",
				"forge generate resource product name:string 'summary:text?' price_cents:int --tenant-scoped --dry-run",
				"forge generate resource category name:string --global --plural categories",
				"forge generate scaffold post title:string body:text published:bool --global --dry-run",
				"forge generate scaffold ticket subject:string 'details:text?' --tenant-scoped",
				"forge generate authentication --dry-run",
			},
			Environment: environmentInfo{Files: []string{".env.development", ".env.local"}, Variables: []string{"FORGE_DATABASE_URL"}, Notes: "Reads go.mod and relevant generator paths; --dry-run does not create files or directories. `generate resource` reads FORGE_DATABASE_URL (process environment over env files, as forge inspect does) only to choose the database dialect; the value is never printed. Field types: string (up to 255 characters), text, int (64-bit), bool, time (UTC), uuid (UUIDv7); a trailing ? makes a field optional (quote it in shells that expand ?). `generate scaffold` takes the same arguments as `generate resource` and adds app/adapters/htmlui/NAME.go and its tests, app/views/PLURAL/{index,show,new,edit,_form,not_found}.gohtml and, when the application has none, the views base `forge new` generates (app/views/views.go, layouts/application.gohtml, shared/_flash.gohtml); the pages are served at /PLURAL, the JSON API at /v1/PLURAL. `generate view NAME` or DIR/NAME adds app/views/DIR/NAME.gohtml (DIR defaults to pages). `generate authentication` (PostgreSQL applications only; others are refused because Forge persists identities only in PostgreSQL) adds app/adapters/htmlauth and app/views/{sessions,passwords}, the sign-in, sign-out, two-factor and password-reset pages forge new generates, and prints their wiring. Exactly one of --tenant-scoped or --global is required. Resources and scaffolds are never public: the printed wiring requires an application-supplied guard, and nothing edits app/bootstrap/bootstrap.go."},
			Flags: []flagDetail{
				{Name: "--dry-run", Description: "Read-only: print exact planned paths without creating directories or files."},
				{Name: "--tenant-scoped", Description: "resource and scaffold: rows belong to one tenant (tenant_id bound in every statement; PostgreSQL also enables row-level security)."},
				{Name: "--global", Description: "resource and scaffold: rows are shared by all tenants."},
				{Name: "--broadcast", Description: "scaffold only: publish created, updated and deleted rows to open index pages as Turbo Streams over Server-Sent Events (adds app/adapters/htmlstreams, the _item partial and the live-update wiring)."},
				{Name: "--plural", Value: "PLURAL", Description: "resource and scaffold: snake_case plural used for the table and routes (default: built-in English rules)."},
			},
		},
		{Name: "doctor", Usage: "forge doctor [--json]", Summary: "Validate local Forge configuration and show redacted diagnostics.", SideEffects: []string{"environment_read", "filesystem_read"}, Examples: []string{"forge doctor", "forge doctor --json"}, Environment: configEnvironment("Checks configuration values and intentionally omits their contents from JSON output."), Flags: []flagDetail{{Name: "--json", Description: "Print schema-versioned diagnostics with stable remediation codes."}}},
		{Name: "inspect", Usage: "forge inspect --json", Summary: "Read-only inventory of commands, migrations, static routes, modules and database adapter; never executes application code.", SideEffects: []string{"filesystem_read", "environment_read"}, Examples: []string{"forge inspect --json"}, Environment: environmentInfo{Files: []string{".env.development", ".env.local"}, Variables: []string{"FORGE_DATABASE_URL"}, Notes: "Process environment takes precedence over env files; the database URL value is never emitted."}, Flags: []flagDetail{{Name: "--json", Description: "Print the schema-versioned project inventory."}}},
		{Name: "routes", Usage: "forge routes", Summary: "Read-only table of the routes found in app/ and cmd/ (Handle, HandleFunc and Resources calls with literal paths) plus Forge's health routes; never executes application code.", SideEffects: []string{"filesystem_read"}, Examples: []string{"forge routes"}, Environment: environmentInfo{Files: []string{}, Variables: []string{}}},
		{
			Name: "uuid", Usage: "forge uuid [--count N]",
			Summary:     "Generate UUIDv7 identifiers.",
			SideEffects: []string{"random_output"},
			Examples:    []string{"forge uuid", "forge uuid --count 5"},
			Environment: environmentInfo{Files: []string{}, Variables: []string{}},
			Flags:       []flagDetail{{Name: "--count", Value: "N", Default: "1", Description: "Number of identifiers, from 1 to 1000."}},
		},
		{Name: "version", Usage: "forge version", Summary: "Print the Forge version.", SideEffects: []string{"read_only"}, Examples: []string{"forge version"}, Environment: environmentInfo{Files: []string{}, Variables: []string{}}},
		{Name: "help", Usage: "forge help [--json]", Summary: "Show human-readable help or the versioned command catalog.", SideEffects: []string{"read_only"}, Examples: []string{"forge help", "forge help --json"}, Environment: environmentInfo{Files: []string{}, Variables: []string{}}, Flags: []flagDetail{{Name: "--json", Description: "Print the stable machine-readable command catalog."}}},
	}
	for i := range commands {
		if commands[i].Examples == nil {
			commands[i].Examples = []string{}
		}
		if commands[i].Environment.Files == nil {
			commands[i].Environment.Files = []string{}
		}
		if commands[i].Environment.Variables == nil {
			commands[i].Environment.Variables = []string{}
		}
		commands[i].ExitCodes = []exitCodeDetail{
			{Code: 0, Meaning: "The command completed successfully."},
			{Code: 1, Meaning: "The command failed, its arguments or configuration were invalid, or delegated application work failed."},
		}
	}
	return commandCatalog{
		SchemaVersion: 1,
		Name:          "forge",
		Version:       version.Version,
		GlobalOptions: []flagDetail{{Name: jsonErrorFormatFlag, Description: "Emit schema-versioned, secret-safe errors on stderr when a command fails."}},
		ProcessExitCodes: []exitCodeDetail{
			{Code: 0, Meaning: "The requested command completed successfully."},
			{Code: 1, Meaning: "A command failed or its arguments/configuration were invalid."},
			{Code: 2, Meaning: "No command was supplied or the top-level command is unknown."},
		},
		Commands: commands,
	}
}

func runtimeEnvironment() environmentInfo {
	return environmentInfo{
		Files: []string{".env.development", ".env.local"},
		Variables: []string{
			"FORGE_ENV", "FORGE_HTTP_ADDR", "FORGE_HTTP_TRANSPORT", "FORGE_HTTP_TLS_CERT_FILE", "FORGE_HTTP_TLS_KEY_FILE",
			"FORGE_HTTP_READ_HEADER_TIMEOUT", "FORGE_HTTP_READ_TIMEOUT", "FORGE_HTTP_WRITE_TIMEOUT", "FORGE_HTTP_IDLE_TIMEOUT", "FORGE_HTTP_SHUTDOWN_TIMEOUT",
			"FORGE_HTTP_MAX_HEADER_BYTES", "FORGE_HTTP_MAX_BODY_BYTES", "FORGE_DATABASE_URL", "FORGE_DATABASE_MAX_CONNS", "FORGE_DATABASE_MIN_CONNS",
			"FORGE_DATABASE_MAX_CONN_LIFETIME", "FORGE_DATABASE_MAX_CONN_IDLE_TIME", "FORGE_DATABASE_CONNECT_TIMEOUT", "FORGE_DATABASE_STATEMENT_TIMEOUT",
			"FORGE_DATABASE_ALLOW_PLAINTEXT", "FORGE_AUTH_MFA_KEY", "FORGE_AUTH_PASSWORD_RESET_URL",
		},
		Notes: "The process environment overrides .env.local, which overrides .env.development. Values are passed to the generated application.",
	}
}

func testEnvironment() environmentInfo {
	input := runtimeEnvironment()
	input.Variables = append(input.Variables,
		"FORGE_TEST_DATABASE_URL",
		"FORGE_TEST_REQUIRE_DATABASE",
		"FORGE_TEST_POSTGRES_URL",
		"FORGE_TEST_MYSQL_URL",
		"FORGE_TEST_SQLITE_URL",
	)
	input.Notes = "The process environment overrides env files; FORGE_ENV defaults to development if unset. All loaded values reach tests, which may consume additional application-specific variables."
	return input
}

func configEnvironment(notes string) environmentInfo {
	input := runtimeEnvironment()
	input.Notes = notes + " Process environment overrides .env.local, which overrides .env.development."
	return input
}
