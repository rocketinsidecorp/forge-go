package postgres_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/auth"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/dbtest"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

const ownerPassword = "correct horse battery staple"

func TestProvisionOwnerCreatesAnOwnerWhoCanSignIn(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repository, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := db.Exec(ctx, "INSERT INTO forge_permissions (name) VALUES ('tasks:read'), ('tasks:write')"); err != nil {
		t.Fatal(err)
	}

	owner, err := repository.ProvisionOwner(ctx, authpostgres.OwnerAccount{
		Organization: " Acme Inc ", TenantSlug: "Acme", Email: " Owner@Example.com ", Password: ownerPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []struct {
		name  string
		value interface{ Version() byte }
	}{{"organization", owner.OrganizationID}, {"tenant", owner.TenantID}, {"user", owner.UserID}, {"membership", owner.MembershipID}, {"role", owner.RoleID}} {
		if id.value.Version() != 7 {
			t.Fatalf("%s ID is not a UUIDv7", id.name)
		}
	}
	slices.Sort(owner.Permissions)
	if owner.TenantSlug != "acme" || owner.Email != "Owner@Example.com" || !slices.Equal(owner.Permissions, []string{"tasks:read", "tasks:write"}) {
		t.Fatalf("owner=%+v", owner)
	}
	var storedHash, organization string
	if err := db.QueryRow(ctx, "SELECT u.password_hash, o.name FROM forge_users u, forge_organizations o WHERE u.id = $1 AND o.id = $2", owner.UserID, owner.OrganizationID).
		Scan(&storedHash, &organization); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(storedHash, ownerPassword) || !strings.HasPrefix(storedHash, "$"+string(auth.DefaultPasswordAlgorithm())+"$") || organization != "Acme Inc" {
		t.Fatalf("hash=%q organization=%q", storedHash, organization)
	}

	// The owner signs in through the normal login service and holds every
	// permission through the owner role.
	login, err := auth.NewLoginService(repository, repository)
	if err != nil {
		t.Fatal(err)
	}
	result, err := login.Login(ctx, auth.LoginInput{Email: "owner@example.com", Password: ownerPassword, TenantSlug: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	session, found, err := repository.ResolveSession(ctx, result.Token.Digest())
	if err != nil || !found {
		t.Fatalf("resolve: found=%v err=%v", found, err)
	}
	if session.Principal.Tenant().ID != owner.TenantID || !session.Principal.Can("tasks:read") || !session.Principal.Can("tasks:write") {
		t.Fatalf("principal permissions=%v", session.Principal.Permissions())
	}

	var kind, outcome string
	if err := db.QueryRow(ctx, "SELECT kind, outcome FROM forge_security_audit_events WHERE subject_id = $1 AND membership_id = $2", owner.UserID, owner.MembershipID).
		Scan(&kind, &outcome); err != nil {
		t.Fatal(err)
	}
	if kind != string(auth.SecurityIdentityProvisioned) || outcome != "succeeded" {
		t.Fatalf("audit kind=%q outcome=%q", kind, outcome)
	}

	// Setup never runs twice, whatever the input.
	for _, account := range []authpostgres.OwnerAccount{
		{Organization: "Acme Inc", TenantSlug: "acme", Email: "owner@example.com", Password: ownerPassword},
		{Organization: "Other", TenantSlug: "other", Email: "intruder@example.com", Password: ownerPassword},
	} {
		if _, err := repository.ProvisionOwner(ctx, account); !errors.Is(err, authpostgres.ErrAlreadyProvisioned) {
			t.Fatalf("second setup for %s: %v", account.TenantSlug, err)
		}
	}
	var tenants, users, events int
	if err := db.QueryRow(ctx, "SELECT (SELECT count(*) FROM forge_tenants), (SELECT count(*) FROM forge_users), (SELECT count(*) FROM forge_security_audit_events WHERE kind = 'auth.identity.provisioned')").
		Scan(&tenants, &users, &events); err != nil {
		t.Fatal(err)
	}
	if tenants != 1 || users != 1 || events != 1 {
		t.Fatalf("after refused reruns: tenants=%d users=%d events=%d", tenants, users, events)
	}
}

func TestProvisionOwnerIsAtomicAndSerialized(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repository, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// A user with the owner's email aborts the whole transaction.
	if _, err := db.Exec(ctx, "INSERT INTO forge_users (id, email, email_normalized, password_hash) VALUES ($1, 'taken@example.com', 'taken@example.com', 'x')", uuid.MustNew()); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ProvisionOwner(ctx, authpostgres.OwnerAccount{Organization: "Acme", TenantSlug: "acme", Email: "Taken@example.com", Password: ownerPassword}); !errors.Is(err, authpostgres.ErrOwnerEmailTaken) {
		t.Fatalf("taken email: %v", err)
	}
	var organizations int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM forge_organizations").Scan(&organizations); err != nil || organizations != 0 {
		t.Fatalf("a failed setup left %d organizations (err=%v)", organizations, err)
	}

	for _, invalid := range []authpostgres.OwnerAccount{
		{Organization: "Acme", TenantSlug: "a", Email: "owner@example.com", Password: ownerPassword},
		{Organization: "Acme", TenantSlug: "acme", Email: "not-an-email", Password: ownerPassword},
		{Organization: "", TenantSlug: "acme", Email: "owner@example.com", Password: ownerPassword},
		{Organization: "Ac\x00me", TenantSlug: "acme", Email: "owner@example.com", Password: ownerPassword},
		{Organization: "Acme", TenantSlug: "acme", Email: "owner@example.com", Password: ""},
		// A non-ASCII login could never sign in (auth.CanonicalLogin), so it is
		// refused before anything is stored.
		{Organization: "Acme", TenantSlug: "acme", Email: "owner@\u0130cloud.com", Password: ownerPassword},
		{Organization: "Acme", TenantSlug: "acme", Email: "\u212aelvin@example.com", Password: ownerPassword},
	} {
		if _, err := repository.ProvisionOwner(ctx, invalid); err == nil {
			t.Fatalf("invalid account accepted: %+v", invalid)
		}
	}

	var wait sync.WaitGroup
	results := make([]error, 4)
	for i := range results {
		wait.Go(func() {
			_, results[i] = repository.ProvisionOwner(ctx, authpostgres.OwnerAccount{
				Organization: "Acme", TenantSlug: "tenant-" + string(rune('a'+i)), Email: "owner" + string(rune('a'+i)) + "@example.com", Password: ownerPassword,
			})
		})
	}
	wait.Wait()
	succeeded := 0
	for _, err := range results {
		switch {
		case err == nil:
			succeeded++
		case !errors.Is(err, authpostgres.ErrAlreadyProvisioned):
			t.Fatalf("concurrent setup: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d concurrent setups succeeded, want exactly 1", succeeded)
	}
}

func TestSetupCommandReadsThePasswordSafely(t *testing.T) {
	db, database := postgrestest.NewWithConfig(t)
	dbtest.Migrate(t, authpostgres.Migrations(), func(loaded []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
		return postgres.NewMigrator(database, loaded, logger)
	})
	cfg := config.Default()
	cfg.Database = database
	command := authpostgres.SetupCommand()
	if command.Name != "setup" || command.Run == nil {
		t.Fatalf("command=%+v", command)
	}
	run := func(args []string, stdin io.Reader, environment map[string]string) (string, string, error) {
		var stdout, stderr bytes.Buffer
		err := command.Run(context.Background(), forge.CommandEnv{
			Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Stdout: &stdout, Stderr: &stderr, Stdin: stdin,
			LookupEnv: func(key string) (string, bool) { value, ok := environment[key]; return value, ok },
		}, args)
		return stdout.String(), stderr.String(), err
	}
	valid := []string{"--tenant", "acme", "--email", "owner@example.com", "--organization", "Acme Inc"}

	for _, current := range []struct {
		name        string
		args        []string
		stdin       io.Reader
		environment map[string]string
	}{
		{"no flags", nil, nil, nil},
		{"missing email", []string{"--tenant", "acme"}, nil, nil},
		{"password flag", append(slices.Clone(valid), "--password", ownerPassword), nil, nil},
		{"positional password", append(slices.Clone(valid), ownerPassword), nil, nil},
		{"invalid tenant", []string{"--tenant", "A B", "--email", "owner@example.com"}, nil, nil},
		{"non-ASCII email", []string{"--tenant", "acme", "--email", "owner@\u0130cloud.com"}, nil, nil},
		{"no password source", valid, strings.NewReader(ownerPassword + "\n"), nil},
		{"two password sources", append(slices.Clone(valid), "--password-stdin"), strings.NewReader(ownerPassword + "\n"), map[string]string{authpostgres.SetupPasswordVariable: ownerPassword}},
	} {
		if _, _, err := run(current.args, current.stdin, current.environment); !errors.Is(err, forge.ErrUsage) {
			t.Errorf("%s: err=%v, want a usage error", current.name, err)
		}
	}
	if _, _, err := run(append(slices.Clone(valid), "--password-stdin"), strings.NewReader("too short\n"), nil); err == nil || errors.Is(err, forge.ErrUsage) {
		t.Fatalf("short password: %v", err)
	}

	stdout, stderr, err := run(append(slices.Clone(valid), "--password-stdin"), strings.NewReader(ownerPassword+"\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout+stderr, ownerPassword) || !strings.Contains(stdout, `sign-in slug "acme"`) || !strings.Contains(stdout, "/session/new") {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
	repository, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	login, err := auth.NewLoginService(repository, repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := login.Login(context.Background(), auth.LoginInput{Email: "owner@example.com", Password: ownerPassword, TenantSlug: "acme"}); err != nil {
		t.Fatalf("the provisioned owner cannot sign in: %v", err)
	}

	_, _, err = run(valid, nil, map[string]string{authpostgres.SetupPasswordVariable: ownerPassword + "-other"})
	if !errors.Is(err, authpostgres.ErrAlreadyProvisioned) {
		t.Fatalf("second setup: %v", err)
	}
}

// In an application without organizations (forge new --auth) setup takes
// --email only, refuses --org, and creates the hidden default organization
// a sign-in without one resolves to.
func TestSingleOrganizationSetupCreatesTheDefaultOrganization(t *testing.T) {
	db, database := postgrestest.NewWithConfig(t)
	dbtest.Migrate(t, authpostgres.Migrations(), func(loaded []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
		return postgres.NewMigrator(database, loaded, logger)
	})
	cfg := config.Default()
	cfg.Database = database
	command := authpostgres.SetupCommand(nil, authpostgres.SetupOrganizations(false))
	run := func(args ...string) (string, error) {
		var stdout bytes.Buffer
		err := command.Run(context.Background(), forge.CommandEnv{
			Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Stdout: &stdout, Stderr: io.Discard, Stdin: strings.NewReader(""),
			LookupEnv: func(key string) (string, bool) {
				return ownerPassword, key == authpostgres.SetupPasswordVariable
			},
		}, args)
		return stdout.String(), err
	}
	if _, err := run("--org", "acme", "--email", "owner@example.com"); !errors.Is(err, forge.ErrUsage) || !strings.Contains(err.Error(), "takes no --org") {
		t.Fatalf("--org without organizations: %v", err)
	}
	stdout, err := run("--email", "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "slug") || !strings.Contains(stdout, "Sign in at /session/new.") {
		t.Fatalf("stdout=%q", stdout)
	}
	repository, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	login, err := auth.NewLoginService(repository, repository, auth.WithSingleTenant(auth.DefaultTenantSlug))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := login.Login(context.Background(), auth.LoginInput{Email: "owner@example.com", Password: ownerPassword}); err != nil {
		t.Fatalf("the owner cannot sign in without an organization: %v", err)
	}
}
