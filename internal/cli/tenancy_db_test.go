package cli

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/stdlib"

	authmysql "github.com/rocketinsidecorp/forge-go/auth/mysql"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	authsqlite "github.com/rocketinsidecorp/forge-go/auth/sqlite"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/mysql"
	"github.com/rocketinsidecorp/forge-go/mysql/mysqltest"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// tenancyDatabase is a fresh database of one adapter with Forge identity.
type tenancyDatabase struct {
	t          *testing.T
	adapter    string
	cfg        config.Database
	db         *sql.DB
	identity   fs.FS
	newMigrate func(config.Database, []migrate.Migration, *slog.Logger) (*migrate.Migrator, error)
}

func newTenancyDatabase(t *testing.T, adapter string) *tenancyDatabase {
	t.Helper()
	d := &tenancyDatabase{t: t, adapter: adapter}
	switch adapter {
	case "postgres":
		database, cfg := postgrestest.NewWithConfig(t)
		d.cfg, d.db, d.identity, d.newMigrate = cfg, stdlib.OpenDBFromPool(database.Pool()), authpostgres.Migrations(), postgres.NewMigrator
	case "mysql":
		database, cfg := mysqltest.NewWithConfig(t)
		d.cfg, d.db, d.identity, d.newMigrate = cfg, database.SQL(), authmysql.Migrations(), mysql.NewMigrator
	default:
		database, cfg := sqlitetest.NewWithConfig(t)
		d.cfg, d.db, d.identity, d.newMigrate = cfg, database.SQL(), authsqlite.Migrations(), sqlite.NewMigrator
	}
	return d
}

var placeholder = regexp.MustCompile(`\?`)

func (d *tenancyDatabase) sql(query string) string {
	if d.adapter != "postgres" {
		return query
	}
	n := 0
	return placeholder.ReplaceAllStringFunc(query, func(string) string { n++; return fmt.Sprintf("$%d", n) })
}

func (d *tenancyDatabase) exec(query string, args ...any) error {
	_, err := d.db.ExecContext(context.Background(), d.sql(query), args...)
	return err
}

func (d *tenancyDatabase) mustExec(query string, args ...any) {
	d.t.Helper()
	if err := d.exec(query, args...); err != nil {
		d.t.Fatalf("%s: %v", query, err)
	}
}

func (d *tenancyDatabase) value(query string, args ...any) string {
	d.t.Helper()
	var value sql.NullString
	if err := d.db.QueryRowContext(context.Background(), d.sql(query), args...).Scan(&value); err != nil {
		d.t.Fatalf("%s: %v", query, err)
	}
	return value.String
}

// migrator migrates the identity schema and app.
func (d *tenancyDatabase) migrator(app fs.FS) *migrate.Migrator {
	d.t.Helper()
	loaded, err := migrate.LoadMigrationSets(d.identity, app)
	if err != nil {
		d.t.Fatal(err)
	}
	migrator, err := d.newMigrate(d.cfg, loaded, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		d.t.Fatal(err)
	}
	d.t.Cleanup(func() { _ = migrator.Close() })
	return migrator
}

// applied lists the applied migration names.
func (d *tenancyDatabase) applied(migrator *migrate.Migrator) string {
	d.t.Helper()
	statuses, err := migrator.Status(context.Background())
	if err != nil {
		d.t.Fatal(err)
	}
	var names []string
	for _, status := range statuses {
		if !status.AppliedAt.IsZero() {
			names = append(names, status.Name)
		}
	}
	return strings.Join(names, ",")
}

// schema describes what the tenancy migration changes: whether products and
// tags have org_id, the organizations' slugs, and whether a guard table was
// left behind.
func (d *tenancyDatabase) schema() string {
	d.t.Helper()
	var parts []string
	for _, table := range []string{"products", "tags"} {
		has := d.exec("SELECT org_id FROM " + table + " WHERE 1 = 0")
		parts = append(parts, fmt.Sprintf("%s.org_id=%v", table, has == nil))
	}
	if d.adapter == "mysql" {
		parts = append(parts, "indexes="+d.value("SELECT GROUP_CONCAT(index_name ORDER BY index_name) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'products'"))
		parts = append(parts, "foreign_keys="+d.value("SELECT GROUP_CONCAT(constraint_name ORDER BY constraint_name) FROM information_schema.table_constraints WHERE table_schema = DATABASE() AND table_name = 'products' AND constraint_type = 'FOREIGN KEY'"))
	}
	parts = append(parts, "slugs="+d.value("SELECT "+map[bool]string{true: "GROUP_CONCAT(slug ORDER BY slug)", false: "string_agg(slug, ',' ORDER BY slug)"}[d.adapter != "postgres"]+" FROM forge_tenants"))
	parts = append(parts, fmt.Sprintf("guard_table=%v", d.exec("SELECT 1 FROM forge_tenancy_guard") == nil))
	return strings.Join(parts, " ")
}

