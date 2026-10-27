package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/format"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
	"github.com/rocketinsidecorp/forge-go/internal/waittest"
	"github.com/rocketinsidecorp/forge-go/mysql/mysqltest"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestRunVersion(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "forge ") || !strings.Contains(stdout.String(), " go1.") {
		t.Fatalf("stdout=%q, want the version, commit, date and Go toolchain", stdout.String())
	}
}

func TestRunJSONErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		code     int
		wantCode string
		wantExit int
		command  string
		wantJSON string
	}{
		{name: "missing command", args: []string{"--error-format=json"}, code: 2, wantCode: "command.required", wantExit: 2, wantJSON: `{"schema_version":1,"error":{"code":"command.required","message":"A command is required.","exit_code":2}}` + "\n"},
		{name: "unknown command", args: []string{"--error-format=json", "private-secret"}, code: 2, wantCode: "command.unknown", wantExit: 2, wantJSON: `{"schema_version":1,"error":{"code":"command.unknown","message":"Unknown command.","exit_code":2}}` + "\n"},
		{name: "failed command", args: []string{"--error-format=json", "uuid", "--count", "0"}, code: 1, wantCode: "command.failed", wantExit: 1, command: "uuid", wantJSON: `{"schema_version":1,"error":{"code":"command.failed","message":"Command failed.","exit_code":1,"command":"uuid"}}` + "\n"},
		{name: "invalid argument", args: []string{"--error-format=json", "version", "private-secret"}, code: 2, wantCode: "command.failed", wantExit: 2, command: "version", wantJSON: `{"schema_version":1,"error":{"code":"command.failed","message":"Command failed.","exit_code":2,"command":"version"}}` + "\n"},
		{name: "invalid flag", args: []string{"--error-format=json", "uuid", "--private-secret"}, code: 2, wantCode: "command.failed", wantExit: 2, command: "uuid", wantJSON: `{"schema_version":1,"error":{"code":"command.failed","message":"Command failed.","exit_code":2,"command":"uuid"}}` + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := Run(tt.args, &stdout, &stderr); got != tt.code {
				t.Fatalf("Run() = %d, want %d", got, tt.code)
			}
			if stdout.Len() != 0 {
				t.Fatalf("unexpected stdout: %q", stdout.String())
			}
			if strings.Contains(stderr.String(), "private-secret") {
				t.Fatalf("JSON error leaked argument data: %s", stderr.String())
			}
			if got := stderr.String(); got != tt.wantJSON {
				t.Fatalf("JSON error changed:\n got: %s\nwant: %s", got, tt.wantJSON)
			}
			var report cliErrorReport
			if err := json.Unmarshal(stderr.Bytes(), &report); err != nil {
				t.Fatalf("decode JSON error: %v: %s", err, stderr.String())
			}
			if report.SchemaVersion != 1 || report.Error.Code != tt.wantCode || report.Error.ExitCode != tt.wantExit || report.Error.Command != tt.command {
				t.Fatalf("unexpected error report: %+v", report)
			}
			if report.Error.Message == "" {
				t.Fatal("error message is empty")
			}
		})
	}
}

