package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/internal/deploy"
	"github.com/rocketinsidecorp/forge-go/internal/version"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

const maxUUIDCount = 1000
const jsonErrorFormatFlag = "--error-format=json"

var errDoctorUnhealthy = errors.New("doctor found invalid configuration")

func Run(args []string, stdout, stderr io.Writer) int {
	jsonErrors := len(args) > 0 && args[0] == jsonErrorFormatFlag
	if jsonErrors {
		args = args[1:]
	}
	if len(args) == 0 {
		if jsonErrors {
			writeCLIError(stderr, "command.required", "A command is required.", 2, "")
			return 2
		}
		usage(stderr)
		return 2
	}
	commandStderr := stderr
	if jsonErrors {
		// Delegated tools and flag parsers can print paths, arguments, or other
		// input-derived text. Keep stderr machine-readable in this mode.
		commandStderr = io.Discard
	}
	var err error
	switch args[0] {
	case "version":
		err = runVersion(args[1:], stdout)
	case "uuid":
		err = runUUID(args[1:], stdout, commandStderr)
	case "doctor":
		err = runDoctor(args[1:], stdout)
	case "inspect":
		err = runInspect(args[1:], stdout)
	case "routes":
		err = runRoutes(args[1:], stdout)
	case "openapi":
		err = runOpenAPI(args[1:], stdout, commandStderr)
	case "new":
		err = runNew(args[1:], stdout, commandStderr)
	case "dev":
		err = runDev(args[1:], stdout, commandStderr)
	case "build":
		err = runBuild(args[1:], stdout, commandStderr)
	case "test":
		err = runTest(args[1:], stdout, commandStderr)
	case "db":
		err = runDB(args[1:], stdout, commandStderr)
	case "migrate":
		err = runMigrate(args[1:], stdout, commandStderr)
	case "rollback":
		err = runRollback(args[1:], stdout, commandStderr)
	case "setup":
		err = runSetup(args[1:], stdout, commandStderr)
	case "seed":
		err = runSeed(args[1:], stdout, commandStderr)
	case "generate":
		err = runGenerate(args[1:], stdout)
	case "destroy":
		err = runDestroy(args[1:], stdout)
	case "deploy":
		err = deploy.RunDeploy(args[1:], stdout, commandStderr)
	case "logs":
		err = deploy.RunLogs(args[1:], stdout, commandStderr)
	case "app":
		err = deploy.RunApp(args[1:], stdout, commandStderr)
	case "credentials":
		err = runCredentials(args[1:], stdout, commandStderr)
	case "i18n":
		err = runI18n(args[1:], stdout)
	case "help":
		err = runHelp(args[1:], stdout)
	case "-h", "--help":
		usage(stdout)
		return 0
	default:
		if jsonErrors {
			writeCLIError(stderr, "command.unknown", "Unknown command.", 2, "")
			return 2
		}
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
	if err != nil {
		if errors.Is(err, errDoctorUnhealthy) {
			return 1
		}
		if errors.Is(err, errInspectIncomplete) {
			return 1
		}
		code := 1
		var coded interface{ ExitCode() int }
		if errors.As(err, &coded) {
			code = coded.ExitCode()
		}
		if jsonErrors {
			writeCLIError(stderr, "command.failed", "Command failed.", code, args[0])
			return code
		}
		fmt.Fprintf(stderr, "forge: %v\n", err)
		return code
	}
	return 0
}

type cliErrorReport struct {
	SchemaVersion int             `json:"schema_version"`
	Error         cliErrorDetails `json:"error"`
}

type cliErrorDetails struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	ExitCode int    `json:"exit_code"`
	Command  string `json:"command,omitempty"`
}

func writeCLIError(output io.Writer, code, message string, exitCode int, command string) {
	_ = json.NewEncoder(output).Encode(cliErrorReport{
		SchemaVersion: 1,
		Error: cliErrorDetails{
			Code: code, Message: message, ExitCode: exitCode, Command: command,
		},
	})
}

func runVersion(args []string, output io.Writer) error {
	if len(args) != 0 {
		return errors.New("version accepts no arguments")
	}
	_, err := fmt.Fprintf(output, "forge %s (%s, %s)\n", version.Version, version.Commit, version.Date)
	return err
}

