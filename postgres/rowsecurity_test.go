package postgres_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// rlsMigrations is a tenant table under a fail-closed FORCE policy, as the
// resource generator writes it.
var rlsMigrations = fstest.MapFS{
	"20260101000000_notes.up.sql": {Data: []byte(`
CREATE TABLE notes (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    body text NOT NULL
);
ALTER TABLE notes ENABLE ROW LEVEL SECURITY;
ALTER TABLE notes FORCE ROW LEVEL SECURITY;
CREATE POLICY notes_tenant_isolation ON notes
    USING (tenant_id::text = current_setting('forge.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('forge.tenant_id', true));
`)},
	"20260101000000_notes.down.sql": {Data: []byte("DROP TABLE notes;")},
}

func tenantContext(t *testing.T) (tenancy.Tenant, context.Context) {
	t.Helper()
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := tenancy.WithContext(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	return tenant, ctx
}

func insertNote(t *testing.T, db *postgres.DB, ctx context.Context, tenant tenancy.Tenant) {
	t.Helper()
	err := db.InTenantTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO notes (id, tenant_id, body) VALUES ($1, $2, 'x')", uuid.MustNew(), tenant.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func countNotes(t *testing.T, db *postgres.DB, ctx context.Context, inTenant bool) int {
	t.Helper()
	var count int
	run := db.InTx
	if inTenant {
		run = db.InTenantTx
	}
	if err := run(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM notes").Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func insufficientPrivilege(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42501"
}

// The runtime role of NewRLS is what an application serves with: row-level
// security confines it to the transaction's tenant even with no tenant
// predicate in the SQL, it sees nothing without a tenant, and it holds no
// DDL privilege with which to switch the policy off.
func TestRuntimeRoleIsConfinedByRowLevelSecurity(t *testing.T) {
	db := postgrestest.NewRLS(t, rlsMigrations)
	tenantA, ctxA := tenantContext(t)
	tenantB, ctxB := tenantContext(t)
	insertNote(t, db.Runtime, ctxA, tenantA)
	insertNote(t, db.Runtime, ctxB, tenantB)
	insertNote(t, db.Runtime, ctxB, tenantB)

	if got := countNotes(t, db.Runtime, ctxA, true); got != 1 {
		t.Errorf("tenant A sees %d rows, want only its own 1", got)
	}
	if got := countNotes(t, db.Runtime, ctxB, true); got != 2 {
		t.Errorf("tenant B sees %d rows, want only its own 2", got)
	}
	if got := countNotes(t, db.Runtime, context.Background(), false); got != 0 {
		t.Errorf("without a tenant the runtime role sees %d rows, want none", got)
	}
	// The owner is subject to the policy too (FORCE), and the admin
	// connection, a superuser, is not: it sees all three.
	if got := countNotes(t, db.Migrator, ctxA, true); got != 1 {
		t.Errorf("the owner sees %d rows as tenant A, want 1", got)
	}
	if got := countNotes(t, db.Admin, context.Background(), false); got != 3 {
		t.Errorf("the admin sees %d rows, want 3", got)
	}

	err := db.Runtime.InTenantTx(ctxA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctxA, "INSERT INTO notes (id, tenant_id, body) VALUES ($1, $2, 'x')", uuid.MustNew(), tenantB.ID)
		return err
	})
	if !insufficientPrivilege(err) {
		t.Errorf("tenant A inserting a row for tenant B: err = %v, want a row-level security violation (42501)", err)
	}

	ctx := context.Background()
	for _, ddl := range []string{
		"CREATE TABLE intruder (id int)",
		"ALTER TABLE notes DISABLE ROW LEVEL SECURITY",
		"ALTER TABLE notes NO FORCE ROW LEVEL SECURITY",
		"DROP POLICY notes_tenant_isolation ON notes",
		"DROP TABLE notes",
		"TRUNCATE notes",
	} {
		if _, err := db.Runtime.Exec(ctx, ddl); !insufficientPrivilege(err) {
			t.Errorf("runtime role ran %q: err = %v, want insufficient privilege (42501)", ddl, err)
		}
	}
	if got := countNotes(t, db.Admin, ctx, false); got != 3 {
		t.Errorf("after the refused statements the table holds %d rows, want 3", got)
	}
}