func TestRunHelpJSONProvidesCompleteCommandCatalog(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"help", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var catalog struct {
		SchemaVersion    int    `json:"schema_version"`
		Name             string `json:"name"`
		ProcessExitCodes []struct {
			Code int `json:"code"`
		} `json:"process_exit_codes"`
		GlobalOptions []struct {
			Name string `json:"name"`
		} `json:"global_options"`
		Commands []struct {
			Name        string   `json:"name"`
			Usage       string   `json:"usage"`
			SideEffects []string `json:"side_effects"`
			Examples    []string `json:"examples"`
			Environment struct {
				Files     []string `json:"files"`
				Variables []string `json:"variables"`
			} `json:"environment"`
			ExitCodes []struct {
				Code    int    `json:"code"`
				Meaning string `json:"meaning"`
			} `json:"exit_codes"`
			Flags []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"flags"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &catalog); err != nil {
		t.Fatalf("decode command catalog: %v\n%s", err, stdout.String())
	}
	if catalog.SchemaVersion != 1 || catalog.Name != "forge" || len(catalog.Commands) != 26 || len(catalog.ProcessExitCodes) != 8 || len(catalog.GlobalOptions) != 1 || catalog.GlobalOptions[0].Name != jsonErrorFormatFlag {
		t.Fatalf("unexpected catalog header: %+v", catalog)
	}
	rollbackDeclaredDestructive := false
	generateDeclaresDryRun := false
	generateDeclaresJob := false
	commandsSeen := make(map[string]bool, len(catalog.Commands))
	processExitCodesSeen := make(map[int]bool, len(catalog.ProcessExitCodes))
	expectedCommands := map[string]bool{
		"new": true, "dev": true, "test": true, "build": true, "migrate": true, "rollback": true, "setup": true, "seed": true, "db": true,
		"generate": true, "doctor": true, "inspect": true, "routes": true, "uuid": true, "version": true, "help": true,
		"credentials": true, "deploy": true, "logs": true, "app": true, "destroy": true, "i18n": true, "openapi": true,
		"check": true, "format": true, "lint": true,
	}
	for _, command := range catalog.Commands {
		if !expectedCommands[command.Name] || commandsSeen[command.Name] || len(command.Examples) == 0 || command.Environment.Files == nil || command.Environment.Variables == nil || len(command.ExitCodes) < 2 {
			t.Errorf("command is missing complete machine-readable metadata: %+v", command)
		}
		commandsSeen[command.Name] = true
		if command.Name == "generate" {
			generateDeclaresJob = strings.Contains(command.Usage, "|job|model|controller|mailer|resource|scaffold|authentication|tenancy>") && containsString(command.Examples, "forge generate job cleanup --dry-run") &&
				containsString(command.Examples, "forge generate resource category name:string --global --plural categories") &&
				containsString(command.Examples, "forge generate scaffold post title:string body:text published:bool --global --dry-run")
		}
		commandExitCodesSeen := make(map[int]bool, len(command.ExitCodes))
		for _, exitCode := range command.ExitCodes {
			if commandExitCodesSeen[exitCode.Code] {
				t.Errorf("command %s documents exit code %d twice", command.Name, exitCode.Code)
			}
			commandExitCodesSeen[exitCode.Code] = true
			if exitCode.Code == 1 && strings.Contains(exitCode.Meaning, "invalid arguments") {
				t.Errorf("command %s: exit code 1 must not describe usage errors, which exit 2: %q", command.Name, exitCode.Meaning)
			}
		}
		if !commandExitCodesSeen[0] || !commandExitCodesSeen[1] || !commandExitCodesSeen[2] {
			t.Errorf("command %s has incomplete exit-code documentation: %v", command.Name, commandExitCodesSeen)
		}
		if !strings.HasPrefix(command.Examples[0], "forge ") {
			t.Errorf("example is not a copyable Forge command: %q", command.Examples[0])
		}
		if command.Name == "rollback" && containsString(command.SideEffects, "database_destructive") {
			rollbackDeclaredDestructive = true
		}
		if command.Name == "test" && (!containsString(command.Environment.Files, ".env.development") || !containsString(command.Environment.Variables, "FORGE_TEST_DATABASE_URL")) {
			t.Errorf("test command omits its environment inputs: %+v", command.Environment)
		}
		if command.Name == "generate" {
			for _, flag := range command.Flags {
				if flag.Name == "--dry-run" && strings.Contains(flag.Description, "without creating") {
					generateDeclaresDryRun = true
				}
			}
		}
	}
	if !rollbackDeclaredDestructive || !generateDeclaresDryRun || !generateDeclaresJob {
		t.Fatalf("catalog rollback=%v generate dry-run=%v generate job=%v", rollbackDeclaredDestructive, generateDeclaresDryRun, generateDeclaresJob)
	}
	hasUsageExitCode := false
	for _, exitCode := range catalog.ProcessExitCodes {
		processExitCodesSeen[exitCode.Code] = true
		hasUsageExitCode = hasUsageExitCode || exitCode.Code == 2
	}
	if !hasUsageExitCode || !processExitCodesSeen[0] || !processExitCodesSeen[1] {
		t.Fatalf("catalog process exit codes are incomplete: %v", processExitCodesSeen)
	}
	if len(commandsSeen) != len(expectedCommands) {
		t.Fatalf("catalog command set = %v, want %v", commandsSeen, expectedCommands)
	}
}

func TestRunHelpRejectsUnknownFormat(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"help", "--xml"}, &stdout, &stderr); code == 0 {
		t.Fatal("unsupported help format unexpectedly succeeded")
	}
}

