package postgres_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

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
		"CREATE TEMPORARY TABLE scratch (id int)",
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
	return withSetting(t, cfg, "role="+role)
}

// withSetting returns cfg with options=-c <setting> in its URL.
func withSetting(t *testing.T, cfg config.Database, setting string) config.Database {
	t.Helper()
	parsed, err := url.Parse(cfg.URL.Reveal())
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	query := parsed.Query()
	query.Set("options", "-c "+setting)
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

	// REPLICATION: logical decoding reads every tenant's changes. Directly,
	// or through membership in a role that has it (the member can SET ROLE).
	exec("ALTER ROLE " + db.RuntimeRole + " REPLICATION")
	refused("REPLICATION in production", open(db.RuntimeConfig, config.Production), "has REPLICATION")
	if err := open(db.RuntimeConfig, config.Development); err != nil {
		t.Errorf("REPLICATION in development: %v", err)
	}
	exec("ALTER ROLE " + db.RuntimeRole + " NOREPLICATION")

	replication := pgx.Identifier{"forge_test_replication_" + strings.ReplaceAll(uuid.MustNew().String(), "-", "")}.Sanitize()
	exec("CREATE ROLE " + replication + " NOLOGIN REPLICATION")
	t.Cleanup(func() { _, _ = db.Admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+replication) })
	exec("GRANT " + replication + " TO " + db.RuntimeRole)
	refused("member of a REPLICATION role in production", open(db.RuntimeConfig, config.Production),
		"can SET ROLE to a superuser, BYPASSRLS or REPLICATION role")
	exec("REVOKE " + replication + " FROM " + db.RuntimeRole)

	// Predefined roles that reach the server's programs and files: a path to
	// superuser, or to every tenant's data files.
	for _, predefined := range []string{"pg_execute_server_program", "pg_write_server_files", "pg_read_server_files"} {
		exec("GRANT " + predefined + " TO " + db.RuntimeRole)
		refused("member of "+predefined, open(db.RuntimeConfig, config.Production),
			"is a member of pg_execute_server_program, pg_write_server_files or pg_read_server_files")
		exec("REVOKE " + predefined + " FROM " + db.RuntimeRole)
	}

	var migrator string
	if err := db.Migrator.QueryRow(ctx, "SELECT current_user").Scan(&migrator); err != nil {
		t.Fatal(err)
	}
	quotedMigrator := pgx.Identifier{migrator}.Sanitize()

	// The owner of a table with row-level security can switch FORCE off
	// (ALTER TABLE ... NO FORCE ROW LEVEL SECURITY): being it, or able to
	// SET ROLE to it, is refused. Owning a table without row-level security
	// is not.
	const owner = "owns, or can SET ROLE to the owner of, a table with row-level security"
	exec("GRANT " + quotedMigrator + " TO " + db.RuntimeRole)
	refused("member of the table owner", open(db.RuntimeConfig, config.Production), owner)
	exec("REVOKE " + quotedMigrator + " FROM " + db.RuntimeRole)
	exec("ALTER TABLE notes OWNER TO " + db.RuntimeRole)
	refused("direct owner of an RLS table", open(db.RuntimeConfig, config.Production), owner)
	if err := open(db.RuntimeConfig, config.Development); err != nil {
		t.Errorf("owner of an RLS table in development: %v", err)
	}
	exec("ALTER TABLE notes OWNER TO " + quotedMigrator)
	exec("CREATE TABLE plain_owned (id int)")
	exec("ALTER TABLE plain_owned OWNER TO " + db.RuntimeRole)
	if err := open(db.RuntimeConfig, config.Production); err != nil {
		t.Errorf("owner of a table without row-level security in production: %v", err)
	}
	exec("DROP TABLE plain_owned")

	// TRUNCATE, REFERENCES and TRIGGER are not subject to row-level
	// security (TRUNCATE empties every tenant's rows): held directly,
	// through PUBLIC, through an inherited membership or through one it can
	// only SET ROLE to, they are refused on a table with row-level security,
	// and not on a plain table.
	const unconfined = "holds TRUNCATE, REFERENCES or TRIGGER"
	for _, privilege := range []string{"TRUNCATE", "REFERENCES", "TRIGGER", "ALL"} {
		exec("GRANT " + privilege + " ON notes TO " + db.RuntimeRole)
		refused(privilege+" on an RLS table", open(db.RuntimeConfig, config.Production), unconfined)
		exec("REVOKE " + privilege + " ON notes FROM " + db.RuntimeRole)
		exec("GRANT SELECT, INSERT, UPDATE, DELETE ON notes TO " + db.RuntimeRole)
	}
	exec("GRANT TRUNCATE ON notes TO PUBLIC")
	refused("TRUNCATE through PUBLIC", open(db.RuntimeConfig, config.Production), unconfined)
	exec("REVOKE TRUNCATE ON notes FROM PUBLIC")
	group := pgx.Identifier{"forge_test_group_" + strings.ReplaceAll(uuid.MustNew().String(), "-", "")}.Sanitize()
	exec("CREATE ROLE " + group + " NOLOGIN")
	t.Cleanup(func() {
		_, _ = db.Admin.Exec(context.Background(), "DROP OWNED BY "+group+"; DROP ROLE IF EXISTS "+group)
	})
	exec("GRANT TRUNCATE ON notes TO " + group)
	exec("GRANT " + group + " TO " + db.RuntimeRole)
	refused("TRUNCATE through an inherited membership", open(db.RuntimeConfig, config.Production), unconfined)
	exec("REVOKE " + group + " FROM " + db.RuntimeRole)
	// PostgreSQL 16 and later: a membership whose privileges are not
	// inherited, but which still allows SET ROLE.
	exec("GRANT " + group + " TO " + db.RuntimeRole + " WITH INHERIT FALSE")
	refused("TRUNCATE through a SET ROLE membership", open(db.RuntimeConfig, config.Production), unconfined)
	exec("REVOKE " + group + " FROM " + db.RuntimeRole)
	if err := open(db.RuntimeConfig, config.Development); err != nil {
		t.Errorf("TRUNCATE in development: %v", err)
	}
	exec("CREATE TABLE plain_truncated (id int)")
	exec("GRANT ALL ON plain_truncated TO " + db.RuntimeRole)
	if err := open(db.RuntimeConfig, config.Production); err != nil {
		t.Errorf("ALL on a table without row-level security in production: %v", err)
	}
	exec("DROP TABLE plain_truncated")

	// Two unprivileged roles, but the session acts as another role than the
	// one it logged in as: only a login as the serving role itself passes.
	plain := "forge_test_plain_" + strings.ReplaceAll(uuid.MustNew().String(), "-", "")
	quotedPlain := pgx.Identifier{plain}.Sanitize()
	exec("CREATE ROLE " + quotedPlain + " NOLOGIN")
	t.Cleanup(func() { _, _ = db.Admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+quotedPlain) })
	exec("GRANT " + quotedPlain + " TO " + db.RuntimeRole)
	refused("runtime role acting as another role", open(actingAs(t, db.RuntimeConfig, plain), config.Production), "acting as")
	exec("REVOKE " + quotedPlain + " FROM " + db.RuntimeRole)

	if err := open(db.RuntimeConfig, config.Production); err != nil {
		t.Fatalf("the runtime role in production after the revocations: %v", err)
	}
}

