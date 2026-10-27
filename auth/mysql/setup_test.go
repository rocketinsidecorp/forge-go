package mysql_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/auth"
	authmysql "github.com/rocketinsidecorp/forge-go/auth/mysql"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/dbtest"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/mysql"
	"github.com/rocketinsidecorp/forge-go/mysql/mysqltest"
)

const ownerPassword = "correct horse battery staple"

// TestSingleTenantSetupSignsInWithoutAnOrganization is decision D33 on
// SQLite: setup --email creates the hidden default organization, and a
// sign-in that names no organization reaches it.
func TestSingleTenantSetupSignsInWithoutAnOrganization(t *testing.T) {
	t.Parallel()
	cfg := mysqltest.Config(t)
	dbtest.Migrate(t, authmysql.Migrations(), func(loaded []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
		return mysql.NewMigrator(cfg, loaded, logger)
	})
	setup := func(options []authmysql.SetupOption, args ...string) (string, error) {
		var stdout bytes.Buffer
		err := authmysql.SetupCommand(options...).Run(context.Background(), forge.CommandEnv{
			Config: config.Config{Database: cfg}, Stdout: &stdout, Stderr: &stdout,
			Stdin:     strings.NewReader(ownerPassword + "\n"),
			LookupEnv: func(string) (string, bool) { return "", false },
		}, args)
		return stdout.String(), err
	}
	if _, err := setup(nil, "--email", "owner@example.com", "--password-stdin"); !errors.Is(err, forge.ErrUsage) {
		t.Fatalf("setup without --org in an application with organizations: %v", err)
	}
	single := []authmysql.SetupOption{authmysql.SetupOrganizations(false), nil}
	output, err := setup(single, "--email", "owner@example.com", "--password-stdin")
	if err != nil || !strings.Contains(output, "Sign in at /session/new.") {
		t.Fatalf("setup = %q %v", output, err)
	}
	if _, err := setup(single, "--email", "second@example.com", "--password-stdin"); !errors.Is(err, authmysql.ErrAlreadyProvisioned) {
		t.Fatalf("second setup: %v", err)
	}

	db, err := mysql.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository, err := authmysql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	login, err := auth.NewLoginService(repository, repository, auth.WithSingleTenant(auth.DefaultTenantSlug))
	if err != nil {
		t.Fatal(err)
	}
	result, err := login.Login(context.Background(), auth.LoginInput{Email: "owner@example.com", Password: ownerPassword, Source: "192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	if session, found, err := repository.ResolveSession(context.Background(), result.Token.Digest()); err != nil || !found || session.Principal.Tenant().ID.Version() != 7 {
		t.Fatalf("session found=%v err=%v", found, err)
	}
	if repository.SessionIdleTimeout() != authmysql.DefaultSessionIdleTimeout {
		t.Fatal("default idle timeout")
	}
	if _, err := authmysql.NewRecoveryThrottler(db); err != nil {
		t.Fatal(err)
	}
}

// TestSetupFailsOnAnUnusableDatabase covers the open failure.
func TestSetupFailsOnAnUnusableDatabase(t *testing.T) {
	t.Parallel()
	cfg := mysqltest.Config(t)
	cfg.URL = config.NewSecret("postgres://nobody@127.0.0.1:1/none") // not a MySQL URL
	err := authmysql.SetupCommand(authmysql.SetupOrganizations(false)).Run(context.Background(), forge.CommandEnv{
		Config: config.Config{Database: cfg}, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		Stdin:     strings.NewReader(ownerPassword + "\n"),
		LookupEnv: func(string) (string, bool) { return "", false },
	}, []string{"--email", "owner@example.com", "--password-stdin"})
	if err == nil {
		t.Fatal("setup succeeded without a usable database")
	}
	if _, err := authmysql.New(nil); err == nil {
		t.Fatal("New(nil) succeeded")
	}
	if _, err := authmysql.NewLoginThrottler(nil, auth.DefaultLoginThrottleConfig()); err == nil {
		t.Fatal("NewLoginThrottler(nil) succeeded")
	}
}
