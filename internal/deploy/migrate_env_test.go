package deploy

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
)

const (
	// migrationSecret is the schema owner's credential of the fixtures.
	migrationSecret = "postgres://shop_migrator:migration-secret-4@db/shop"
	migrationName   = "FORGE_MIGRATION_DATABASE_URL"
)

// migrateEnvConfig is validConfig with the migrate step's own credential.
var migrateEnvConfig = validConfig + `migrate:
  env:
    secret: [FORGE_MIGRATION_DATABASE_URL]
`

// removeScript is what deletes the migrate env file and an interrupted
// upload's temporary copy.
const removeScript = "rm -f .forge/shop/env/migrate.step.env .forge/shop/env/migrate.step.env.tmp"

// stdinRunner records the stdin of every command next to the script.
type stdinRunner struct {
	*fakeRunner
	mu      sync.Mutex
	uploads []upload
	order   []string
}

type upload struct {
	host, script, stdin string
}

func (r *stdinRunner) Run(ctx context.Context, host string, cmd Command) (Result, error) {
	// Like the SSH runner, refuse to run once the context is done.
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	r.mu.Lock()
	r.order = append(r.order, host+": "+cmd.Script)
	if cmd.Stdin != nil {
		r.uploads = append(r.uploads, upload{host, cmd.Script, string(cmd.Stdin)})
	}
	r.mu.Unlock()
	return r.fakeRunner.Run(ctx, host, cmd)
}

func migrateEnvFixture(t *testing.T, dryRun bool, config string) (*Deployer, *stdinRunner, *bytes.Buffer) {
	t.Helper()
	d, runner, log, _ := planFixtureFor(t, dryRun, config, map[string]string{migrationName: migrationSecret})
	wrapped := &stdinRunner{fakeRunner: runner}
	d.Runner = wrapped
	return d, wrapped, log
}

func TestMigrateEnvReachesOnlyTheMigrateContainer(t *testing.T) {
	d, runner, _ := migrateEnvFixture(t, false, migrateEnvConfig)
	if _, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: true}); err != nil {
		t.Fatal(err)
	}
	var migrateUploads []upload
	for _, u := range runner.uploads {
		switch {
		case strings.Contains(u.script, "migrate.step.env"):
			migrateUploads = append(migrateUploads, u)
		case strings.Contains(u.script, "/env/web.env") || strings.Contains(u.script, "/env/jobs.env"):
			if strings.Contains(u.stdin, migrationSecret) || strings.Contains(u.stdin, migrationName) {
				t.Fatalf("serving env file leaks the migration credential: %s", u.script)
			}
		}
	}
	if len(migrateUploads) != 1 {
		t.Fatalf("migrate env uploaded %d times, want once: %+v", len(migrateUploads), migrateUploads)
	}
	up := migrateUploads[0]
	if up.host != "192.0.2.10" {
		t.Fatalf("migrate env went to %s, want the migrate host only", up.host)
	}
	if want := "umask 077 && mkdir -p .forge/shop/env && cat > .forge/shop/env/migrate.step.env.tmp && mv .forge/shop/env/migrate.step.env.tmp .forge/shop/env/migrate.step.env"; up.script != want {
		t.Fatalf("upload script = %s", up.script)
	}
	// The role's own environment plus the credential, in one file.
	if want := "FORGE_ENV=production\nFORGE_DATABASE_URL=postgres://shop:db-secret-2@db/shop\n" + migrationName + "=" + migrationSecret + "\n"; up.stdin != want {
		t.Fatalf("migrate env content = %q, want %q", up.stdin, want)
	}

	upIndex, runIndex, rmIndex := -1, -1, -1
	for i, call := range runner.order {
		switch {
		case strings.Contains(call, "migrate.step.env") && strings.Contains(call, "cat >"):
			upIndex = i
		case strings.Contains(call, "docker run --rm") && strings.Contains(call, "migrate.step.env"):
			runIndex = i
			if !strings.HasPrefix(call, "192.0.2.10: ") || strings.Contains(call, "web.env") || strings.Count(call, "--env-file") != 1 {
				t.Fatalf("migrate command = %s", call)
			}
		case strings.HasSuffix(call, ": "+removeScript):
			rmIndex = i
		case strings.Contains(call, "migrate.step.env"):
			t.Fatalf("another command uses the migrate env file: %s", call)
		}
		if strings.Contains(call, migrationSecret) || strings.Contains(call, "migration-secret-4") {
			t.Fatalf("a command line carries the credential: %s", call)
		}
		if strings.Contains(call, "docker run --detach") && strings.Contains(call, "migrate") {
			t.Fatalf("a serving container gets migrate settings: %s", call)
		}
	}
	if upIndex < 0 || runIndex < 0 || rmIndex < 0 || upIndex > runIndex || runIndex > rmIndex {
		t.Fatalf("want upload (%d) < migrate (%d) < remove (%d)", upIndex, runIndex, rmIndex)
	}
}