func TestRunDoctorJSONReturnsSecretSafeDiagnostics(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	var stdout bytes.Buffer
	err := runDoctorAt([]string{"--json"}, &stdout, root, []string{
		"FORGE_ENV=production",
		"FORGE_HTTP_TRANSPORT=plain",
		"FORGE_DATABASE_URL=postgres://alice:super-secret@db.example/app",
	})
	if !errors.Is(err, errDoctorUnhealthy) {
		t.Fatalf("error = %v, want errDoctorUnhealthy", err)
	}
	if strings.Contains(stdout.String(), "super-secret") || strings.Contains(stdout.String(), "db.example") {
		t.Fatalf("JSON diagnostic leaked configuration: %s", stdout.String())
	}
	var report doctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode doctor JSON: %v\n%s", err, stdout.String())
	}
	if report.SchemaVersion != 1 || report.OK || len(report.Diagnostics) != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if report.Diagnostics[0].Code != "configuration.invalid" || report.Diagnostics[0].Remediation == "" {
		t.Fatalf("diagnostic lacks stable code/remediation: %+v", report.Diagnostics[0])
	}
}

func TestRunDoctorJSONReportsHealthyConfigurationWithoutDatabaseSecrets(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	var stdout bytes.Buffer
	if err := runDoctorAt([]string{"--json"}, &stdout, root, []string{
		"FORGE_DATABASE_URL=postgres://alice:super-secret@db.example/app?sslmode=verify-full",
	}); err != nil {
		t.Fatalf("run doctor: %v", err)
	}
	if strings.Contains(stdout.String(), "super-secret") || strings.Contains(stdout.String(), "db.example") {
		t.Fatalf("JSON doctor leaked database URL details: %s", stdout.String())
	}
	var report doctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode doctor JSON: %v\n%s", err, stdout.String())
	}
	if report.SchemaVersion != 1 || !report.OK || report.Environment != "development" || report.Database == "" || len(report.Diagnostics) != 0 {
		t.Fatalf("unexpected healthy report: %+v", report)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestRunUUID(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"uuid", "--count", "2"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	lines := strings.Fields(stdout.String())
	if len(lines) != 2 {
		t.Fatalf("UUID count=%d", len(lines))
	}
	for _, line := range lines {
		if _, err := uuid.Parse(line); err != nil {
			t.Fatalf("parse generated UUID: %v", err)
		}
	}
}

func TestRunRejectsUnknownCommandAndLargeCount(t *testing.T) {
	t.Parallel()
	tests := [][]string{{"unknown"}, {"uuid", "--count", "1001"}}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code == 0 {
			t.Fatalf("args=%v unexpectedly succeeded", args)
		}
	}
}

func TestRunNewGeneratesApplicationWithoutDependencies(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "shop")
	var stdout, stderr bytes.Buffer
	// The positional name may come before or after flags.
	if code := Run([]string{"new", "--skip-deps", "--dir", dir, "shop"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	for _, path := range []string{"Dockerfile", ".dockerignore", "compose.yaml", "cmd/shop/main.go"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(path))); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
	}
	if !strings.Contains(stdout.String(), "docker compose up --build") || !strings.Contains(stdout.String(), "http://127.0.0.1:8080/") {
		t.Fatalf("missing next steps: %s", stdout.String())
	}
}

func TestRunNewRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tests := [][]string{
		{"new"},
		{"new", "Bad_Name", "--skip-deps"},
		{"new", "one", "two"},
		{"new", "app", "--dir", filepath.Join(dir, "app"), "--forge-path", dir},
		{"new", "app2", "--skip-deps", "--database", "oracle"},
		{"new", "app3", "--skip-deps", "--database", "mysql", "--skip-database"},
	}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code == 0 {
			t.Fatalf("args=%v unexpectedly succeeded", args)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "app")); !os.IsNotExist(err) {
		t.Fatalf("invalid framework path still generated files: %v", err)
	}
}

func TestResolveCommand(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if _, err := resolveCommand(root, nil); err == nil {
		t.Fatal("expected missing go.mod to fail")
	}
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/app\n")
	writeFile(t, filepath.Join(root, "cmd", "api", "main.go"), "package main\n")
	if name, err := resolveCommand(root, nil); err != nil || name != "api" {
		t.Fatalf("name=%q err=%v", name, err)
	}
	writeFile(t, filepath.Join(root, "cmd", "worker", "main.go"), "package main\n")
	if _, err := resolveCommand(root, nil); err == nil {
		t.Fatal("expected ambiguity error with two commands")
	}
	if name, err := resolveCommand(root, []string{"worker"}); err != nil || name != "worker" {
		t.Fatalf("name=%q err=%v", name, err)
	}
	for _, args := range [][]string{{"../escape"}, {"missing"}, {"api", "worker"}} {
		if _, err := resolveCommand(root, args); err == nil {
			t.Fatalf("args=%v unexpectedly resolved", args)
		}
	}
}