func (d *tenancyDatabase) organization(slug string) uuid.UUID {
	d.t.Helper()
	org, id := uuid.MustNew(), uuid.MustNew()
	d.mustExec("INSERT INTO forge_organizations (id, name) VALUES (?, ?)", org, slug)
	d.mustExec("INSERT INTO forge_tenants (id, organization_id, slug) VALUES (?, ?, ?)", id, org, slug)
	return id
}

// tenancyApp is a single-organization application's migrations: products
// and tags created plain, then forge generate tenancy's migration.
func tenancyApp(adapter string, converted bool) fstest.MapFS {
	q := func(name string) string { return quoteIdentifier(adapter, name) }
	id := map[string]string{"postgres": "uuid PRIMARY KEY", "mysql": "CHAR(36) NOT NULL PRIMARY KEY", "sqlite": "TEXT NOT NULL PRIMARY KEY"}[adapter]
	suffix := map[string]string{"mysql": " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4", "sqlite": " STRICT"}[adapter]
	app := fstest.MapFS{}
	for i, table := range []string{"products", "tags"} {
		name := fmt.Sprintf("2026110%d000000_create_%s", i+1, table)
		app[name+".up.sql"] = &fstest.MapFile{Data: fmt.Appendf(nil, "CREATE TABLE %s (%s %s, %s TEXT NOT NULL)%s;\n", q(table), q("id"), id, q("name"), suffix)}
		app[name+".down.sql"] = &fstest.MapFile{Data: fmt.Appendf(nil, "DROP TABLE %s;\n", q(table))}
	}
	if converted {
		up, down := tenancyMigration(adapter, []string{"products", "tags"}, "acme")
		app["20261103000000_add_organizations.up.sql"] = &fstest.MapFile{Data: []byte(up)}
		app["20261103000000_add_organizations.down.sql"] = &fstest.MapFile{Data: []byte(down)}
	}
	return app
}

