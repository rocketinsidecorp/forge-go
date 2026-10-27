package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

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
	Aliases     []string         `json:"aliases,omitempty"`
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
	if len(args) == 1 && args[0] == "--json" {
		return json.NewEncoder(output).Encode(currentCommandCatalog())
	}
	// forge help COMMAND --help is the help of COMMAND, like forge COMMAND --help.
	if len(args) == 2 && isHelpFlag(args[1]) && !strings.HasPrefix(args[0], "-") {
		args = args[:1]
	}
	if len(args) == 1 && !strings.HasPrefix(args[0], "-") {
		return writeCommandHelp(output, args[0])
	}
	return usageError{helpUsage}
}

// usageError is an invalid command line: exit status 2, like an unknown
// top-level command.
type usageError struct{ message string }

func (e usageError) Error() string { return e.message }

// ExitCode is the process exit status.
func (usageError) ExitCode() int { return 2 }

// parseFlags is flags.Parse that reports an invalid flag (unknown, or a bad
// value) as a usageError, which exits 2 like Forge-Rust and the other usage
// errors. A request for help (flag.ErrHelp) is returned unchanged: the caller
// answers it with exit status 0.
func parseFlags(flags *flag.FlagSet, args []string) error {
	err := flags.Parse(args)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		return usageError{err.Error()}
	}
	return err
}

func isHelpFlag(arg string) bool { return arg == "-h" || arg == "--help" }

const helpUsage = "usage: forge help [--json | COMMAND]"

// helpArgumentLimit is how many leading arguments, command name included, a
// command that forwards the rest of its line to another program may have
// before -h and --help belong to that program: `forge test -h` is Forge's help
// but `forge test ./... -h` is go test's, and `forge app exec CMD --help` runs
// CMD --help on the host.
var helpArgumentLimit = map[string]int{"test": 2, "app": 3}

// commandHelpRequested reports whether args (a command name and its
// arguments) ask for that command's help: -h or --help before a literal "--".
func commandHelpRequested(args []string) bool {
	if len(args) < 2 || commandDetailNamed(args[0]) == nil {
		return false
	}
	if args[0] == "help" {
		// forge help --help describes help; forge help COMMAND --help is
		// answered by runHelp.
		return len(args) == 2 && isHelpFlag(args[1])
	}
	limit, bounded := helpArgumentLimit[args[0]]
	for i, arg := range args[1:] {
		if arg == "--" || (bounded && i+1 >= limit) {
			return false
		}
		if isHelpFlag(arg) {
			return true
		}
	}
	return false
}

func commandDetailNamed(name string) *commandDetail {
	if canonical, ok := commandAliases[name]; ok {
		name = canonical
	}
	catalog := currentCommandCatalog()
	for i := range catalog.Commands {
		if catalog.Commands[i].Name == name {
			return &catalog.Commands[i]
		}
	}
	return nil
}

// writeCommandHelp prints one command's usage, summary, flags, examples and
// notes, from the same catalog `forge help --json` serves.
func writeCommandHelp(output io.Writer, name string) error {
	command := commandDetailNamed(name)
	if command == nil {
		return usageError{fmt.Sprintf("unknown command %q: run `forge help` to list the commands", name)}
	}
	var text strings.Builder
	fmt.Fprintf(&text, "usage: %s\n", command.Usage)
	for _, alias := range command.Aliases {
		fmt.Fprintf(&text, "alias: forge %s (forge %s %s)\n", alias, alias, strings.TrimPrefix(command.Usage, "forge "+command.Name+" "))
	}
	fmt.Fprintf(&text, "\n%s\n", command.Summary)
	if len(command.Flags) > 0 {
		text.WriteString("\nflags:\n")
		for _, f := range command.Flags {
			label := f.Name
			if f.Value != "" {
				label += " " + f.Value
			}
			fmt.Fprintf(&text, "  %s\n", label)
			description := f.Description
			if f.Default != "" {
				description += " (default: " + f.Default + ")"
			}
			fmt.Fprintf(&text, "      %s\n", description)
		}
	}
	if len(command.Examples) > 0 {
		text.WriteString("\nexamples:\n")
		for _, example := range command.Examples {
			fmt.Fprintf(&text, "  %s\n", example)
		}
	}
	if command.Environment.Notes != "" {
		fmt.Fprintf(&text, "\nnotes:\n  %s\n", command.Environment.Notes)
	}
	if len(command.Environment.Files) > 0 {
		fmt.Fprintf(&text, "\nreads: %s\n", strings.Join(command.Environment.Files, ", "))
	}
	if len(command.Environment.Variables) > 0 {
		fmt.Fprintf(&text, "\nenvironment: %s\n", strings.Join(command.Environment.Variables, ", "))
	}
	_, err := io.WriteString(output, text.String())
	return err
}