func TestMigrateEnvIsRemovedWhenTheMigrationFails(t *testing.T) {
	d, runner, _ := migrateEnvFixture(t, false, migrateEnvConfig)
	runner.fail = func(_ string, cmd Command) error {
		if strings.Contains(cmd.Script, "docker run --rm") {
			return &RemoteError{Host: "192.0.2.10", ExitCode: 1, Stderr: "migration 0002 failed using " + migrationSecret}
		}
		return nil
	}
	report, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: true})
	if ExitCodeOf(err) != ExitMigrationFailed {
		t.Fatalf("err = %v (exit %d)", err, ExitCodeOf(err))
	}
	if strings.Contains(err.Error(), migrationSecret) || strings.Contains(err.Error(), "migration-secret-4") {
		t.Fatalf("error leaks the credential: %v", err)
	}
	if len(report.Warnings) != 0 {
		t.Fatalf("warnings = %v", report.Warnings)
	}
	removed := false
	for _, call := range runner.order {
		removed = removed || call == "192.0.2.10: "+removeScript
		if strings.Contains(call, "docker run --detach") {
			t.Fatalf("a container was started after a failed migration: %s", call)
		}
	}
	if !removed {
		t.Fatal("the credential file was left behind after a failed migration")
	}
}

func TestMigrateEnvRemovalFailureIsAWarning(t *testing.T) {
	d, runner, _ := migrateEnvFixture(t, false, migrateEnvConfig)
	runner.fail = func(_ string, cmd Command) error {
		if cmd.Script == removeScript {
			return errors.New("ssh: connection reset")
		}
		return nil
	}
	report, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "delete it by hand") || !strings.Contains(report.Warnings[0], ".forge/shop/env/migrate.step.env") {
		t.Fatalf("warnings = %v", report.Warnings)
	}
}

func TestMigrateEnvUploadFailureAbortsBeforeMigratingAndStillRemovesTheFile(t *testing.T) {
	d, runner, _ := migrateEnvFixture(t, false, migrateEnvConfig)
	runner.fail = func(_ string, cmd Command) error {
		// The remote command may already have run when the error arrives
		// (Ctrl-C, a dropped connection).
		if strings.Contains(cmd.Script, "cat > .forge/shop/env/migrate.step.env.tmp") {
			return errors.New("disk full")
		}
		return nil
	}
	_, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: true})
	if ExitCodeOf(err) != ExitMigrationFailed || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v (exit %d)", err, ExitCodeOf(err))
	}
	removed := false
	for _, call := range runner.order {
		if strings.Contains(call, "docker run") {
			t.Fatalf("a container ran after the upload failed: %s", call)
		}
		removed = removed || call == "192.0.2.10: "+removeScript
	}
	if !removed {
		t.Fatalf("the interrupted upload's files were not removed on the migrate host: %v", runner.order)
	}
}

func TestMigrateEnvIsRemovedWhenTheContextIsCancelledDuringTheMigration(t *testing.T) {
	d, runner, _ := migrateEnvFixture(t, false, migrateEnvConfig)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner.fail = func(_ string, cmd Command) error {
		if strings.Contains(cmd.Script, "docker run --rm") {
			cancel()
			return context.Canceled
		}
		return nil
	}
	_, err := d.Deploy(ctx, DeployOptions{Version: "v3", SkipBuild: true})
	if ExitCodeOf(err) != ExitMigrationFailed {
		t.Fatalf("err = %v (exit %d)", err, ExitCodeOf(err))
	}
	if !slices.Contains(runner.order, "192.0.2.10: "+removeScript) {
		t.Fatalf("the file was not removed after cancellation: %v", runner.order)
	}
}