// actingAs returns cfg with a role setting in its URL (options=-c role=...):
// the session logs in as cfg's user and then acts as role.
func actingAs(t *testing.T, cfg config.Database, role string) config.Database {
	t.Helper()
	parsed, err := url.Parse(cfg.URL.Reveal())
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	query := parsed.Query()
	query.Set("options", "-c role="+role)
	parsed.RawQuery = strings.ReplaceAll(query.Encode(), "+", "%20")
	cfg.URL = config.NewSecret(parsed.String())
	return cfg
}

func TestRequireRowSecurityRefusesBypassingRolesOutsideDevelopmentAndTest(t *testing.T) {
	db := postgrestest.NewRLS(t, rlsMigrations)
	ctx := context.Background()
	open := func(cfg config.Database, environment config.Environment) error {
		pool, err := postgres.Open(ctx, cfg, postgres.RequireRowSecurity(environment))
		if err == nil {
			pool.Close()
		}
		return err
	}
	refused := func(name string, err error, reason string) {
		t.Helper()
		if !errors.Is(err, postgres.ErrRowSecurityBypass) || !strings.Contains(err.Error(), reason) ||
			!strings.Contains(err.Error(), "FORGE_DATABASE_URL") {
			t.Errorf("%s: err = %v, want ErrRowSecurityBypass naming %q", name, err, reason)
		}
	}
	served := []config.Environment{config.Staging, config.Production}

	for _, environment := range served {
		if err := open(db.RuntimeConfig, environment); err != nil {
			t.Fatalf("the runtime role in %s: %v", environment, err)
		}
	}

	var superuser bool
	if err := db.Admin.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&superuser); err != nil {
		t.Fatal(err)
	}
	admin := postgrestest.Config(t) // the test server's own user
	if superuser {
		for _, environment := range served {
			refused("superuser in "+string(environment), open(admin, environment), "is a superuser")
		}
		// Logged in as the superuser but acting as the runtime role:
		// current_user alone looks harmless, SET ROLE NONE would not be.
		refused("superuser acting as the runtime role", open(actingAs(t, admin, strings.Trim(db.RuntimeRole, `"`)), config.Production), "is a superuser")
	}
	for _, environment := range []config.Environment{config.Development, config.Test} {
		if err := open(admin, environment); err != nil {
			t.Errorf("the test server's user in %s: %v", environment, err)
		}
	}

	exec := func(statement string) {
		t.Helper()
		if _, err := db.Admin.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	exec("ALTER ROLE " + db.RuntimeRole + " BYPASSRLS")
	refused("BYPASSRLS in production", open(db.RuntimeConfig, config.Production), "has BYPASSRLS")
	if err := open(db.RuntimeConfig, config.Development); err != nil {
		t.Errorf("BYPASSRLS in development: %v", err)
	}
	exec("ALTER ROLE " + db.RuntimeRole + " NOBYPASSRLS")

	exec("ALTER ROLE " + db.RuntimeRole + " CREATEROLE")
	refused("CREATEROLE in staging", open(db.RuntimeConfig, config.Staging), "has CREATEROLE")
	exec("ALTER ROLE " + db.RuntimeRole + " NOCREATEROLE")

	bypass := pgx.Identifier{"forge_test_bypass_" + strings.ReplaceAll(uuid.MustNew().String(), "-", "")}.Sanitize()
	exec("CREATE ROLE " + bypass + " NOLOGIN BYPASSRLS")
	t.Cleanup(func() { _, _ = db.Admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+bypass) })
	exec("GRANT " + bypass + " TO " + db.RuntimeRole)
	refused("member of a BYPASSRLS role in production", open(db.RuntimeConfig, config.Production), "can SET ROLE")
	exec("REVOKE " + bypass + " FROM " + db.RuntimeRole)

	// Two unprivileged roles, but the session acts as another role than the
	// one it logged in as: only a login as the serving role itself passes.
	var migrator string
	if err := db.Migrator.QueryRow(ctx, "SELECT current_user").Scan(&migrator); err != nil {
		t.Fatal(err)
	}
	quotedMigrator := pgx.Identifier{migrator}.Sanitize()
	exec("GRANT " + quotedMigrator + " TO " + db.RuntimeRole)
	refused("runtime role acting as another role", open(actingAs(t, db.RuntimeConfig, migrator), config.Production), "acting as")
	exec("REVOKE " + quotedMigrator + " FROM " + db.RuntimeRole)

	if err := open(db.RuntimeConfig, config.Production); err != nil {
		t.Fatalf("the runtime role in production after the revocations: %v", err)
	}
}