func currentCommandCatalog() commandCatalog {
	commands := []commandDetail{
		{
			Name: "new", Usage: "forge new NAME [flags]",
			Summary:     "Generate a Dockerized full-stack application skeleton (server-rendered views; --api for JSON only).",
			SideEffects: []string{"filesystem_write", "subprocess", "network_optional"},
			Examples:    []string{"forge new inventory", "forge new inventory --auth", "forge new inventory --tenants", "forge new inventory --auth -d mysql", "forge new inventory --locale pt-BR", "forge new inventory --api-auth", "forge new inventory --js", "forge new inventory --api", "forge new inventory --api -d postgresql", "forge new inventory --skip-database --skip-deps"},
			Environment: environmentInfo{Files: []string{}, Variables: []string{}, Notes: "Go dependency-resolution subprocesses inherit the caller environment; Forge runtime variables are not consumed by this command."},
			Flags: []flagDetail{
				{Name: "--module", Value: "PATH", Description: "Go module path; defaults to NAME."},
				{Name: "--dir", Value: "DIR", Description: "Target directory; must be absent or empty."},
				{Name: "--database", Value: "DB", Default: "sqlite", Description: "Database: sqlite, postgresql, mysql, mariadb or none. The default is SQLite, or PostgreSQL with --tenants (decision D35)."},
				{Name: "-d", Value: "DB", Default: "sqlite", Description: "Shorthand for --database."},
				{Name: "--auth", Description: "Generate Forge identity (decision D31): sign-in, sign-out, two-factor and password-reset pages, the identity migrations and `forge setup --email`, on any database. The application has one hidden organization: generated tables are plain tables. Without it there is no authentication; `forge generate authentication` adds it later."},
				{Name: "--tenants", Description: "Generate a multi-tenant application (decision D33): users belong to organizations, which sign-in and `forge setup --org` name; generated tables take --org-scoped (an org_id column every query is filtered by, behind row-level security on PostgreSQL) or --global. Implies --auth, and PostgreSQL unless --database is given."},
				{Name: "--skip-database", Description: "Generate without a database; conflicts with an explicit --database."},
				{Name: "--locale", Value: "LOCALE", Default: "en", Description: "Default locale of the generated pages: en, es or pt-BR. config/locales/en.json is always generated, plus LOCALE.json."},
				{Name: "--api", Description: "Generate an API-only application, like rails new --api: JSON handlers (GET / names the service, GET /v1/status, the health routes and the OpenAPI document) without app/views, app/assets, app/javascript, config/locales, sign-in pages or browser middleware (CSRF, method override, flash). Conflicts with --api-auth, --auth, --tenants, --locale and --js. In such an application `forge generate view`, `scaffold`, `authentication` and an HTML `controller` refuse; `resource`, `model`, `controller --api`, `mailer`, `job` and `migration` work."},
				{Name: "--js", Description: "Keep the front end in plain JavaScript (app/javascript/application.js and controllers/*.js, served as written) instead of the default TypeScript (app/javascript/**/*.ts, tsconfig.json and declarations for Turbo and Stimulus, compiled by forge with esbuild into app/javascript/dist; no Node needed). Conflicts with --api."},
				{Name: "--api-auth", Description: "Also mount the JSON authentication APIs (POST /auth/login, /auth/logout, /auth/cookie/login, /auth/cookie/logout, MFA completion, /auth/recovery/request, /auth/recovery/reset, GET /v1/auth/session). Without it the HTML sign-in pages are the only way to sign in. Implies --auth."},
				{Name: "--forge-path", Value: "DIR", Description: "Use a local Forge checkout and vendor it."},
				{Name: "--framework-path", Value: "DIR", Description: "Deprecated alias of --forge-path; prints a deprecation line and will be removed in the next minor version."},
				{Name: "--skip-deps", Description: "Do not resolve Go dependencies."},
				{Name: "--skip-lockfile", Description: "Alias of --skip-deps (Forge-Rust's name: go.sum is the lockfile)."},
			},
		},
		devCommandDetail(),
		{Name: "test", Usage: "forge test [ARGS]", Summary: "Run Go tests with Forge environment files loaded.", SideEffects: []string{"subprocess", "application_defined"}, Examples: []string{"forge test", "forge test ./... -run TestHealth"}, Environment: testEnvironment()},
		{
			Name: "check", Usage: "forge check",
			Summary:     "Type-check the application: go build ./... and go vet ./..., and tsc --noEmit for a TypeScript front end when tsc is on PATH (the counterpart of Forge-Rust's cargo check --all-targets). The TypeScript is compiled first, so go build embeds what the sources say.",
			SideEffects: []string{"subprocess", "filesystem_write"},
			Examples:    []string{"forge check"},
			Environment: environmentInfo{Files: []string{}, Variables: []string{}, Notes: "Runs the go tool (and tsc, when found) in the current directory without a shell; they inherit the caller environment and write no binary. A TypeScript front end is compiled into app/javascript/dist first (files are written only when their content changes). Without tsc on PATH the TypeScript is not type-checked and one note says how to install it (npm install --global --ignore-scripts typescript@6.0.3). Forge .env files are not loaded."},
		},
		{
			Name: "format", Usage: "forge format [--check]",
			Summary:     "Format the application's Go files with gofmt -l -w (vendor, testdata and nested modules are skipped); the files it rewrites are listed. With --check, list the files that need formatting, write nothing and exit 1 when there are any.",
			SideEffects: []string{"filesystem_write", "subprocess"},
			Examples:    []string{"forge format", "forge format --check"},
			Environment: environmentInfo{Files: []string{}, Variables: []string{}, Notes: "Runs the gofmt of the Go toolchain found on PATH, without a shell."},
			Flags:       []flagDetail{{Name: "--check", Description: "List the files that need formatting and exit 1 if there are any, without writing (the form CI runs)."}},
		},
		{
			Name: "lint", Usage: "forge lint",
			Summary:     "Run golangci-lint run ./... (with the application's .golangci.yml when it has one); when golangci-lint is not on PATH the command fails and names how to install it.",
			SideEffects: []string{"subprocess"},
			Examples:    []string{"forge lint"},
			Environment: environmentInfo{Files: []string{".golangci.yml"}, Variables: []string{}, Notes: "golangci-lint is not part of the Go toolchain. Install it with `" + golangciLintInstall + "`. The counterpart of Forge-Rust's cargo clippy."},
		},
		{Name: "build", Usage: "forge build [NAME]", Summary: "Build the application binary under bin/, compiling a TypeScript front end into app/javascript/dist first.", SideEffects: []string{"filesystem_write", "subprocess"}, Examples: []string{"forge build", "forge build worker"}, Environment: environmentInfo{Files: []string{}, Variables: []string{}, Notes: "The Go build subprocess inherits the caller environment; Forge .env files are not loaded."}},
		dbCommandDetail(),
		{Name: "migrate", Usage: "forge migrate [status]", Summary: "Apply pending migrations or report migration status.", SideEffects: []string{"database_write", "database_read"}, Examples: []string{"forge migrate", "forge migrate status"}, Environment: migrationEnvironment()},
		{
			Name: "rollback", Usage: "forge rollback [-steps N]",
			Summary:     "Run down migrations of the application (never Forge framework migrations); may destroy application data.",
			SideEffects: []string{"database_destructive"},
			Examples:    []string{"forge rollback", "forge rollback -steps 2"},
			Environment: migrationEnvironment(),
			Flags:       []flagDetail{{Name: "-steps", Value: "N", Default: "1", Description: "Number of application migrations to revert."}},
		},
		{
			Name: "setup", Usage: "forge setup [--org SLUG] --email EMAIL [--org-name NAME] [--password-stdin]",
			Summary:     "Create the owner account (with --tenants, the first organization too); refuses once one exists. A single-organization application (forge new --auth) takes --email only; one with organizations (--tenants) needs --org.",
			SideEffects: []string{"database_write", "subprocess", "interactive_input"},
			Examples:    []string{"forge setup --email owner@example.com", "forge setup --org acme --email owner@example.com --org-name \"Acme Inc\""},
			Environment: environmentInfo{
				Files:     []string{".env.development", ".env.local"},
				Variables: append(runtimeEnvironment().Variables, "FORGE_SETUP_PASSWORD"),
				Notes:     "Runs the application's setup command with the development environment. The owner password is read from FORGE_SETUP_PASSWORD, from one line of standard input with --password-stdin, or from a terminal prompt without echo; it is never an argument and never printed. It must be at least 12 characters.",
			},
			Flags: []flagDetail{
				{Name: "--org", Value: "SLUG", Description: "Applications with organizations only: the slug people type at sign-in, a lowercase letter, then 1-62 lowercase letters, digits or hyphens. --tenant is an alias."},
				{Name: "--email", Value: "EMAIL", Description: "The owner's sign-in email."},
				{Name: "--org-name", Value: "NAME", Description: "Organization display name; defaults to the slug. --organization is an alias."},
				{Name: "--password-stdin", Description: "Read the password from the first line of standard input."},
			},
		},
		{Name: "seed", Usage: "forge seed", Summary: "Load the application's idempotent seed data (db.Seed) in one transaction.", SideEffects: []string{"database_write", "subprocess"}, Examples: []string{"forge seed"}, Environment: runtimeEnvironment()},
		{
			Name: "generate", Aliases: []string{"g"}, Usage: "forge generate <migration|view|job|model|controller|mailer|resource|scaffold|join|authentication|tenancy> NAME [arguments...] [flags]",
			Summary:     "Generate or preview migrations, auto-escaped HTML views, cancellation-aware job stubs, typed queue jobs, models (migration, domain, application and persistence layers), controllers (handlers, routes and views without persistence), mailers (typed methods, text and HTML templates, previews), a complete resource (migration, domain, application, persistence, JSON API and tests), or a scaffold: that resource plus server-rendered HTML CRUD pages (forms, validation errors at 422, flash notices, CSRF), without overwriting files.",
			SideEffects: []string{"filesystem_read", "filesystem_write"},
			Examples: []string{
				"forge generate migration create_widgets --dry-run", "forge g migration add_sku_to_products sku:string stock:int",
				"forge g scaffold product name:string price:integer", "forge generate view welcome", "forge generate job cleanup --dry-run",
				"forge generate resource product name:string 'summary:text?' price_cents:int --org-scoped --dry-run",
				"forge generate resource category name:string --global --plural categories",
				"forge generate scaffold post title:string body:text published:bool --global --dry-run",
				"forge generate scaffold ticket subject:string 'details:text?' --org-scoped",
				"forge generate authentication --dry-run",
				"forge generate authentication --api-auth",
				"forge generate tenancy --org acme",
				"forge generate model post title:string 'body:text?' --org-scoped",
				"forge generate controller pages home about_us --public",
				"forge generate controller reports monthly --api",
				"forge generate mailer user welcome:name,url password_reset:name,reset_url",
				"forge generate job send_welcome user_id:uuid --queue",
				"forge generate scaffold comment body:text post:references",
				"forge generate scaffold note body:text 'post:references?' --on-delete=cascade",
				"forge generate join post tag",
			},
			Environment: environmentInfo{Files: []string{".env.development", ".env.local"}, Variables: []string{"FORGE_DATABASE_URL"}, Notes: "Reads go.mod and relevant generator paths; --dry-run does not create files or directories. `generate resource` reads FORGE_DATABASE_URL (process environment over env files, as forge inspect does) only to choose the database dialect; the value is never printed. Field types: string (up to 255 characters), text, integer (64-bit), boolean, timestamp (UTC), uuid (UUIDv7) and references; int, bool, time, datetime and belongs_to are aliases; a trailing ? makes a field optional (quote it in shells that expand ?). `post:references` (model, resource and scaffold) adds a post_id column with its index and a foreign key to posts, composite (org_id, post_id) with organizations; deleting a referenced post is refused while rows reference it (409 post_in_use, or a flash message) unless --on-delete=cascade; a scaffold also gets a parent select, /posts/{id}/comments pages and JSON, and Turbo Frames on the post page. `generate join LEFT RIGHT` (generate join post tag) adds the many-to-many join table post_tags (a primary key over the pair, both foreign keys ON DELETE CASCADE), its layers and JSON API and, when LEFT is a scaffold, the page on which a post links and unlinks tags; both sides must be resources or scaffolds generated first, and the reversed pair is refused. `generate scaffold` takes the same arguments as `generate resource` and adds app/adapters/htmlui/NAME.go and its tests, app/views/PLURAL/{index,show,new,edit,_form,not_found}.gohtml and, when the application has none, the views base `forge new` generates (app/views/views.go, layouts/application.gohtml, shared/_flash.gohtml); the pages are served at /PLURAL, the JSON API at /v1/PLURAL (with Forge identity it authenticates the bearer tokens of --api-auth, or else the session cookie plus the X-CSRF-Token header on changes). `generate view NAME` or DIR/NAME adds app/views/DIR/NAME.gohtml (DIR defaults to pages). `generate authentication` adds what `forge new --auth` generates to an application created without it, on any database: app/bootstrap/identity.go (the identity store, the sign-in, two-factor and password-reset services, wired by the routes seam), app/adapters/htmlauth, app/views/{sessions,passwords,mfa} and their translations; it rewrites app/bootstrap/app_gen.go (identity migrations and the setup command) and the seam, and appends an MFA key to .env.local, so nothing is edited by hand; with --api-auth in an application that has identity already it writes nothing and prints the JSON authentication APIs to mount in identity.go. `generate tenancy [--org SLUG]` converts a single-organization --auth application to organizations: a migration adds org_id to every generated table (filled with the existing organization, required, indexed, behind row-level security on PostgreSQL; its down refuses while another organization has rows), the generated code of those tables is rendered again --org-scoped (refused for a file edited by hand), app_gen.go says Organizations = true, and --org renames the existing organization's sign-in slug from default. `generate model` takes the arguments of `generate resource` and creates the migration, domain, application and persistence layers with their tests, without the HTTP layer. `generate controller NAME ACTION...` adds app/adapters/htmlui/NAME_controller.go (or app/adapters/httpapi with --api), its test, one view per action in app/views/NAME (HTML only) and, for a guarded controller in an application with Forge identity, a migration that registers the NAME:read permission; actions are read-only GET routes. `generate mailer NAME METHOD[:field,field]...` adds app/mailers/NAME_mailer.go with typed methods, its preview and tests, and app/mailers/templates/NAME/METHOD.{subject.tmpl,text.tmpl,html.gohtml}. `generate job NAME [field:type...] --queue` adds a typed handler for the persistent job queue and its test (app/jobs/NAME.go); without --queue it generates the recurring job stub. In an application with organizations (forge new --tenants) exactly one of --org-scoped or --global is required for models, resources and scaffolds; a single-organization application refuses --org-scoped and needs neither. `generate migration NAME [field:type...]` writes db/migrations/<version>_NAME.{up,down}.sql: add_COLUMNS_to_TABLE with fields adds them with ALTER TABLE ... ADD COLUMN (down drops them), remove_COLUMNS_from_TABLE does the reverse, in the application's SQL dialect; required columns get the type's zero value as default for existing rows (a required time or uuid column is refused: make it optional); any other name, or no fields, writes blank files and a warning to fill them in, since an empty migration is refused. `forge g` is short for `forge generate`. Resources, scaffolds and controllers mount their routes in app/bootstrap/routes_gen.go, which they own (bootstrap.go calls its generatedRoutes; a hand edit is detected by its checksum and refused; running a generator again with the same arguments is a no-op; destroy takes the routes back out); applications whose bootstrap.go predates it get the wiring printed instead. Nothing edits app/bootstrap/bootstrap.go. Routes are never public by accident: with Forge identity they need sign-in and the permission the migration grants to the owner role; without it the guard refuses everyone unless --public (resources and scaffolds need --global with it), and to replace a generated guard (a package-level func in routes_gen.go) the owner copies it into another file of package bootstrap and runs a generator again: until then the build fails with redeclared, never silently. In an API-only application (`forge new --api`) `generate view`, `generate scaffold`, `generate authentication` and `generate controller` without --api refuse and write nothing, as in Forge-Rust; the other generators write no view, asset or translation."},
			Flags: []flagDetail{
				{Name: "--dry-run", Description: "Read-only: print exact planned paths without creating directories or files."},
				{Name: "--org-scoped", Description: "model, resource and scaffold, in an application with organizations (forge new --tenants): rows belong to one organization (an org_id column bound in every statement; PostgreSQL also enables row-level security). --tenant-scoped is an alias. Refused in a single-organization application, whose tables are plain tables."},
				{Name: "--global", Description: "model, resource and scaffold, in an application with organizations: rows are shared by every organization (their writes stay closed until platform operators are authorized). One of --org-scoped or --global is required there."},
				{Name: "--broadcast", Description: "scaffold only: publish created, updated and deleted rows to open index pages as Turbo Streams over Server-Sent Events (adds app/adapters/htmlstreams, the _item partial and the live-update wiring)."},
				{Name: "--plural", Value: "PLURAL", Description: "model, resource and scaffold: snake_case plural used for the table and routes (default: built-in English rules)."},
				{Name: "--api", Description: "controller only: generate JSON handlers under /v1 in app/adapters/httpapi instead of HTML pages."},
				{Name: "--public", Description: "controller: mount the routes without a guard (without it the controller needs the NAME:read permission). resource and scaffold, only in applications without Forge identity and with --global: serve the routes to anyone through a pass-through public guard instead of the deny-all one."},
				{Name: "--queue", Description: "job only: generate a typed handler for the persistent job queue instead of the recurring job stub."},
				{Name: "--on-delete", Value: "restrict|cascade", Default: "restrict", Description: "model, resource and scaffold with a references field: what deleting the referenced row does. restrict refuses it while rows reference it; cascade deletes those rows with it."},
			},
		},
		{
			Name: "credentials", Usage: "forge credentials <init|edit|show|rotate-key> [--environment E] [flags]",
			Summary:     "Manage encrypted per-environment credentials (config/credentials/ENV.json.enc, AES-256-GCM): create a key and skeleton, edit in $VISUAL/$EDITOR, show redacted or revealed, rotate the key.",
			SideEffects: []string{"filesystem_read", "filesystem_write", "subprocess", "interactive_input"},
			Examples: []string{
				"forge credentials init --environment production", "forge credentials edit --environment production",
				"forge credentials show", "forge credentials show --environment production --reveal",
				"forge credentials rotate-key --environment production --print-key --keep-old",
			},
			Environment: environmentInfo{
				Files:     []string{"config/credentials/ENV.json.enc", "config/credentials/ENV.key", ".gitignore"},
				Variables: []string{"FORGE_MASTER_KEY", "FORGE_ENV", "VISUAL", "EDITOR"},
				Notes:     "The key is read from FORGE_MASTER_KEY, else config/credentials/ENV.key, which must be mode 0600 and is added to .gitignore by init. `edit` decrypts to a 0600 file in a private temporary directory (memory-backed on Linux), runs the editor without a shell, validates the JSON and re-encrypts atomically; the temporary directory is overwritten and removed on every exit path except SIGKILL. `show` redacts every value unless --reveal, which refuses to run unless standard output is a terminal. `rotate-key` re-encrypts with a new random key and needs --print-key or a key file to store it in; the key is never printed unless asked.",
			},
			Flags: []flagDetail{
				{Name: "--environment", Value: "ENV", Default: "$FORGE_ENV or development", Description: "Environment whose credentials to use (also -e)."},
				{Name: "--reveal", Description: "show: print values; requires an interactive terminal."},
				{Name: "--keep-old", Description: "rotate-key: keep the previous keys readable alongside the new one (migration window)."},
				{Name: "--print-key", Description: "rotate-key: print the new key list to standard output, for a secret manager."},
				{Name: "--write-key-file", Description: "rotate-key: store the new key in the key file even when the current key came from FORGE_MASTER_KEY."},
			},
		},
		{
			Name: "i18n", Usage: "forge i18n check [--json]",
			Summary:     "Compare the translation files in config/locales: list the keys each locale lacks (keys of any other locale, plus literal keys of t \"KEY\" in templates and i18n.T(ctx, \"KEY\") in Go), plural forms a locale's CLDR rule needs, and keys nothing references.",
			SideEffects: []string{"filesystem_read"},
			Examples:    []string{"forge i18n check", "forge i18n check --json"},
			Environment: environmentInfo{Files: []string{"config/locales/*.json"}, Variables: []string{}, Notes: "Read-only; never executes application code. Unused keys are informational: keys built at runtime are not detected, and date.*, time.* and number.* formatting data is never reported as unused."},
			Flags:       []flagDetail{{Name: "--json", Description: "Print the schema-versioned report; the exit status is 1 when a locale lacks translations or plural forms."}},
		},
		{Name: "doctor", Usage: "forge doctor [--json]", Summary: "Validate local Forge configuration and show redacted diagnostics.", SideEffects: []string{"environment_read", "filesystem_read"}, Examples: []string{"forge doctor", "forge doctor --json"}, Environment: configEnvironment("Checks configuration values and intentionally omits their contents from JSON output."), Flags: []flagDetail{{Name: "--json", Description: "Print schema-versioned diagnostics with stable remediation codes."}}},
		{Name: "inspect", Usage: "forge inspect --json", Summary: "Read-only inventory of commands, migrations, static routes, modules and database adapter; never executes application code.", SideEffects: []string{"filesystem_read", "environment_read"}, Examples: []string{"forge inspect --json"}, Environment: environmentInfo{Files: []string{".env.development", ".env.local"}, Variables: []string{"FORGE_DATABASE_URL"}, Notes: "Process environment takes precedence over env files; the database URL value is never emitted."}, Flags: []flagDetail{{Name: "--json", Description: "Print the schema-versioned project inventory."}}},
		{Name: "routes", Usage: "forge routes [NAME]", Summary: "Table of every route registered while the application composes (HTML and JSON, with the resource action and the registering source line) plus Forge's built-in routes. Runs the application's Configure without serving HTTP; falls back to a scan of app/ and cmd/ when the application cannot be run.", SideEffects: []string{"subprocess", "application_defined"}, Examples: []string{"forge routes", "forge routes worker", "forge routes | grep /v1"}, Environment: environmentInfo{Files: runtimeEnvironment().Files, Variables: runtimeEnvironment().Variables, Notes: "Builds cmd/NAME (the only binary, or the one NAME picks) and runs its routes command with the development environment; Configure sees app.Serving() false: a generated application opens its database pool lazily and starts no scheduler or worker, so the database need not be reachable and no job tick is recorded. When the application cannot be built or configured, the command reports why on stderr and lists the routes a source scan finds: literal Handle, HandleFunc and Resources paths only, which misses routes built from constants or loops."}},
		{
			Name: "openapi", Usage: "forge openapi [--output FILE | --check FILE]",
			Summary:     "Print or write the OpenAPI 3.1 document of the application's described routes (openapi.Describe, openapi.Typed); --check fails when a committed file is out of date. Runs the application's Configure without serving HTTP.",
			SideEffects: []string{"subprocess", "application_defined", "filesystem_write_optional"},
			Examples:    []string{"forge openapi", "forge openapi --output openapi.json", "forge openapi --check openapi.json"},
			Environment: environmentInfo{Files: runtimeEnvironment().Files, Variables: runtimeEnvironment().Variables, Notes: "Builds cmd/NAME and runs its openapi command with the development environment; Configure sees app.Serving() false, so a generated application opens its database pool lazily and starts no scheduler or worker. Undescribed routes are listed on stderr. Serving the document at GET /openapi.json is separate and controlled by FORGE_OPENAPI_ENABLED."},
			Flags: []flagDetail{
				{Name: "--output", Value: "FILE", Description: "Write the document to FILE instead of standard output."},
				{Name: "--check", Value: "FILE", Description: "Compare FILE with the document; the exit status is 1 when it differs."},
			},
		},
		{
			Name: "uuid", Usage: "forge uuid [--count N]",
			Summary:     "Generate UUIDv7 identifiers.",
			SideEffects: []string{"random_output"},
			Examples:    []string{"forge uuid", "forge uuid --count 5"},
			Environment: environmentInfo{Files: []string{}, Variables: []string{}},
			Flags:       []flagDetail{{Name: "--count", Value: "N", Default: "1", Description: "Number of identifiers, from 1 to 1000."}},
		},
		{Name: "version", Usage: "forge version", Summary: "Print the Forge version.", SideEffects: []string{"read_only"}, Examples: []string{"forge version"}, Environment: environmentInfo{Files: []string{}, Variables: []string{}}},
		{Name: "help", Usage: "forge help [--json | COMMAND]", Summary: "Show human-readable help, one command's help (also forge COMMAND --help) or the versioned command catalog.", SideEffects: []string{"read_only"}, Examples: []string{"forge help", "forge help new", "forge new --help", "forge help --json"}, Environment: environmentInfo{Files: []string{}, Variables: []string{}}, ExitCodes: []exitCodeDetail{{Code: 2, Meaning: "The command is unknown or the arguments are invalid."}}, Flags: []flagDetail{{Name: "--json", Description: "Print the stable machine-readable command catalog."}}},
	}
	commands = append(commands, deployCommandDetails()...)
	commands = slices.Insert(commands, slices.IndexFunc(commands, func(c commandDetail) bool { return c.Name == "generate" })+1, destroyCommandDetail())
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
		codes := []exitCodeDetail{
			{Code: 0, Meaning: "The command completed successfully."},
			{Code: 1, Meaning: "The command, or a tool or application it runs (go, gofmt, golangci-lint, the database client, the application's own commands), failed, or the configuration was invalid."},
		}
		// help documents its own, narrower code 2.
		if !slices.ContainsFunc(commands[i].ExitCodes, func(c exitCodeDetail) bool { return c.Code == 2 }) {
			codes = append(codes, exitCodeDetail{Code: 2, Meaning: "Usage error: an unknown flag or subcommand, an invalid flag value, or a missing or extra argument; nothing ran."})
		}
		commands[i].ExitCodes = append(codes, commands[i].ExitCodes...)
	}
	return commandCatalog{
		SchemaVersion: 1,
		Name:          "forge",
		Version:       effectiveVersion(version.Version, readBuildInfo),
		GlobalOptions: []flagDetail{{Name: jsonErrorFormatFlag, Description: "Emit schema-versioned, secret-safe errors on stderr when a command fails."}},
		ProcessExitCodes: append([]exitCodeDetail{
			{Code: 0, Meaning: "The requested command completed successfully."},
			{Code: 1, Meaning: "A command, or a tool or application it runs, failed, or the configuration was invalid."},
			{Code: 2, Meaning: "No command was supplied, the top-level command is unknown, forge help was given an unknown command or invalid arguments, a command got an unknown flag or an invalid flag value, or forge deploy, logs or app got an unknown subcommand or a missing or extra argument."},
		}, deployExitCodes...),
		Commands: commands,
	}
}