func runUUID(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("uuid", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	count := flags.Int("count", 1, "number of UUIDv7 values")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *count < 1 || *count > maxUUIDCount {
		return fmt.Errorf("count must be between 1 and %d", maxUUIDCount)
	}
	for range *count {
		id, err := uuid.New()
		if err != nil {
			return fmt.Errorf("generate UUIDv7: %w", err)
		}
		if _, err := fmt.Fprintln(output, id); err != nil {
			return err
		}
	}
	return nil
}

func runDoctor(args []string, output io.Writer) error {
	return runDoctorAt(args, output, ".", os.Environ())
}

type doctorReport struct {
	SchemaVersion int                `json:"schema_version"`
	OK            bool               `json:"ok"`
	Environment   string             `json:"environment,omitempty"`
	Database      string             `json:"database,omitempty"`
	Diagnostics   []doctorDiagnostic `json:"diagnostics"`
}

type doctorDiagnostic struct {
	Code        string `json:"code"`
	Severity    string `json:"severity"`
	Summary     string `json:"summary"`
	Remediation string `json:"remediation"`
}

func runDoctorAt(args []string, output io.Writer, root string, baseEnv []string) error {
	jsonOutput := len(args) == 1 && args[0] == "--json"
	if len(args) != 0 && !jsonOutput {
		return errors.New("usage: forge doctor [--json]")
	}
	env, err := environment(root, baseEnv)
	if err != nil {
		if jsonOutput {
			return writeDoctorFailure(output, doctorDiagnostic{
				Code:        "environment_file.invalid",
				Severity:    "error",
				Summary:     "Development environment files could not be loaded.",
				Remediation: "Check .env.development and .env.local for valid KEY=VALUE lines.",
			})
		}
		return err
	}
	cfg, err := config.LoadWithLookup(lookupIn(env))
	if err != nil {
		if jsonOutput {
			return writeDoctorFailure(output, doctorDiagnostic{
				Code:        "configuration.invalid",
				Severity:    "error",
				Summary:     "Forge configuration is invalid.",
				Remediation: "Review FORGE_* settings against the configuration guide; secret values are intentionally omitted.",
			})
		}
		return fmt.Errorf("configuration invalid: %w", err)
	}
	database := "not configured"
	if !cfg.Database.URL.IsZero() {
		database = describeDatabase(cfg.Database.URL.Reveal())
	}
	if jsonOutput {
		adapter := string(cfg.Database.Adapter())
		if adapter == "" {
			adapter = "none"
		}
		return json.NewEncoder(output).Encode(doctorReport{
			SchemaVersion: 1,
			OK:            true,
			Environment:   string(cfg.Environment),
			Database:      adapter,
			Diagnostics:   []doctorDiagnostic{},
		})
	}
	_, err = fmt.Fprintf(output, "configuration: ok\nenvironment: %s\nhttp address: %s\ntransport: %s\ndatabase: %s\n",
		cfg.Environment, cfg.HTTP.Address, cfg.HTTP.Transport, database)
	return err
}

func writeDoctorFailure(output io.Writer, diagnostic doctorDiagnostic) error {
	if err := json.NewEncoder(output).Encode(doctorReport{
		SchemaVersion: 1,
		OK:            false,
		Diagnostics:   []doctorDiagnostic{diagnostic},
	}); err != nil {
		return err
	}
	return errDoctorUnhealthy
}

// describeDatabase prints only scheme, user, host, database or file, and
// the transport-security parameter (sslmode, tls). Passwords can hide in the
// userinfo and in query parameters (password, sslpassword), so nothing else
// of the URL is echoed.
func describeDatabase(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "configured (unparseable)"
	}
	safe := url.URL{Scheme: parsed.Scheme, Opaque: parsed.Opaque, Host: parsed.Host, Path: parsed.Path}
	if parsed.User != nil {
		safe.User = url.User(parsed.User.Username())
	}
	kept := url.Values{}
	for _, key := range []string{"sslmode", "tls"} {
		if value := parsed.Query().Get(key); value != "" {
			kept.Set(key, value)
		}
	}
	safe.RawQuery = kept.Encode()
	return safe.String()
}