func TestMissingMigrateSecretFailsClosedBeforeAnyRemoteCommand(t *testing.T) {
	d, runner, _ := migrateEnvFixture(t, false, migrateEnvConfig)
	d.Secrets = NewSecrets(map[string]string{
		"REGISTRY_PASSWORD": "registry-secret-1", "FORGE_DATABASE_URL": "postgres://shop:db-secret-2@db/shop", "POSTGRES_PASSWORD": "pg-secret-3",
	})
	_, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: true})
	if err == nil || !strings.Contains(err.Error(), "secrets not defined in .forge/secrets: "+migrationName) {
		t.Fatalf("err = %v", err)
	}
	if len(runner.order) != 0 {
		t.Fatalf("commands ran before the secret check: %v", runner.order)
	}
}

func TestDryRunWithMigrateEnvShowsNoValue(t *testing.T) {
	d, runner, log := migrateEnvFixture(t, true, migrateEnvConfig)
	if _, err := d.Deploy(t.Context(), DeployOptions{Version: "v3"}); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "deploy-migrate-env", log.String())
	for _, u := range runner.uploads {
		if strings.Contains(u.script, "migrate.step.env") {
			t.Fatalf("dry run uploaded the migrate env file: %+v", u)
		}
	}
}

func TestDeployWithoutMigrateEnvKeepsTheRoleEnvFile(t *testing.T) {
	d, runner, _ := migrateEnvFixture(t, false, validConfig)
	if _, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: true}); err != nil {
		t.Fatal(err)
	}
	var migrate []string
	for _, call := range runner.order {
		if strings.Contains(call, "migrate.step.env") || strings.Contains(call, ": rm -f .forge/shop/env/") {
			t.Fatalf("a deploy without migrate.env touches %s", call)
		}
		if strings.Contains(call, "docker run --rm") {
			migrate = append(migrate, call)
		}
	}
	if len(migrate) != 1 || !strings.Contains(migrate[0], "--env-file .forge/shop/env/web.env ") {
		t.Fatalf("migrate commands = %v", migrate)
	}
}

func TestMigrateEnvDisabledMigrationIsRejected(t *testing.T) {
	_, err := ParseConfig([]byte(validConfig + "migrate:\n  enabled: false\n  env:\n    secret: [X]\n"))
	if err == nil || !strings.Contains(err.Error(), "migrate.env is set but migrate.enabled is false") {
		t.Fatalf("err = %v", err)
	}
}

func TestMigrateEnvAddsToTheMigrateRoleEnvironment(t *testing.T) {
	config := validConfig + `migrate:
  role: jobs
  env:
    clear:
      MIGRATION_LOCK_TIMEOUT: 5m
    secret: [FORGE_MIGRATION_DATABASE_URL]
`
	cfg, err := ParseConfig([]byte(config))
	if err != nil {
		t.Fatal(err)
	}
	clear, secret := commands{cfg: cfg}.migrateEnv()
	// The global env and the migrate role's env, plus the overlay.
	if clear["WORKERS"] != "4" || clear["FORGE_ENV"] != "production" || clear["MIGRATION_LOCK_TIMEOUT"] != "5m" || len(clear) != 3 {
		t.Fatalf("clear = %v", clear)
	}
	if !slices.Equal(secret, []string{"FORGE_DATABASE_URL", migrationName}) {
		t.Fatalf("secret = %v", secret)
	}
	// The serving roles are untouched.
	roleClear, roleSecret := commands{cfg: cfg}.roleEnv("web")
	if len(roleClear) != 1 || roleClear["FORGE_ENV"] != "production" || !slices.Equal(roleSecret, []string{"FORGE_DATABASE_URL"}) {
		t.Fatalf("role env changed: %v %v", roleClear, roleSecret)
	}
}