// TestGeneratedApplication is the end-to-end check for `forge new`: generated
// projects must compile, vet and pass their own tests against this checkout of
// the framework, and the database workflow must work end to end.
func TestGeneratedApplication(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	frameworkPath, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	generate := func(t *testing.T, name string, extra ...string) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), name)
		args := append([]string{"new", name, "--module", "example.com/acme/" + name, "--dir", dir, "--forge-path", frameworkPath}, extra...)
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
		}
		if _, err := os.Stat(filepath.Join(dir, "vendor", "modules.txt")); err != nil {
			t.Fatalf("framework was not vendored: %v", err)
		}
		if _, err := generateView(dir, "welcome"); err != nil {
			t.Fatalf("generate an embedded view: %v", err)
		}
		var jobOutput bytes.Buffer
		if err := runGenerateAt([]string{"job", "cleanup"}, &jobOutput, dir); err != nil {
			t.Fatalf("generate a job: %v", err)
		}
		for _, args := range [][]string{{"vet", "./..."}, {"test", "./..."}, {"build", "-o", os.DevNull, "./cmd/" + name}} {
			command := exec.Command("go", args...)
			command.Dir = dir
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("go %v: %v\n%s", args, err, output)
			}
		}
		return dir
	}

	// The README quickstart, run as written by a real forge binary, then
	// driven over HTTP like a person in a browser (readme_quickstart_test.go).
	for _, name := range []string{"sqlite3", "mysql", "postgresql"} {
		t.Run("readme quickstart "+name, func(t *testing.T) {
			t.Parallel()
			testReadmeQuickstart(t, frameworkPath, name)
		})
	}
	t.Run("readme quickstart welcome", func(t *testing.T) {
		t.Parallel()
		testWelcomeQuickstart(t, frameworkPath)
	})

	t.Run("without database", func(t *testing.T) {
		t.Parallel()
		dir := generate(t, "edge", "--skip-database")
		if _, err := os.Stat(filepath.Join(dir, "db")); !os.IsNotExist(err) {
			t.Fatalf("db/ generated with --skip-database: %v", err)
		}
	})

	// API-only applications (forge new --api): no view, asset or browser
	// middleware; the generators that write pages refuse them, and the JSON
	// resources compile, pass their database tests and lint once wired.
	for _, database := range []struct {
		name, adapter string
		config        func(testing.TB) config.Database
	}{
		{"postgresql", "postgres", postgrestest.Config},
		{"sqlite", "sqlite", sqlitetest.Config},
	} {
		t.Run("api "+database.name, func(t *testing.T) {
			t.Parallel()
			cfg := database.config(t)
			dir := filepath.Join(t.TempDir(), "ledger")
			var stdout, stderr bytes.Buffer
			args := []string{"new", "ledger", "--api", "--module", "example.com/acme/ledger", "--dir", dir, "--forge-path", frameworkPath, "-d", database.name}
			if code := Run(args, &stdout, &stderr); code != 0 {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			assertNoBrowserFiles(t, dir)
			for _, refused := range [][]string{{"view", "welcome"}, {"scaffold", "post", "title:string", "--global"}} {
				if err := runGenerateAt(refused, io.Discard, dir); err == nil || !strings.Contains(err.Error(), "forge new --api") {
					t.Fatalf("forge generate %v in an API-only application: %v", refused, err)
				}
			}
			generateAndWireResources(t, dir, database.adapter, false)
			assertNoBrowserFiles(t, dir)
			// The binary migrates the application's schema only: an API-only
			// application has no Forge identity (decision D31) and no setup
			// command.
			writeFile(t, filepath.Join(dir, ".env.local"), "FORGE_DATABASE_URL="+cfg.URL.Reveal()+"\nFORGE_MIGRATION_DATABASE_URL="+cfg.URL.Reveal()+"\n")
			env, err := environment(dir, os.Environ())
			if err != nil {
				t.Fatal(err)
			}
			var migrated bytes.Buffer
			if err := runApp(context.Background(), dir, "ledger", []string{"migrate"}, env, &migrated, &migrated); err != nil {
				t.Fatalf("ledger migrate: %v\n%s", err, migrated.String())
			}
			if strings.Contains(migrated.String(), "_identity") {
				t.Fatalf("an API-only application migrated Forge identity:\n%s", migrated.String())
			}
			for _, name := range []string{"_create_products", "_create_categories"} {
				if !strings.Contains(migrated.String(), name) {
					t.Fatalf("ledger migrate did not apply %s:\n%s", name, migrated.String())
				}
			}
			var setup bytes.Buffer
			if err := runApp(context.Background(), dir, "ledger", []string{"setup"}, env, &setup, &setup); err == nil || !strings.Contains(setup.String(), `unknown command "setup"`) {
				t.Fatalf("an API-only application has a setup command: %v\n%s", err, setup.String())
			}
			command := exec.Command("go", "test", "-count=1", "-v", "./app/bootstrap")
			command.Dir = dir
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("go test ./app/bootstrap: %v\n%s", err, output)
			}
			for _, want := range []string{"--- PASS: TestNoBrowserPipeline", "--- PASS: TestWelcomeNamesTheService", "--- PASS: TestConfigureConnectsToDatabase"} {
				if !strings.Contains(string(output), want) {
					t.Fatalf("missing %q\n%s", want, output)
				}
			}
			lint, err := exec.LookPath("golangci-lint")
			if err != nil {
				t.Log("golangci-lint is not on PATH; the lint check was skipped")
				return
			}
			writeFile(t, filepath.Join(dir, ".golangci.yml"), readFile(t, filepath.Join(frameworkPath, ".golangci.yml")))
			golangciLint.Lock()
			defer golangciLint.Unlock()
			// The vendored framework, as a developer's `forge lint` sees it.
			command = exec.Command(lint, "run", "--allow-parallel-runners", "./...")
			command.Dir = dir
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("golangci-lint: %v\n%s", err, output)
			}
		})
	}

	// Each database: generated tests pass, a generated resource compiles and
	// its integration tests run against a real server, and the migration
	// workflow runs through the application binary. The generated resource
	// code depends only on the adapter, so "mariadb" (the mysql adapter)
	// exercises the same code against whichever server FORGE_TEST_MYSQL_URL
	// names, MySQL or MariaDB.
	for _, database := range []struct {
		name    string
		adapter string // as `forge inspect` reports it
		config  func(testing.TB) config.Database
	}{
		{"postgresql", "postgres", postgrestest.Config},
		{"mysql", "mysql", mysqltest.Config},
		{"mariadb", "mysql", mysqltest.Config},
		{"sqlite", "sqlite", sqlitetest.Config},
	} {
		t.Run("database workflow "+database.name, func(t *testing.T) {
			t.Parallel()
			// Skips (or fails under FORGE_TEST_REQUIRE_DATABASE) before any
			// work when the database server is missing.
			cfg := database.config(t)
			// With organizations (forge new --tenants -d DB): Forge identity
			// on every database, and tables scoped to an organization.
			dir := generate(t, "ledger", "--database", database.name, "--tenants")
			up, down, err := generateMigration(dir, "create_entries", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, up, "CREATE TABLE entries (id CHAR(36) PRIMARY KEY, amount_cents BIGINT NOT NULL);\n")
			writeFile(t, down, "DROP TABLE entries;\n")

			generateAndWireResources(t, dir, database.adapter, true)

			// The database settings come from .env.local, as a developer
			// would configure them. One role serves and migrates here: the
			// generated PostgreSQL FORGE_MIGRATION_DATABASE_URL names the
			// Compose server, so a developer's own server replaces it too.
			writeFile(t, filepath.Join(dir, ".env.local"), "FORGE_DATABASE_URL="+cfg.URL.Reveal()+"\nFORGE_MIGRATION_DATABASE_URL="+cfg.URL.Reveal()+"\n")
			env, err := environment(dir, os.Environ())
			if err != nil {
				t.Fatal(err)
			}
			run := func(args ...string) string {
				t.Helper()
				var output bytes.Buffer
				if err := runApp(context.Background(), dir, "ledger", args, env, &output, &output); err != nil {
					t.Fatalf("ledger %v: %v\n%s", args, err, output.String())
				}
				return output.String()
			}
			output := run("migrate")
			for _, want := range []string{"_create_entries", "_create_products", "_create_categories"} {
				if !strings.Contains(output, want) {
					t.Fatalf("migrate output is missing %s: %s", want, output)
				}
			}
			if output := run("migrate", "status"); !strings.Contains(output, "applied") {
				t.Fatalf("status output: %s", output)
			}
			// The binary describes the wired resources without serving.
			var spec, notes bytes.Buffer
			if err := runApp(context.Background(), dir, "ledger", []string{"openapi"}, env, &spec, &notes); err != nil {
				t.Fatalf("ledger openapi: %v\n%s", err, notes.String())
			}
			var document struct {
				Paths map[string]any `json:"paths"`
			}
			if err := json.Unmarshal(spec.Bytes(), &document); err != nil {
				t.Fatalf("ledger openapi printed no JSON document: %v\n%s", err, spec.String())
			}
			for _, path := range []string{"/v1/status", "/health", "/v1/products", "/v1/products/{id}", "/v1/categories/{id}"} {
				if _, ok := document.Paths[path]; !ok {
					t.Errorf("ledger openapi does not describe %s", path)
				}
			}
			// Every migration reverts cleanly (on PostgreSQL the resource's
			// permissions and their grants go with it) and applies again.
			if output := run("rollback", "-steps", "3"); !strings.Contains(output, "reverted") {
				t.Fatalf("rollback output: %s", output)
			}
			// Only Forge's own migrations are left: the scheduled job
			// ticks table (newest, every server database) and the
			// identity schema are never rolled back (ADR 0039).
			var rest bytes.Buffer
			err = runApp(context.Background(), dir, "ledger", []string{"rollback"}, env, &rest, &rest)
			if database.adapter == "sqlite" {
				if err == nil || !strings.Contains(rest.String(), "20260927090000_identity, from the auth package") {
					t.Fatalf("rollback with only Forge's migrations left: %v\n%s", err, rest.String())
				}
			} else if err == nil || !strings.Contains(rest.String(), "20261026120000_job_schedules, from the jobs package") {
				t.Fatalf("rollback with only Forge's migrations left: %v\n%s", err, rest.String())
			}
			if output := run("migrate"); !strings.Contains(output, "_create_products") {
				t.Fatalf("migrate after rollback: %s", output)
			}
		})
	}
}