// A pg_roles view earlier on the search_path, showing no role at all, does
// not hide the superuser: the check reads pg_catalog.pg_roles.
func TestRequireRowSecurityIgnoresAShadowingPgRolesView(t *testing.T) {
	admin, cfg := postgrestest.NewWithConfig(t)
	ctx := context.Background()
	var superuser bool
	if err := admin.QueryRow(ctx, "SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname = current_user").Scan(&superuser); err != nil {
		t.Fatal(err)
	}
	if !superuser {
		t.Skip("the test server's user is not a superuser")
	}
	if _, err := admin.Exec(ctx, "CREATE VIEW public.pg_roles AS SELECT * FROM pg_catalog.pg_roles WHERE false"); err != nil {
		t.Fatal(err)
	}
	pool, err := postgres.Open(ctx, withSetting(t, cfg, "search_path=public,pg_catalog"), postgres.RequireRowSecurity(config.Production))
	if err == nil {
		pool.Close()
	}
	if !errors.Is(err, postgres.ErrRowSecurityBypass) || !strings.Contains(err.Error(), "is a superuser") {
		t.Fatalf("superuser behind a shadowing pg_roles view: err = %v, want it refused as a superuser", err)
	}
}

// A user-defined = operator for name, earlier on a search_path setting, does
// not make the superuser look like a harmless role: the check pins
// search_path to pg_catalog, so its operators are PostgreSQL's own.
func TestRequireRowSecurityIgnoresAShadowingOperator(t *testing.T) {
	admin, cfg := postgrestest.NewWithConfig(t)
	ctx := context.Background()
	var superuser bool
	if err := admin.QueryRow(ctx, "SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname = current_user").Scan(&superuser); err != nil {
		t.Fatal(err)
	}
	if !superuser {
		t.Skip("the test server's user is not a superuser")
	}
	// The harmless role every comparison would match. Roles are cluster-wide.
	harmless := "forge_test_harmless_" + strings.ReplaceAll(uuid.MustNew().String(), "-", "")
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgx.Identifier{harmless}.Sanitize())
	})
	for _, statement := range []string{
		"CREATE ROLE " + pgx.Identifier{harmless}.Sanitize() + " NOLOGIN",
		"CREATE FUNCTION public.fake_eq(name, name) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$ SELECT $1::text = '" + harmless + "' $$",
		"CREATE OPERATOR public.= (LEFTARG = name, RIGHTARG = name, FUNCTION = public.fake_eq)",
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for name, cfg := range map[string]config.Database{
		"default search_path":            cfg,
		"search_path=public, pg_catalog": withSetting(t, cfg, "search_path=public,pg_catalog"),
	} {
		pool, err := postgres.Open(ctx, cfg, postgres.RequireRowSecurity(config.Production))
		if err == nil {
			pool.Close()
		}
		if !errors.Is(err, postgres.ErrRowSecurityBypass) || !strings.Contains(err.Error(), "is a superuser") {
			t.Errorf("%s: err = %v, want the superuser refused", name, err)
		}
	}
}