// lookupIn adapts a KEY=VALUE list to an environment lookup; later entries win.
func lookupIn(env []string) func(string) (string, bool) {
	values := make(map[string]string, len(env))
	for _, entry := range env {
		if key, value, ok := strings.Cut(entry, "="); ok {
			values[key] = value
		}
	}
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func usage(output io.Writer) {
	fmt.Fprint(output, `usage: forge [--error-format=json] <command> [arguments]

commands:
  new NAME            generate an application with Dockerfile, compose and CI;
                      --database postgresql (default), mysql, mariadb, sqlite or none;
                      --locale en (default), es or pt-BR
  dev [NAME] [--no-reload] [--poll-interval D]
                      run the application; rebuild, restart gracefully and
                      reload the browser when files change
  test [ARGS]         go test with .env.development loaded (default ./...)
  build               compile the application into bin/
  db COMMAND          database lifecycle: create, drop, reset, setup, prepare,
                      migrate, rollback, status, seed, schema:dump,
                      schema:load, console (see README)
  migrate [status]    apply pending migrations, or list them
  rollback [-steps N] revert the latest application migrations
  setup --tenant SLUG --email EMAIL [--organization NAME] [--password-stdin]
                      create the first tenant and its owner account (PostgreSQL;
                      the password is prompted for, never an argument)
  seed                load db.Seed's data in one transaction
  generate migration NAME [--dry-run]
                      create or preview db/migrations/<timestamp>_NAME.{up,down}.sql
  generate view [DIR/]NAME [--dry-run]
                      create or preview an auto-escaped page in app/views
  generate job NAME [--dry-run]
                      create or preview a cancellation-aware job stub and tests
  generate job NAME [field:type...] --queue [--dry-run]
                      a typed handler for the persistent job queue, with its
                      enqueue function and tests
  generate model NAME field:type... (--tenant-scoped | --global) [--plural P] [--dry-run]
                      a migration, domain, application and persistence layers
                      with tests; no HTTP layer
  generate controller NAME ACTION... [--api] [--public] [--dry-run]
                      HTTP handlers, routes, views and tests, no persistence;
                      never public unless --public
  generate mailer NAME METHOD[:field,field]... [--dry-run]
                      a mailer with typed methods, text and HTML templates,
                      previews and tests
  generate resource NAME field:type... (--tenant-scoped | --global) [--plural P] [--dry-run]
                      create or preview a migration, domain, application,
                      persistence and HTTP layers with tests; never public
  generate scaffold NAME field:type... (--tenant-scoped | --global) [--plural P] [--dry-run]
                      a resource plus HTML CRUD pages: forms, 422 errors,
                      flash notices, CSRF; never public
  generate authentication [--dry-run]
                      add the sign-in, sign-out, two-factor and password-reset
                      pages forge new generates (PostgreSQL)
  destroy GENERATOR ARGUMENTS... [--force] [--migrations] [--dry-run]
                      remove what a generator created, unless it was edited
  deploy [init|setup|rollback VERSION|prune|details|lock|maintenance]
                      deploy containers to hosts over SSH behind forge-proxy;
                      --dry-run prints the remote commands, secrets redacted
  logs [-f]           stream container logs from every host (host| prefix)
  app exec|shell      run a command or a shell next to the deployed app
  credentials <init|edit|show|rotate-key> [--environment E]
                      encrypted per-environment credentials (config/credentials)
  i18n check [--json] list translations missing per locale in config/locales
                      and keys no template or Go code references
  doctor [--json]     validate the FORGE_* configuration
  inspect --json      inspect app structure without executing it
  routes              list the routes found in the source, as a table
  openapi [--output FILE | --check FILE]
                      print or write the OpenAPI 3.1 document of the described
                      routes; --check fails when FILE is out of date
  help --json         print the versioned machine-readable command catalog
  uuid [--count N]    generate UUIDv7 identifiers
  version             print the Forge version
  --error-format=json  emit versioned, secret-safe errors on stderr
`)
}