// generateAndWireResources generates a tenant-scoped resource with every
// field type (required and optional) and a global one, pastes the printed
// wiring into bootstrap.go, and checks that the application vets, builds and
// passes its tests, including the database integration tests, which must run
// rather than skip. identity says the application has Forge identity and
// organizations (forge new --tenants); without it the tables are plain.
func generateAndWireResources(t *testing.T, dir, adapter string, identity bool) {
	t.Helper()
	// The database is read from .env.development; keep a developer's own
	// FORGE_DATABASE_URL out of the decision.
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "FORGE_DATABASE_URL=") {
			env = append(env, entry)
		}
	}
	requests := [][]string{
		{
			"product", "name:string", "summary:string?", "body:text", "notes:text?", "quantity:int", "limit_count:int?",
			"active:bool", "featured:bool?", "released_at:time", "retired_at:time?", "owner_id:uuid", "parent_id:uuid?", "--tenant-scoped",
		},
		{"category", "name:string", "description:text?", "--global"},
	}
	var specs []resourceSpec
	for _, args := range requests {
		if !identity {
			args = slices.DeleteFunc(slices.Clone(args), func(arg string) bool { return arg == "--tenant-scoped" || arg == "--global" })
		}
		request, err := parseResourceArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err := runGenerateEnv(append([]string{"resource"}, args...), &output, dir, env, time.Now()); err != nil {
			t.Fatalf("generate resource %s: %v", request.Name, err)
		}
		specs = append(specs, resourceSpec{resourceRequest: request, Module: "example.com/acme/ledger", Database: adapter, Identity: identity, Organizations: identity})
		if !strings.Contains(output.String(), "create  "+filepath.Join("app", "domain", request.Name+".go")) {
			t.Fatalf("generate output: %s", output.String())
		}
	}
	wireResources(t, dir, specs)

	// The generated code imports framework packages the application did not
	// use before; the vendored framework copy needs them.
	for _, args := range [][]string{{"mod", "vendor"}, {"vet", "./..."}, {"build", "-o", os.DevNull, "./cmd/ledger"}} {
		command := exec.Command("go", args...)
		command.Dir = dir
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, output)
		}
	}
	command := exec.Command("go", "test", "-count=1", "./...")
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go test ./...: %v\n%s", err, output)
	}
	// Again, verbosely, for the generated packages only: their tests must run,
	// not skip. (The scaffold's own bootstrap test skips by design.)
	command = exec.Command("go", "test", "-count=1", "-v", "./app/domain", "./app/application", "./app/adapters/...")
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go test: %v\n%s", err, output)
	}
	wants := []string{
		"--- PASS: TestProductRepositoryCreateAndGet",
		"--- PASS: TestCategoryRepositoryCreateAndGet", "--- PASS: TestProductLifecycle", "--- PASS: TestCategoryServiceCreate",
	}
	if identity {
		wants = append(wants, "--- PASS: TestProductRepositoryIsolatesTenants")
	}
	for _, want := range wants {
		if !strings.Contains(string(output), want) {
			t.Fatalf("generated tests did not run: missing %q\n%s", want, output)
		}
	}
	// Row-level security guards organizations; plain tables have none.
	if adapter == "postgres" && identity && !strings.Contains(string(output), "--- PASS: TestProductRowLevelSecurity") {
		t.Fatalf("the row-level security test did not run\n%s", output)
	}
	if strings.Contains(string(output), "--- SKIP") {
		t.Fatalf("a generated test was skipped; the database integration tests must run\n%s", output)
	}
}

