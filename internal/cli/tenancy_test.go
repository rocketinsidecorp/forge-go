package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

var tenancyNow = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func runTenancy(root string, args ...string) (string, error) {
	var out bytes.Buffer
	err := runGenerateEnv(append([]string{"tenancy"}, args...), &out, root, nil, tenancyNow)
	return out.String(), err
}

func generateIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runGenerateEnv(args, &out, dir, nil, tenancyNow.Add(-time.Hour)); err != nil {
		t.Fatalf("forge generate %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

// TestGenerateTenancyConvertsASingleOrganizationApplication is decision
// D36: the generated tables become org-scoped (their code rendered again,
// as --org-scoped renders it), the migration adds org_id, and the
// application has organizations afterwards; a --public table stays global.
func TestGenerateTenancyConvertsASingleOrganizationApplication(t *testing.T) {
	t.Parallel()
	dir := newApp(t, scaffold.Options{Database: "sqlite"})
	generateIn(t, dir, "scaffold", "note", "body:text", "--public")
	if _, err := runAuthentication(dir); err != nil {
		t.Fatal(err)
	}
	generateIn(t, dir, "scaffold", "product", "name:string", "price:integer")
	generateIn(t, dir, "model", "tag", "label:string")
	// The same scaffold in an application created with --tenants.
	want := newApp(t, scaffold.Options{Database: "sqlite", Tenants: true})
	generateIn(t, want, "scaffold", "product", "name:string", "price:integer", "--org-scoped")

	before := snapshotTree(t, dir)
	if _, err := runTenancy(dir, "--dry-run"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before, snapshotTree(t, dir)) {
		t.Fatal("--dry-run wrote files")
	}
	output, err := runTenancy(dir, "--org", "acme")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"create  db/migrations/20261027120000_add_organizations.up.sql",
		"convert  " + filepath.Join("app", "adapters", "persistence", "product.go"),
		"convert  " + filepath.Join("app", "adapters", "persistence", "tag.go"),
		"rewrite  " + scaffold.AppFile,
		"Org-scoped now: products, tags",
		"notes stays global",
		`belong to the organization "acme"`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
	// The converted code is what --org-scoped generates.
	for _, path := range []string{"app/domain/product.go", "app/application/product.go", "app/adapters/persistence/product.go", "app/adapters/persistence/product_test.go", "app/adapters/httpapi/product.go", "app/adapters/htmlui/product.go", scaffold.AppFile} {
		if readFile(t, filepath.Join(dir, filepath.FromSlash(path))) != readFile(t, filepath.Join(want, filepath.FromSlash(path))) {
			t.Errorf("%s differs from forge new --tenants with an --org-scoped scaffold", path)
		}
	}
	up := readFile(t, filepath.Join(dir, "db", "migrations", "20261027120000_add_organizations.up.sql"))
	for _, want := range []string{`ALTER TABLE "products" ADD COLUMN "org_id" TEXT NOT NULL DEFAULT '';`, `UPDATE "products" SET "org_id" = (SELECT id FROM forge_tenants WHERE slug = 'default');`, `CREATE INDEX "tags_org_id_idx"`, "UPDATE forge_tenants SET slug = 'acme' WHERE slug = 'default';"} {
		if !strings.Contains(up, want) {
			t.Errorf("the migration lacks %q:\n%s", want, up)
		}
	}
	if strings.Contains(up, `"notes"`) {
		t.Errorf("the --public table was scoped:\n%s", up)
	}
	// The application has organizations now.
	if _, err := runTenancy(dir); err == nil || !strings.Contains(err.Error(), "has organizations already") {
		t.Fatalf("a second conversion: %v", err)
	}
	var out bytes.Buffer
	if err := runGenerateEnv([]string{"scaffold", "widget", "name:string"}, &out, dir, nil, tenancyNow); err == nil || !strings.Contains(err.Error(), "choose a scope") {
		t.Fatalf("a scaffold without a scope after the conversion: %v", err)
	}
	generateIn(t, dir, "scaffold", "widget", "name:string", "--org-scoped")
}

func TestGenerateTenancyRefuses(t *testing.T) {
	t.Parallel()
	edited := func(t *testing.T) string {
		dir := newApp(t, scaffold.Options{Database: "sqlite", Auth: true})
		generateIn(t, dir, "scaffold", "product", "name:string")
		path := filepath.Join(dir, "app", "adapters", "persistence", "product.go")
		writeFile(t, path, readFile(t, path)+"\n// mine\n")
		return dir
	}
	for name, test := range map[string]struct {
		setup func(t *testing.T) string
		args  []string
		want  string
	}{
		"no go.mod":           {func(t *testing.T) string { return t.TempDir() }, nil, "go.mod not found"},
		"no such directory":   {func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") }, nil, "go.mod not found"},
		"no authentication":   {func(t *testing.T) string { return newApp(t, scaffold.Options{Database: "sqlite"}) }, nil, "forge generate authentication` first"},
		"organizations":       {func(t *testing.T) string { return newApp(t, scaffold.Options{Database: "sqlite", Tenants: true}) }, nil, "has organizations already"},
		"edited code":         {edited, nil, "is not what `forge generate scaffold"},
		"a bad slug":          {func(t *testing.T) string { return newApp(t, scaffold.Options{Database: "sqlite", Auth: true}) }, []string{"--org", "Bad Slug"}, "must be a new organization slug"},
		"the default slug":    {func(t *testing.T) string { return newApp(t, scaffold.Options{Database: "sqlite", Auth: true}) }, []string{"--org=default"}, "must be a new organization slug"},
		"an unknown flag":     {func(t *testing.T) string { return t.TempDir() }, []string{"--force"}, tenancyUsage},
		"a flag twice":        {func(t *testing.T) string { return t.TempDir() }, []string{"--dry-run", "--dry-run"}, tenancyUsage},
		"--org without value": {func(t *testing.T) string { return t.TempDir() }, []string{"--org"}, tenancyUsage},
		"broken environment": {func(t *testing.T) string {
			dir := newApp(t, scaffold.Options{Database: "sqlite", Auth: true})
			writeFile(t, filepath.Join(dir, ".env.development"), "bad line\n")
			return dir
		}, nil, "environment files could not be loaded"},
		"an edited app_gen.go": {func(t *testing.T) string {
			dir := newApp(t, scaffold.Options{Database: "sqlite", Auth: true})
			path := filepath.Join(dir, scaffold.AppFile)
			writeFile(t, path, readFile(t, path)+"\n// mine\n")
			return dir
		}, nil, "edited by hand"},
		"a converted migration exists": {func(t *testing.T) string {
			dir := newApp(t, scaffold.Options{Database: "sqlite", Auth: true})
			writeFile(t, filepath.Join(dir, "db", "migrations", "20260101000000_add_organizations.up.sql"), "SELECT 1;")
			return dir
		}, nil, "exists already"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := test.setup(t)
			var before []string
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				before = snapshotTree(t, dir)
			}
			if _, err := runTenancy(dir, test.args...); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
			if before != nil && !slices.Equal(before, snapshotTree(t, dir)) {
				t.Fatal("a refused conversion wrote files")
			}
		})
	}
}

// TestTenancyMigrationOnEveryDatabase pins the SQL of each dialect: the
// backfill, the required column and its index, PostgreSQL's row-level
// security, and a down that refuses while another organization has rows.
func TestTenancyMigrationOnEveryDatabase(t *testing.T) {
	t.Parallel()
	for adapter, wants := range map[string][2][]string{
		"postgres": {
			{`ALTER TABLE "posts" ADD COLUMN "org_id" uuid;`, `ALTER TABLE "posts" ALTER COLUMN "org_id" SET NOT NULL;`, `ALTER TABLE "posts" FORCE ROW LEVEL SECURITY;`, `CREATE POLICY "posts_org" ON "posts"`},
			{`DROP POLICY "posts_org" ON "posts";`, `ALTER TABLE "posts" DISABLE ROW LEVEL SECURITY;`, `DROP INDEX "posts_org_id_idx";`, `ALTER TABLE "posts" DROP COLUMN "org_id";`, "CHECK (rows_of_other_organizations = 0)"},
		},
		"mysql": {
			{"ALTER TABLE `posts` ADD COLUMN `org_id` CHAR(36) NULL AFTER `id`;", "ALTER TABLE `posts` MODIFY `org_id` CHAR(36) NOT NULL;", "CREATE INDEX `posts_org_id_idx` ON `posts` (`org_id`, `id`);"},
			{"DROP INDEX `posts_org_id_idx` ON `posts`;", "ALTER TABLE `posts` DROP COLUMN `org_id`;", "INSERT INTO forge_tenancy_rollback_guard"},
		},
		"sqlite": {
			{`CREATE TRIGGER "posts_org_id_required_insert" BEFORE INSERT ON "posts"`, `CREATE TRIGGER "posts_org_id_required_update" BEFORE UPDATE OF "org_id" ON "posts"`},
			{`DROP TRIGGER "posts_org_id_required_insert";`, `DROP INDEX "posts_org_id_idx";`, `ALTER TABLE "posts" DROP COLUMN "org_id";`},
		},
	} {
		up, down := tenancyMigration(adapter, []string{"posts"}, "")
		for _, want := range wants[0] {
			if !strings.Contains(up, want) {
				t.Errorf("%s up lacks %q:\n%s", adapter, want, up)
			}
		}
		for _, want := range wants[1] {
			if !strings.Contains(down, want) {
				t.Errorf("%s down lacks %q:\n%s", adapter, want, down)
			}
		}
		if adapter != "postgres" && strings.Contains(up, "ROW LEVEL SECURITY") {
			t.Errorf("%s has no row-level security:\n%s", adapter, up)
		}
	}
	up, down := tenancyMigration("sqlite", nil, "acme")
	if !strings.Contains(up, "No generated table needs") || !strings.Contains(up, "SET slug = 'acme'") || strings.Contains(down, "guard") || !strings.Contains(down, "SET slug = 'default' WHERE slug = 'acme'") {
		t.Fatalf("no tables:\n%s\n%s", up, down)
	}
}