func TestMigrateEnvNamesMayNotAppearInAnyOtherEnvBlock(t *testing.T) {
	cases := map[string]struct {
		config string
		want   string
	}{
		"global secret": {
			strings.Replace(migrateEnvConfig, "secret: [FORGE_DATABASE_URL]", "secret: [FORGE_DATABASE_URL, FORGE_MIGRATION_DATABASE_URL]", 1),
			"FORGE_MIGRATION_DATABASE_URL is in migrate.env and in env.secret: remove it from env.secret, so only the migrate step receives it",
		},
		"global clear": {
			strings.Replace(migrateEnvConfig, "FORGE_ENV: production", "FORGE_ENV: production\n    FORGE_MIGRATION_DATABASE_URL: x", 1),
			"in migrate.env and in env.clear",
		},
		"role secret": {
			strings.Replace(migrateEnvConfig, "      clear:\n        WORKERS", "      secret: [FORGE_MIGRATION_DATABASE_URL]\n      clear:\n        WORKERS", 1),
			"in migrate.env and in servers.jobs.env.secret",
		},
		"role clear": {
			strings.Replace(migrateEnvConfig, `WORKERS: "4"`, `WORKERS: "4"`+"\n        FORGE_MIGRATION_DATABASE_URL: x", 1),
			"in migrate.env and in servers.jobs.env.clear",
		},
		"clear in migrate and global": {
			validConfig + "migrate:\n  env:\n    clear: {FORGE_ENV: migration}\n",
			"FORGE_ENV is in migrate.env and in env.clear",
		},
		"web role not migrate role": {
			strings.Replace(migrateEnvConfig, "    hosts: [192.0.2.10, 192.0.2.11]", "    hosts: [192.0.2.10, 192.0.2.11]\n    env: {secret: [FORGE_MIGRATION_DATABASE_URL]}", 1),
			"in migrate.env and in servers.web.env.secret",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.config))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestExplicitlyNullMigrateEnvIsRejected(t *testing.T) {
	for name, block := range map[string]string{
		"empty value": "migrate:\n  env:\n",
		"tilde":       "migrate:\n  env: ~\n",
		"null":        "migrate:\n  env: null\n",
		"flow null":   "migrate: {enabled: true, env: null}\n",
		"capital":     "migrate:\n  env: Null\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte(validConfig + block))
			if err == nil || !strings.Contains(err.Error(), "migrate.env must set at least one clear value or secret") {
				t.Fatalf("err = %v", err)
			}
		})
	}
	// A migrate block without env, or with other null fields, is still valid.
	for _, block := range []string{"migrate:\n  enabled: true\n", "migrate:\n  role:\n"} {
		if _, err := ParseConfig([]byte(validConfig + block)); err != nil {
			t.Fatalf("%q: %v", block, err)
		}
	}
}

func TestMigrateEnvSecretsAreRequiredAndListed(t *testing.T) {
	cfg, err := ParseConfig([]byte(migrateEnvConfig))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.SecretNames(), migrationName) {
		t.Fatalf("SecretNames = %v", cfg.SecretNames())
	}
	cfg, err = ParseConfig([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Migrate.Env != nil || slices.Contains(cfg.SecretNames(), migrationName) {
		t.Fatalf("a configuration without migrate.env gained one: %+v", cfg.Migrate)
	}
}

func TestInitTemplateMigrateEnvExampleIsValidWhenUncommented(t *testing.T) {
	const commented = "  # env:\n  #   secret:\n  #     - FORGE_MIGRATION_DATABASE_URL\n"
	if !strings.Contains(ConfigTemplate, commented) {
		t.Fatal("the deploy init template lacks the commented migrate.env example")
	}
	cfg, err := ParseConfig([]byte(strings.ReplaceAll(strings.Replace(ConfigTemplate, commented, "  env:\n    secret:\n      - FORGE_MIGRATION_DATABASE_URL\n", 1), "SERVICE", "shop")))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Migrate.Env == nil || !slices.Equal(cfg.Migrate.Env.Secret, []string{migrationName}) {
		t.Fatalf("migrate.env = %+v", cfg.Migrate.Env)
	}
}

func TestExecKeepsTheRoleEnvironment(t *testing.T) {
	cfg, err := ParseConfig([]byte(migrateEnvConfig))
	if err != nil {
		t.Fatal(err)
	}
	script := commands{cfg: cfg}.execOneOff("web", []string{"migrate", "status"}, false)
	if !strings.Contains(script, "--env-file .forge/shop/env/web.env") || strings.Contains(script, "migrate.step.env") {
		t.Fatalf("exec script = %s", script)
	}
}