// wireResources applies the wiring `forge generate resource` prints to the
// generated bootstrap.go, exactly as a developer pasting it would.
func wireResources(t *testing.T, dir string, specs []resourceSpec) {
	t.Helper()
	var parts []resourceWiringParts
	for _, spec := range specs {
		parts = append(parts, buildResourceWiring(spec))
	}
	wireRoutes(t, dir, parts)
}

// wireRoutes is wireParts for the generators that wire their routes
// themselves (resource, scaffold, controller): an application whose
// bootstrap.go calls the routes seam needs no paste, and the seam must then
// hold every usage.
func wireRoutes(t *testing.T, dir string, partsList []resourceWiringParts) {
	t.Helper()
	if !strings.Contains(readFile(t, filepath.Join(dir, "app", "bootstrap", "bootstrap.go")), scaffold.RoutesFunction+"(") {
		wireParts(t, dir, partsList)
		return
	}
	// With Forge identity the seam's guards take the session store: compare
	// the printed wiring with the seam's calls of them left out.
	seam := strings.ReplaceAll(readFile(t, filepath.Join(dir, scaffold.RoutesFile)), "(repository, middleware)", "")
	for _, parts := range partsList {
		for _, line := range strings.Split(strings.TrimSpace(parts.Usage), "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.Contains(seam, line) {
				t.Fatalf("the generators did not wire %q into %s:\n%s", line, scaffold.RoutesFile, seam)
			}
		}
	}
}