// TestTenancyMigrationGuardsChangeNothingWhenTheyRefuse runs forge generate
// tenancy's migration on every database (SQLite; MySQL or MariaDB, whichever
// FORGE_TEST_MYSQL_URL names; PostgreSQL). Each refusal must leave the
// schema, the data and the migration record exactly as they were (MySQL
// commits DDL statement by statement, so the guards run first):
//
//   - up with rows but no organization (setup never ran);
//   - down while a second organization exists, even one without rows, which
//     would otherwise sign in to the first one's data;
//   - down after the organization's slug was renamed, still with two.
//
// Converted, org_id must name an organization: NULL, empty and unknown
// values are refused.
func TestTenancyMigrationGuardsChangeNothingWhenTheyRefuse(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the database servers")
	}
	t.Parallel()
	for _, adapter := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(adapter, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			d := newTenancyDatabase(t, adapter)
			if _, err := d.migrator(tenancyApp(adapter, false)).Up(ctx); err != nil {
				t.Fatal(err)
			}
			for i := range 3 {
				d.mustExec("INSERT INTO products (id, name) VALUES (?, ?)", uuid.MustNew(), fmt.Sprintf("p%d", i))
			}

			// Up refuses rows without an organization, changing nothing.
			converted := d.migrator(tenancyApp(adapter, true))
			before, applied := d.schema(), d.applied(converted)
			_, err := converted.Up(ctx)
			if err == nil || !strings.Contains(err.Error(), map[bool]string{true: "run forge setup first", false: "forge_tenancy_rows_need_forge_setup_first"}[adapter == "postgres"]) {
				t.Fatalf("up without an organization: %v", err)
			}
			if after := d.schema(); after != before || d.applied(converted) != applied {
				t.Fatalf("a refused up changed the database:\nbefore %s (%s)\nafter  %s (%s)", before, applied, after, d.applied(converted))
			}

			// With the organization setup creates, up converts.
			acme := d.organization("default")
			if _, err := converted.Up(ctx); err != nil {
				t.Fatal(err)
			}
			if got := d.value("SELECT COUNT(*) FROM products WHERE org_id = ?", acme); got != "3" {
				t.Fatalf("products backfilled with acme: %s", got)
			}
			if got := d.value("SELECT slug FROM forge_tenants"); got != "acme" {
				t.Fatalf("slug = %q", got)
			}
			// org_id names an organization: NULL, empty and unknown are refused.
			bad := []any{nil, ""}
			if adapter != "postgres" {
				// PostgreSQL isolates org_id with row-level security instead
				// of a foreign key.
				bad = append(bad, uuid.MustNew().String())
			}
			for _, bad := range bad {
				if err := d.exec("INSERT INTO tags (id, org_id, name) VALUES (?, ?, 'x')", uuid.MustNew(), bad); err == nil {
					t.Fatalf("a tag with org_id %v was stored", bad)
				}
				if err := d.exec("UPDATE products SET org_id = ? WHERE name = 'p0'", bad); err == nil {
					t.Fatalf("a product was moved to org_id %v", bad)
				}
			}
			if adapter == "sqlite" {
				// REPLACE turns a NULL into the column default on a NOT NULL
				// column; the triggers still see the NULL.
				if err := d.exec("INSERT OR REPLACE INTO tags (id, org_id, name) VALUES (?, NULL, 'x')", uuid.MustNew()); err == nil {
					t.Fatal("INSERT OR REPLACE stored a tag without an organization")
				}
			}

			// A second organization without rows: down refuses, changing
			// nothing, also after the first one's slug was renamed.
			d.organization("globex")
			for _, rename := range []bool{false, true} {
				if rename {
					d.mustExec("UPDATE forge_tenants SET slug = 'renamed' WHERE slug = 'acme'")
				}
				migrator := d.migrator(tenancyApp(adapter, true))
				before, applied := d.schema(), d.applied(migrator)
				_, err := migrator.Down(ctx, 1)
				if err == nil || !strings.Contains(err.Error(), map[bool]string{true: "more than one organization exists", false: "forge_tenancy_rollback_needs_one_organization"}[adapter == "postgres"]) {
					t.Fatalf("down with a second organization (renamed=%v): %v", rename, err)
				}
				if after := d.schema(); after != before || d.applied(migrator) != applied || !strings.Contains(applied, "add_organizations") {
					t.Fatalf("a refused down changed the database:\nbefore %s (%s)\nafter  %s (%s)", before, applied, after, d.applied(migrator))
				}
			}

			// Without it, down reverts the schema and the hidden slug.
			d.mustExec("DELETE FROM forge_tenants WHERE slug = 'globex'")
			migrator := d.migrator(tenancyApp(adapter, true))
			if _, err := migrator.Down(ctx, 1); err != nil {
				t.Fatal(err)
			}
			if got, want := d.schema(), "products.org_id=false tags.org_id=false"; !strings.HasPrefix(got, want) || !strings.Contains(got, "slugs=default ") || strings.Contains(d.applied(migrator), "add_organizations") {
				t.Fatalf("after down: %s (%s)", got, d.applied(migrator))
			}
			if got := d.value("SELECT COUNT(*) FROM products"); got != "3" {
				t.Fatalf("down lost rows: %s", got)
			}
		})
	}
}

// A table generated --org-scoped on SQLite or MySQL/MariaDB has org_id as a
// foreign key to forge_tenants: a row of an unknown organization is refused.
func TestOrgScopedTablesReferenceForgeTenants(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the database servers")
	}
	t.Parallel()
	for _, adapter := range []string{"sqlite", "mysql"} {
		t.Run(adapter, func(t *testing.T) {
			t.Parallel()
			rendered, err := renderResource(resourceSpec{
				resourceRequest: resourceRequest{Name: "product", Plural: "products", Fields: []resourceField{{Name: "name", Type: "string"}}, TenantScoped: true},
				Module:          "example.com/shop", Database: adapter, Identity: true, Organizations: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			d := newTenancyDatabase(t, adapter)
			app := fstest.MapFS{
				"20261101000000_create_products.up.sql":   {Data: []byte(rendered.migrationUp)},
				"20261101000000_create_products.down.sql": {Data: []byte(rendered.migrationDown)},
			}
			if _, err := d.migrator(app).Up(context.Background()); err != nil {
				t.Fatal(err)
			}
			acme := d.organization("acme")
			insert := "INSERT INTO products (id, org_id, name, version, created_at, updated_at) VALUES (?, ?, 'x', 1, '2026-10-27 00:00:00.000000', '2026-10-27 00:00:00.000000')"
			d.mustExec(insert, uuid.MustNew(), acme)
			if err := d.exec(insert, uuid.MustNew(), uuid.MustNew()); err == nil {
				t.Fatal("a product of an unknown organization was stored")
			}
		})
	}
}