// migrationEnvironment is runtimeEnvironment for the commands that run the
// application's migrate and rollback, which connect as the migration role.
func migrationEnvironment() environmentInfo {
	environment := runtimeEnvironment()
	environment.Variables = append(environment.Variables, "FORGE_MIGRATION_DATABASE_URL")
	environment.Notes += " migrate and rollback connect as FORGE_MIGRATION_DATABASE_URL's role when it is set (on PostgreSQL the schema owner), to FORGE_DATABASE_URL's database."
	return environment
}

func runtimeEnvironment() environmentInfo {
	return environmentInfo{
		Files: []string{".env.development", ".env.local"},
		Variables: []string{
			"FORGE_ENV", "FORGE_HTTP_ADDR", "FORGE_HTTP_TRANSPORT", "FORGE_HTTP_TLS_CERT_FILE", "FORGE_HTTP_TLS_KEY_FILE",
			"FORGE_HTTP_READ_HEADER_TIMEOUT", "FORGE_HTTP_READ_TIMEOUT", "FORGE_HTTP_WRITE_TIMEOUT", "FORGE_HTTP_IDLE_TIMEOUT", "FORGE_HTTP_SHUTDOWN_TIMEOUT",
			"FORGE_HTTP_MAX_HEADER_BYTES", "FORGE_HTTP_MAX_BODY_BYTES", "FORGE_HTTP_TRUSTED_PROXIES", "FORGE_DATABASE_URL", "FORGE_DATABASE_MAX_CONNS", "FORGE_DATABASE_MIN_CONNS",
			"FORGE_DATABASE_MAX_CONN_LIFETIME", "FORGE_DATABASE_MAX_CONN_IDLE_TIME", "FORGE_DATABASE_CONNECT_TIMEOUT", "FORGE_DATABASE_STATEMENT_TIMEOUT",
			"FORGE_DATABASE_ALLOW_PLAINTEXT", "FORGE_AUTH_MFA_KEY", "FORGE_AUTH_PASSWORD_RESET_URL", "FORGE_AUTH_SESSION_IDLE_TIMEOUT", "FORGE_MASTER_KEY", "FORGE_OPENAPI_ENABLED",
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

func destroyCommandDetail() commandDetail {
	return commandDetail{
		Name: "destroy", Aliases: []string{"d"}, Usage: "forge destroy <model|resource|scaffold|join|controller|mailer|job|view> ARGUMENTS... [--force] [--migrations] [--dry-run]",
		Summary:     "Remove the files a generator created, found by regenerating them from the same arguments; a file that differs from what the generator would write, or that a bigger generation still needs, is never removed without --force, and migrations are kept unless --migrations.",
		SideEffects: []string{"filesystem_read", "filesystem_write"},
		Examples: []string{
			"forge destroy scaffold post title:string --org-scoped --dry-run", "forge destroy controller pages home about_us",
			"forge destroy mailer user welcome:name", "forge destroy model post title:string --global --migrations",
			"forge destroy join post tag --migrations",
		},
		Environment: environmentInfo{Files: []string{".env.development", ".env.local"}, Variables: []string{"FORGE_DATABASE_URL"}, Notes: "Takes the arguments the generator was run with, so it can compare what is on disk with what the generator writes; a file that differs is listed and nothing is removed, unless --force. A model or resource whose resource or scaffold HTTP layer is still there, and a view a controller renders, are refused the same way. A model, resource or scaffold that another one references (post:references) or joins (generate join) is refused even with --force: destroy those first; with --migrations, so is a migration that declares the UNIQUE (org_id, id) key other foreign keys still use. The views base, the live-update transport and other files shared with the application are never removed. app/bootstrap/bootstrap.go is never edited: the wiring to take out is printed."},
		Flags: []flagDetail{
			{Name: "--dry-run", Description: "Print what would be removed without removing anything."},
			{Name: "--force", Description: "Remove files that differ from what this generator would write (edited, or written by another generator), and files a bigger generation (the resource or scaffold around a model, the controller that renders a view) still needs."},
			{Name: "--migrations", Description: "Also remove the generator's migration files. Run `forge rollback` first if they were applied."},
		},
	}
}