// The refusal is Open's default outside development and test, read from
// cfg.Environment; a Database built in code without an environment is
// checked too (fail closed), and AllowRowSecurityBypass is the explicit,
// order-sensitive opt-out.
func TestOpenRefusesBypassingRolesByDefaultOutsideDevelopmentAndTest(t *testing.T) {
	admin, cfg := postgrestest.NewWithConfig(t)
	ctx := context.Background()
	var superuser bool
	if err := admin.QueryRow(ctx, "SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname = current_user").Scan(&superuser); err != nil {
		t.Fatal(err)
	}
	if !superuser {
		t.Skip("the test server's user is not a superuser")
	}
	open := func(environment config.Environment, opts ...postgres.Option) error {
		in := cfg
		in.Environment = environment
		pool, err := postgres.Open(ctx, in, opts...)
		if err == nil {
			pool.Close()
		}
		return err
	}
	for _, environment := range []config.Environment{config.Staging, config.Production, ""} {
		if err := open(environment); !errors.Is(err, postgres.ErrRowSecurityBypass) {
			t.Errorf("a superuser in %q without options: err = %v, want ErrRowSecurityBypass", environment, err)
		}
		if err := open(environment, postgres.AllowRowSecurityBypass()); err != nil {
			t.Errorf("a superuser in %q with AllowRowSecurityBypass: %v", environment, err)
		}
	}
	for _, environment := range []config.Environment{config.Development, config.Test} {
		if err := open(environment); err != nil {
			t.Errorf("a superuser in %s: %v", environment, err)
		}
	}
	// A pool built by hand from config.Default has no environment: checked.
	hand := config.Default().Database
	hand.URL = cfg.URL
	if pool, err := postgres.Open(ctx, hand); !errors.Is(err, postgres.ErrRowSecurityBypass) {
		if err == nil {
			pool.Close()
		}
		t.Errorf("a superuser through config.Default().Database: err = %v, want ErrRowSecurityBypass", err)
	}
	// RequireRowSecurity only ever adds the check: for development it does
	// not switch off the check a production configuration asks for, and for
	// production it wins over AllowRowSecurityBypass in either order.
	for _, environment := range []config.Environment{config.Development, config.Test} {
		if err := open(config.Production, postgres.RequireRowSecurity(environment)); !errors.Is(err, postgres.ErrRowSecurityBypass) {
			t.Errorf("RequireRowSecurity(%s) on a production configuration: err = %v, want ErrRowSecurityBypass", environment, err)
		}
		if err := open(environment, postgres.RequireRowSecurity(environment)); err != nil {
			t.Errorf("RequireRowSecurity(%s) on a %s configuration: %v", environment, environment, err)
		}
	}
	if err := open(config.Development, postgres.RequireRowSecurity(config.Production)); !errors.Is(err, postgres.ErrRowSecurityBypass) {
		t.Errorf("RequireRowSecurity(production) on a development configuration: err = %v, want ErrRowSecurityBypass", err)
	}
	for name, opts := range map[string][]postgres.Option{
		"Require then Allow": {postgres.RequireRowSecurity(config.Production), postgres.AllowRowSecurityBypass()},
		"Allow then Require": {postgres.AllowRowSecurityBypass(), postgres.RequireRowSecurity(config.Production)},
	} {
		if err := open(config.Development, opts...); !errors.Is(err, postgres.ErrRowSecurityBypass) {
			t.Errorf("%s: err = %v, want ErrRowSecurityBypass", name, err)
		}
	}
	if err := open(config.Production, postgres.RequireRowSecurity(config.Development), postgres.AllowRowSecurityBypass()); err != nil {
		t.Errorf("AllowRowSecurityBypass with RequireRowSecurity(development): %v", err)
	}
}