// declaredName is the variable a printed wiring block declares first
// ("protect", "renderer"), which tells whether the block is already present.
var declaredName = regexp.MustCompile(`(?m)^\t(\w+)(?:, err)? := `)

// wireParts pastes the wiring of any generator into bootstrap.go: the imports,
// each once-per-application block when it is not there yet, and the usage.
func wireParts(t *testing.T, dir string, partsList []resourceWiringParts) {
	t.Helper()
	path := filepath.Join(dir, "app", "bootstrap", "bootstrap.go")
	source := readFile(t, path)
	var imports, usage strings.Builder
	var guard string
	addOnce := func(block string) {
		if block == "" {
			return
		}
		if match := declaredName.FindStringSubmatch(block); len(match) > 1 && regexp.MustCompile(`(?m)^\t`+regexp.QuoteMeta(match[1])+`(, err)? := `).MatchString(guard) {
			return
		}
		guard += "\n" + block
	}
	for _, parts := range partsList {
		// Live updates: the signer comes before the view renderer.
		if parts.StreamSigner != "" && !strings.Contains(source, "streamSigner, err :=") && !strings.Contains(guard, "streamSigner, err :=") {
			const identityRenderer = "\trenderer, err := views.New(views.WithI18n(translator))\n"
			if strings.Contains(source, identityRenderer) {
				source = strings.Replace(source, identityRenderer, parts.StreamSigner+"\trenderer, err := views.New(views.WithI18n(translator), views.WithStreams(streamSigner))\n", 1)
			} else {
				addOnce(parts.StreamSigner)
			}
		}
		addOnce(parts.Guard)
		addOnce(parts.PlatformGuard)
		addOnce(parts.CookieGuard)
		addOnce(parts.Renderer)
		addOnce(parts.StreamsRegister)
		for _, importPath := range parts.Imports {
			if !strings.Contains(source, `"`+importPath+`"`) && !strings.Contains(imports.String(), `"`+importPath+`"`) {
				imports.WriteString("\t\"" + importPath + "\"\n")
			}
		}
		usage.WriteString(parts.Usage)
	}
	source = strings.Replace(source, "\t\"net/http\"\n", "\t\"net/http\"\n"+imports.String(), 1)
	anchor := "\t// Pages behind a permission authenticate the session cookie"
	insertAfter := false
	if !strings.Contains(source, anchor) {
		anchor = "\tif err := routes(ctx, app, translator); err != nil {\n\t\treturn err\n\t}\n"
		insertAfter = true
	}
	if !strings.Contains(source, anchor) {
		// An API-only application (forge new --api) with a database.
		anchor = "\tif err := routes(ctx, app); err != nil {\n\t\treturn err\n\t}\n"
		insertAfter = true
	}
	if !strings.Contains(source, anchor) {
		// An application without a database: Configure only calls routes.
		anchor = "\treturn nil\n}\n\n// useBrowserPipeline"
		insertAfter = false
	}
	if !strings.Contains(source, anchor) {
		// An API-only application without a database.
		anchor = "\treturn nil\n}\n\ntype statusResponse"
		insertAfter = false
	}
	if !strings.Contains(source, anchor) {
		t.Fatalf("bootstrap.go has no wiring anchor:\n%s", source)
	}
	block := guard + "\n" + usage.String()
	if insertAfter {
		source = strings.Replace(source, anchor, anchor+block, 1)
	} else {
		source = strings.Replace(source, anchor, block+"\n"+anchor, 1)
	}
	source = strings.Replace(source, "\n\n\treturn nil\n}\n\n// useBrowserPipeline", "\n\treturn nil\n}\n\n// useBrowserPipeline", 1)
	formatted, err := format.Source([]byte(source))
	if err != nil {
		t.Fatalf("pasted wiring does not parse: %v\n%s", err, source)
	}
	writeFile(t, path, string(formatted))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDevForwardsInterruptToApplication is a regression test: `forge dev`
// used `go run`, which does not forward signals, so stopping forge left the
// application running.
func TestDevForwardsInterruptToApplication(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a program with the go toolchain")
	}
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/sig\n\ngo 1.27.1\n")
	writeFile(t, filepath.Join(root, "cmd", "sig", "main.go"), `package main

import (
	"fmt"
	"os"
	"os/signal"
)

func main() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	fmt.Println("ready")
	<-signals
	fmt.Println("graceful shutdown")
}
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- runApp(ctx, root, "sig", nil, os.Environ(), output, output) }()

	if !waittest.Poll(60*time.Second, func() bool { return strings.Contains(output.String(), "ready") }) {
		t.Fatalf("application never started: %s", output.String())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("dev: %v output=%s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("application kept running after cancellation")
	}
	if !strings.Contains(output.String(), "graceful shutdown") {
		t.Fatalf("application was not interrupted gracefully: %s", output.String())
	}
}

type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *syncBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func TestRunOpenAPIValidatesArgumentsBeforeBuilding(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--bogus"}, {"--output"}, {"--output", "a", "--check", "b"}, {"--output=a", "--check=b"}, {"extra"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(append([]string{"openapi"}, args...), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), openAPIUsage) {
			t.Errorf("forge openapi %v: code=%d stderr=%s", args, code, stderr.String())
		}
	}
}

// TestInvalidFlagsOfEverySubcommandExitWithStatus2 pins the exit status of a
// command line that does not parse: 2, like Forge-Rust, never 1 (audit N-D3).
// forge test, rollback and setup are left out: they forward their arguments to
// go test and to the application, which report their own failures.
func TestInvalidFlagsOfEverySubcommandExitWithStatus2(t *testing.T) {
	forwarding := map[string]bool{"test": true, "rollback": true, "setup": true}
	t.Chdir(t.TempDir())
	for _, command := range currentCommandCatalog().Commands {
		if forwarding[command.Name] {
			continue
		}
		var stdout, stderr bytes.Buffer
		if code := Run([]string{command.Name, "--bogus"}, &stdout, &stderr); code != 2 {
			t.Errorf("forge %s --bogus: exit %d, want 2 (stderr %q)", command.Name, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "forge: ") {
			t.Errorf("forge %s --bogus: stderr %q does not report the error", command.Name, stderr.String())
		}
	}
	// Subcommands of commands that have them, and a bad flag value.
	for _, args := range [][]string{
		{"new", "--bogus"}, {"new", "shop", "--bogus"}, {"new", "--database"}, {"new"}, {"uuid", "--count", "x"},
		{"db", "migrate", "--bogus"}, {"credentials", "show", "--bogus"}, {"generate", "migration", "x", "--bogus"},
		{"generate", "resource", "x", "--bogus"}, {"destroy", "model", "x", "--bogus"}, {"dev", "--poll-interval", "soon"},
		{"deploy", "--bogus"}, {"logs", "--bogus"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != 2 {
			t.Errorf("forge %s: exit %d, want 2 (stderr %q)", strings.Join(args, " "), code, stderr.String())
		}
	}
}

// TestInvalidFlagKeepsExitStatus2WithJSONErrors checks the machine-readable
// path reports the same status.
func TestInvalidFlagKeepsExitStatus2WithJSONErrors(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{jsonErrorFormatFlag, "new", "--bogus"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), `"exit_code":2`) {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}

// TestNewSkipLockfileIsTheForgeRustNameOfSkipDeps checks that Forge-Rust's
// --skip-lockfile is accepted with the meaning of --skip-deps: no go tool runs.
func TestNewSkipLockfileIsTheForgeRustNameOfSkipDeps(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "shop")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"new", "shop", "--skip-lockfile", "--dir", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "  run     go ") {
		t.Errorf("--skip-lockfile resolved dependencies: %s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "go.sum")); err == nil {
		t.Error("--skip-lockfile left a go.sum")
	}
}