// A lazy pool connects on its first query, so Open succeeds without a
// reachable server, and it checks the role of every connection it opens
// instead of the pool at Open.
func TestLazyPoolConnectsOnFirstUseAndChecksEachConnection(t *testing.T) {
	ctx := context.Background()
	unreachable := config.Default().Database
	unreachable.URL = config.NewSecret("postgres://forge:secret@127.0.0.1:1/forge?sslmode=disable")
	unreachable.ConnectTimeout = 2 * time.Second
	if _, err := postgres.Open(ctx, unreachable); err == nil {
		t.Fatal("Open connected to an unreachable server")
	}
	lazy, err := postgres.Open(ctx, unreachable, postgres.Lazy(true))
	if err != nil {
		t.Fatalf("a lazy pool on an unreachable server: %v", err)
	}
	if err := lazy.Ping(ctx); err == nil {
		t.Error("a lazy pool on an unreachable server answered a ping")
	}
	lazy.Close()

	db := postgrestest.NewRLS(t, rlsMigrations)
	runtime := db.RuntimeConfig
	runtime.Environment = config.Production
	served, err := postgres.Open(ctx, runtime, postgres.Lazy(true))
	if err != nil {
		t.Fatal(err)
	}
	defer served.Close()
	if err := served.Ping(ctx); err != nil {
		t.Errorf("the runtime role through a lazy pool in production: %v", err)
	}

	var superuser bool
	if err := db.Admin.QueryRow(ctx, "SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname = current_user").Scan(&superuser); err != nil {
		t.Fatal(err)
	}
	if !superuser {
		return
	}
	admin := postgrestest.Config(t)
	admin.Environment = config.Production
	refusing, err := postgres.Open(ctx, admin, postgres.Lazy(true))
	if err != nil {
		t.Fatalf("Open of a lazy pool checked the role up front: %v", err)
	}
	defer refusing.Close()
	var one int
	if err := refusing.QueryRow(ctx, "SELECT 1").Scan(&one); !errors.Is(err, postgres.ErrRowSecurityBypass) {
		t.Fatalf("a superuser's first query through a lazy pool in production: err = %v, want ErrRowSecurityBypass", err)
	}
}
