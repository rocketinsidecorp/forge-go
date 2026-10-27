package cli

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseResourceArgs(t *testing.T) {
	t.Parallel()
	valid := []struct {
		name         string
		args         []string
		wantName     string
		wantPlural   string
		wantTenant   bool
		wantDryRun   bool
		wantFields   []resourceField
		wantErrorFor string
	}{
		{
			name: "tenant scoped", args: []string{"product", "name:string", "price_cents:int", "--tenant-scoped"},
			wantName: "product", wantPlural: "products", wantTenant: true,
			wantFields: []resourceField{{Name: "name", Type: "string", Optional: false}, {Name: "price_cents", Type: "int", Optional: false}},
		},
		{
			name: "flags may come first and last", args: []string{"--dry-run", "--global", "category", "title:text?", "--plural", "categories"},
			wantName: "category", wantPlural: "categories", wantDryRun: true,
			wantFields: []resourceField{{Name: "title", Type: "text", Optional: true}},
		},
		{
			name: "plural with equals", args: []string{"person", "name:string", "--global", "--plural=people"},
			wantName: "person", wantPlural: "people",
			wantFields: []resourceField{{Name: "name", Type: "string", Optional: false}},
		},
		{
			// Identifiers are quoted in all generated SQL, so SQL keywords are fine names.
			name: "reserved SQL words", args: []string{"group", "order:int", "change:string", "--global"},
			wantName: "group", wantPlural: "groups",
			wantFields: []resourceField{{Name: "order", Type: "int", Optional: false}, {Name: "change", Type: "string", Optional: false}},
		},
		{
			name: "every type", args: []string{"order_item", "a:string", "b:text?", "c:int", "d:bool?", "e:time", "f:uuid?", "--tenant-scoped"},
			wantName: "order_item", wantPlural: "order_items", wantTenant: true,
			wantFields: []resourceField{{Name: "a", Type: "string", Optional: false}, {Name: "b", Type: "text", Optional: true}, {Name: "c", Type: "int", Optional: false}, {Name: "d", Type: "bool", Optional: true}, {Name: "e", Type: "time", Optional: false}, {Name: "f", Type: "uuid", Optional: true}},
		},
	}
	for _, test := range valid {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseResourceArgs(test.args)
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != test.wantName || got.Plural != test.wantPlural || got.TenantScoped != test.wantTenant || got.DryRun != test.wantDryRun {
				t.Fatalf("got %+v", got)
			}
			if !slices.Equal(got.Fields, test.wantFields) {
				t.Fatalf("fields = %+v, want %+v", got.Fields, test.wantFields)
			}
		})
	}

	invalid := []struct {
		name string
		args []string
		want string
	}{
		{"both scopes", []string{"product", "name:string", "--global", "--tenant-scoped"}, "mutually exclusive"},
		{"no name", []string{"--global"}, "usage:"},
		{"no fields", []string{"product", "--global"}, "at least one field"},
		{"unknown flag", []string{"product", "name:string", "--global", "--force"}, "unknown flag"},
		{"plural without value", []string{"product", "name:string", "--global", "--plural"}, "--plural needs a value"},
		{"plural twice", []string{"product", "name:string", "--global", "--plural", "a", "--plural", "b"}, "more than once"},
		{"dry run twice", []string{"product", "name:string", "--global", "--dry-run", "--dry-run"}, "more than once"},
		{"uppercase name", []string{"Product", "name:string", "--global"}, "singular snake_case"},
		{"hyphenated name", []string{"order-item", "name:string", "--global"}, "singular snake_case"},
		{"double underscore", []string{"order__item", "name:string", "--global"}, "singular snake_case"},
		{"digit first", []string{"1product", "name:string", "--global"}, "singular snake_case"},
		{"name too long", []string{strings.Repeat("a", 41), "name:string", "--global"}, "singular snake_case"},
		{"test suffix", []string{"widget_test", "name:string", "--global"}, "build constraint"},
		{"os suffix", []string{"widget_linux", "name:string", "--global"}, "build constraint"},
		{"plural equals name", []string{"sheep", "name:string", "--global", "--plural", "sheep"}, "must differ"},
		{"plural not snake", []string{"person", "name:string", "--global", "--plural", "People"}, "snake_case"},
		{"plural forge prefix", []string{"thing", "name:string", "--global", "--plural", "forge_things"}, "reserved for Forge"},
		{"missing type", []string{"product", "name", "--global"}, "name:type"},
		{"unknown type", []string{"product", "name:varchar", "--global"}, "unsupported type"},
		{"float type", []string{"product", "price:float", "--global"}, "unsupported type"},
		{"decimal type", []string{"product", "price:decimal", "--global"}, "unsupported type"},
		{"uppercase field", []string{"product", "Name:string", "--global"}, "field name"},
		{"reserved id", []string{"product", "id:uuid", "--global"}, "reserved"},
		{"reserved tenant_id", []string{"product", "tenant_id:uuid", "--tenant-scoped"}, "reserved"},
		{"reserved version", []string{"product", "version:int", "--global"}, "reserved"},
		{"reserved created_at", []string{"product", "created_at:time", "--global"}, "reserved"},
		{"reserved updated_at", []string{"product", "updated_at:time", "--global"}, "reserved"},
		{"duplicate field", []string{"product", "name:string", "name:text", "--global"}, "declared twice"},
		{"go name collision", []string{"product", "i_d:int", "--global"}, "collides"},
		{"clear flag collision", []string{"product", "note:text?", "clear_note:bool", "--global"}, "Clear"},
		{"name already plural", []string{"products", "name:string", "--global"}, `"product"`},
		{"name already plural ies", []string{"categories", "name:string", "--global"}, `"category"`},
		{"name already plural es", []string{"boxes", "name:string", "--global"}, `"box"`},
		{"apply collides with a method", []string{"product", "apply:bool", "--global"}, "collides"},
		{"is_empty collides with a method", []string{"product", "is_empty:bool", "--global"}, "collides"},
		{"version collides", []string{"product", "id:int", "--global"}, "reserved"},
		{"go keyword name", []string{"type", "name:string", "--global"}, "Go keyword"},
		{"predeclared name", []string{"error", "name:string", "--global"}, "Go keyword"},
		{"imported package name", []string{"time", "name:string", "--global"}, "Go keyword"},
		{"imported package plural", []string{"thing", "name:string", "--global", "--plural", "errors"}, "Go keyword"},
		{"identifier too long", []string{strings.Repeat("a", 38), strings.Repeat("b", 38) + ":string", "--global"}, "too long"},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseResourceArgs(test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestPluralize(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"product":    "products",
		"category":   "categories",
		"company":    "companies",
		"day":        "days",
		"key":        "keys",
		"status":     "statuses",
		"box":        "boxes",
		"match":      "matches",
		"dish":       "dishes",
		"order_item": "order_items",
		"policy":     "policies",
	} {
		if got := pluralize(name); got != want {
			t.Errorf("pluralize(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestExportedName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{"product": "Product", "product_line": "ProductLine", "order_items": "OrderItems", "a1_b2": "A1B2", "owner_id": "OwnerID", "api_key": "APIKey", "uuid": "UUID", "identity": "Identity", "base_url": "BaseURL"} {
		if got := exportedName(name); got != want {
			t.Errorf("exportedName(%q) = %q, want %q", name, got, want)
		}
	}
}

// resourceProject is a minimal Forge application directory: enough for the
// generator, which only reads go.mod, the environment files and cmd/.
// plainProject is resourceProject without Forge identity: an application
// created without --auth, whose tables are plain tables.
func plainProject(t *testing.T, databaseURL string) string {
	t.Helper()
	root := resourceProject(t, databaseURL)
	writeFile(t, filepath.Join(root, "cmd", "shop", "main.go"), "package main\n\nfunc main() {}\n")
	return root
}

func resourceProject(t *testing.T, databaseURL string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n\ngo 1.27.1\n")
	writeFile(t, filepath.Join(root, "db", "migrations", ".keep"), "")
	if databaseURL != "" {
		writeFile(t, filepath.Join(root, ".env.development"), "FORGE_DATABASE_URL="+databaseURL+"\n")
	}
	// Forge identity on its database, from before app_gen.go: an
	// application with organizations, whose tables take a scope.
	adapter := "postgres"
	switch {
	case strings.HasPrefix(databaseURL, "mysql:"):
		adapter = "mysql"
	case strings.HasPrefix(databaseURL, "sqlite:"):
		adapter = "sqlite"
	}
	writeFile(t, filepath.Join(root, "cmd", "shop", "main.go"), "package main\n\nimport _ \"github.com/rocketinsidecorp/forge-go/auth/"+adapter+"\"\n\nfunc main() {}\n")
	return root
}

var resourceDatabases = []struct{ name, url string }{
	{"postgres", "postgres://u:p@localhost/db"},
	{"mysql", "mysql://u:p@localhost/db"},
	{"sqlite", "sqlite:storage/development.db"},
}

func runResource(root string, args ...string) (string, error) {
	var out bytes.Buffer
	err := runGenerateEnv(append([]string{"resource"}, args...), &out, root, nil, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	return out.String(), err
}

func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		if relative == ".forge" {
			return nil // the generate lock's directory, which generators keep
		}
		entries = append(entries, relative)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestGenerateResourceDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "postgres://u:p@localhost/db")
	before := snapshotTree(t, root)
	output, err := runResource(root, "product", "name:string", "--tenant-scoped", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if after := snapshotTree(t, root); !slices.Equal(before, after) {
		t.Fatalf("--dry-run changed the project:\nbefore %v\nafter  %v", before, after)
	}
	for _, want := range []string{
		"would create  db/migrations/20261014120000_create_products.up.sql",
		"would create  db/migrations/20261014120000_create_products.down.sql",
		"would create  " + filepath.Join("app", "domain", "product.go"),
		"would create  " + filepath.Join("app", "domain", "product_test.go"),
		"would create  " + filepath.Join("app", "application", "product.go"),
		"would create  " + filepath.Join("app", "adapters", "persistence", "product_test.go"),
		"would create  " + filepath.Join("app", "adapters", "httpapi", "product.go"),
		"auth.RequireAudited(required, next, repository)",
		"httpapi.RegisterProducts(app, productService, protect)",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("dry-run output is missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "  create  ") {
		t.Errorf("dry-run claims to have created files:\n%s", output)
	}
}

func TestGenerateResourceWritesFiles(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "mysql://u:p@localhost/db")
	output, err := runResource(root, "order_item", "sku:string", "quantity:int", "--tenant-scoped")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"db/migrations/20261014120000_create_order_items.up.sql",
		"db/migrations/20261014120000_create_order_items.down.sql",
		"app/domain/order_item.go", "app/domain/order_item_test.go",
		"app/application/order_item.go", "app/application/order_item_test.go",
		"app/adapters/persistence/order_item.go", "app/adapters/persistence/order_item_test.go",
		"app/adapters/httpapi/order_item.go", "app/adapters/httpapi/order_item_test.go",
	} {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", path, info.Mode().Perm())
		}
		if !strings.Contains(output, "  create  "+filepath.FromSlash(path)) {
			t.Errorf("output does not list %s:\n%s", path, output)
		}
	}
	// Forge identity on MySQL (decision D30): the guard is the session's.
	if !strings.Contains(output, "auth.RequireAudited(required, next, repository)") || strings.Contains(output, "return next, nil") {
		t.Errorf("identity wiring must print the session guard:\n%s", output)
	}

	// A second resource coexists; the migration version moves past the first.
	if _, err := runResource(root, "category", "name:string", "--global"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "db", "migrations", "20261014120001_create_categories.up.sql")); err != nil {
		t.Fatalf("second migration was not bumped past the first: %v", err)
	}
}

func TestGenerateResourceRefusesToOverwriteAnyFile(t *testing.T) {
	t.Parallel()
	for _, existing := range []string{
		"app/domain/product.go", "app/domain/product_test.go",
		"app/application/product.go", "app/application/product_test.go",
		"app/adapters/persistence/product.go", "app/adapters/persistence/product_test.go",
		"app/adapters/httpapi/product.go", "app/adapters/httpapi/product_test.go",
	} {
		t.Run(existing, func(t *testing.T) {
			t.Parallel()
			root := resourceProject(t, "sqlite:storage/development.db")
			writeFile(t, filepath.Join(root, filepath.FromSlash(existing)), "package x // hand-written\n")
			before := snapshotTree(t, root)
			_, err := runResource(root, "product", "name:string", "--global")
			if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") || !strings.Contains(err.Error(), filepath.FromSlash(existing)) {
				t.Fatalf("err = %v, want a refusal naming %s", err, existing)
			}
			if after := snapshotTree(t, root); !slices.Equal(before, after) {
				t.Fatalf("a refused run left files behind:\nbefore %v\nafter  %v", before, after)
			}
			body, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(existing)))
			if string(body) != "package x // hand-written\n" {
				t.Fatalf("existing file was modified: %q", body)
			}
		})
	}
}

func TestGenerateResourceRejectsSymlinks(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "app", "domain")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := runResource(root, "product", "name:string", "--global"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want a symlink refusal", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("the generator wrote through a symlink: %v", entries)
	}
}

func TestGenerateResourceNeedsADatabase(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct{ url, want string }{
		"none":    {"", "no database"},
		"unknown": {"oracle://u:p@localhost/db", "supported database"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := resourceProject(t, test.url)
			before := snapshotTree(t, root)
			_, err := runResource(root, "product", "name:string", "--global")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want it to contain %q", err, test.want)
			}
			if after := snapshotTree(t, root); !slices.Equal(before, after) {
				t.Fatal("a failed run changed the project")
			}
		})
	}
	if _, err := runResource(t.TempDir(), "product", "name:string", "--global"); err == nil || !strings.Contains(err.Error(), "go.mod") {
		t.Fatalf("outside an application: err = %v, want a go.mod error", err)
	}
}

func TestGenerateResourceReadsTheDatabaseFromTheProjectEnvironment(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "postgres://u:p@localhost/db")
	// The process environment wins over the env files, as everywhere in Forge.
	var out bytes.Buffer
	err := runGenerateEnv([]string{"resource", "product", "name:string", "--global"}, &out, root,
		[]string{"FORGE_DATABASE_URL=sqlite:storage/other.db"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile(filepath.Join(root, "db", "migrations", filepath.Base(firstMatch(t, root, "*_create_products.up.sql"))))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(up), "STRICT") {
		t.Fatalf("expected SQLite DDL from the process environment:\n%s", up)
	}
}

func firstMatch(t *testing.T, root, pattern string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, "db", "migrations", pattern))
	if err != nil || len(matches) != 1 {
		t.Fatalf("glob %s = %v, %v", pattern, matches, err)
	}
	return matches[0]
}

func TestGenerateResourceRefusesDeclarationConflicts(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	// Another resource whose generated names collide with product's:
	// product_input would declare ProductInput twice in package domain.
	if _, err := runResource(root, "product", "name:string", "--global"); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, root)
	_, err := runResource(root, "product_input", "name:string", "--global")
	if err == nil || !strings.Contains(err.Error(), "already declares") {
		t.Fatalf("err = %v, want a declaration conflict", err)
	}
	if after := snapshotTree(t, root); !slices.Equal(before, after) {
		t.Fatal("a conflicting run left files behind")
	}
}

func TestResourcePermissionsAreOnlySeededWithForgeIdentity(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "postgres://u:p@localhost/db")
	if _, err := runResource(root, "product", "name:string", "--tenant-scoped"); err != nil {
		t.Fatal(err)
	}
	up := readFile(t, firstMatch(t, root, "*_create_products.up.sql"))
	down := readFile(t, firstMatch(t, root, "*_create_products.down.sql"))
	for _, want := range []string{
		"INSERT INTO forge_permissions", "'products:read'", "'products:write'", "ON CONFLICT (name) DO NOTHING",
		// The owner role `setup` creates holds every permission, new ones too.
		"INSERT INTO forge_role_permissions (role_id, permission)", "WHERE r.name = 'owner'", "ON CONFLICT (role_id, permission) DO NOTHING",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("up migration is missing %q:\n%s", want, up)
		}
	}
	if strings.Index(up, "INSERT INTO forge_role_permissions") < strings.Index(up, "INSERT INTO forge_permissions") {
		t.Errorf("owner grants must follow the permissions they reference:\n%s", up)
	}
	// Grants go before the permissions they reference, and both before the table.
	grants := strings.Index(down, "DELETE FROM forge_role_permissions")
	permissions := strings.Index(down, "DELETE FROM forge_permissions")
	table := strings.Index(down, `DROP TABLE "products"`)
	if grants < 0 || permissions < grants || table < permissions {
		t.Errorf("down migration order is wrong:\n%s", down)
	}

	// A PostgreSQL application that does not compose the identity migrations
	// has no forge_permissions table to seed.
	bare := resourceProject(t, "postgres://u:p@localhost/db")
	writeFile(t, filepath.Join(bare, "cmd", "shop", "main.go"), "package main\n\nfunc main() {}\n")
	output, err := runResource(bare, "product", "name:string", "--global")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readFile(t, firstMatch(t, bare, "*_create_products.up.sql")), "forge_permissions") || strings.Contains(output, "auth.Require") {
		t.Errorf("identity wiring generated for an application without identity:\n%s", output)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// allTypes exercises every field type, required and optional.
var allTypes = []resourceField{
	{Name: "name", Type: "string", Optional: false}, {Name: "summary", Type: "string", Optional: true},
	{Name: "body", Type: "text", Optional: false}, {Name: "notes", Type: "text", Optional: true},
	{Name: "quantity", Type: "int", Optional: false}, {Name: "limit_count", Type: "int", Optional: true},
	{Name: "active", Type: "bool", Optional: false}, {Name: "featured", Type: "bool", Optional: true},
	{Name: "released_at", Type: "time", Optional: false}, {Name: "retired_at", Type: "time", Optional: true},
	{Name: "owner_id", Type: "uuid", Optional: false}, {Name: "parent_id", Type: "uuid", Optional: true},
}

var minimalFields = []resourceField{{Name: "count", Type: "int", Optional: false}, {Name: "enabled", Type: "bool", Optional: true}}

// TestResourceIdentifiersAreUUIDv7AndTypesAreExact is the guard for ADR 0021
// rule 1 and 5: for every database and both scopes, the primary key (and the
// tenant) use the database's UUID storage, the generated SQL and Go contain no
// other identifier strategy, and no column is floating point or decimal.
func TestResourceIdentifiersAreUUIDv7AndTypesAreExact(t *testing.T) {
	t.Parallel()
	forbidden := regexp.MustCompile(`(?i)\b(serial|bigserial|smallserial|auto_increment|autoincrement|gen_random_uuid|uuid_generate_v[0-9a-z]+|float|float4|float8|real|double|decimal|numeric|money)\b`)
	mysqlUUIDFunction := regexp.MustCompile(`\bUUID\(\)`)
	idColumn := map[string]*regexp.Regexp{
		"postgres": regexp.MustCompile(`(?m)^\s+"id"\s+uuid PRIMARY KEY,?$`),
		"mysql":    regexp.MustCompile("(?m)^\\s+`id`\\s+CHAR\\(36\\) NOT NULL PRIMARY KEY,?$"),
		"sqlite":   regexp.MustCompile(`(?m)^\s+"id"\s+TEXT NOT NULL PRIMARY KEY,?$`),
	}
	tenantColumn := map[string]*regexp.Regexp{
		"postgres": regexp.MustCompile(`(?m)^\s+"org_id"\s+uuid NOT NULL REFERENCES forge_tenants \(id\),?$`),
		// The type and collation of forge_tenants.id, which the foreign key
		// below needs.
		"mysql":  regexp.MustCompile("(?m)^\\s+`org_id`\\s+CHAR\\(36\\) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,?$"),
		"sqlite": regexp.MustCompile(`(?m)^\s+"org_id"\s+TEXT NOT NULL REFERENCES forge_tenants \(id\),?$`),
	}
	// org_id names an organization: a foreign key on the databases without
	// row-level security.
	tenantForeignKey := map[string]string{
		"postgres": `REFERENCES forge_tenants (id)`,
		"mysql":    "CONSTRAINT `products_org_id_fk` FOREIGN KEY (`org_id`) REFERENCES forge_tenants (id)",
		"sqlite":   `REFERENCES forge_tenants (id)`,
	}
	for _, database := range resourceDatabases {
		for _, tenant := range []bool{true, false} {
			for name, fields := range map[string][]resourceField{"every type": allTypes, "minimal": minimalFields} {
				t.Run(database.name+"/"+map[bool]string{true: "tenant", false: "global"}[tenant]+"/"+name, func(t *testing.T) {
					t.Parallel()
					spec := resourceSpec{
						resourceRequest: resourceRequest{Name: "product", Plural: "products", Fields: fields, TenantScoped: tenant},
						Module:          "example.com/shop", Database: database.name, Identity: database.name == "postgres",
					}
					rendered, err := renderResource(spec)
					if err != nil {
						t.Fatal(err)
					}
					up := rendered.migrationUp
					if !idColumn[database.name].MatchString(up) {
						t.Errorf("the id column is not the %s UUIDv7 storage:\n%s", database.name, up)
					}
					if tenant && !tenantColumn[database.name].MatchString(up) {
						t.Errorf("org_id is not the %s UUID storage:\n%s", database.name, up)
					}
					if want := tenantForeignKey[database.name]; tenant && want != "" && !strings.Contains(up, want) {
						t.Errorf("org_id is not a foreign key to forge_tenants on %s:\n%s", database.name, up)
					}
					if !tenant && strings.Contains(up, "org_id") {
						t.Errorf("a global table has an org_id column:\n%s", up)
					}
					if database.name == "sqlite" && !strings.Contains(up, ") STRICT;") {
						t.Errorf("SQLite table is not STRICT:\n%s", up)
					}
					texts := []string{up, rendered.migrationDown}
					for _, source := range rendered.sources {
						texts = append(texts, source.content)
					}
					for _, text := range texts {
						if match := forbidden.FindString(text); match != "" {
							t.Errorf("generated output contains %q", match)
						}
					}
					if mysqlUUIDFunction.MatchString(up) {
						t.Errorf("generated SQL calls UUID():\n%s", up)
					}
					// IDs come from the application only; uuidv7() would also
					// tie the table to PostgreSQL 18.
					if strings.Contains(strings.ToLower(up), "uuidv7(") {
						t.Errorf("generated SQL calls uuidv7():\n%s", up)
					}
					for _, source := range rendered.sources {
						if strings.Contains(source.content, "google/uuid") || strings.Contains(source.content, "uuid.NewString") {
							t.Errorf("%s%s generates identifiers outside forge's uuid package", source.dir, source.suffix)
						}
					}
					assertGoSourcesParse(t, rendered)
				})
			}
		}
	}
}

func assertGoSourcesParse(t *testing.T, rendered renderedResource) {
	t.Helper()
	for _, source := range rendered.sources {
		formatted, err := format.Source([]byte(source.content))
		if err != nil {
			t.Errorf("%s%s does not format: %v", source.dir, source.suffix, err)
			continue
		}
		if _, err := parser.ParseFile(token.NewFileSet(), "x.go", formatted, parser.AllErrors); err != nil {
			t.Errorf("%s%s does not parse: %v", source.dir, source.suffix, err)
		}
	}
}

func TestResourceTenantScopeShapesTheGeneratedCode(t *testing.T) {
	t.Parallel()
	render := func(database string, tenant bool) renderedResource {
		rendered, err := renderResource(resourceSpec{
			resourceRequest: resourceRequest{Name: "product", Plural: "products", Fields: allTypes, TenantScoped: tenant},
			Module:          "example.com/shop", Database: database, Identity: database == "postgres",
		})
		if err != nil {
			t.Fatal(err)
		}
		return rendered
	}
	persistence := func(rendered renderedResource) string {
		for _, source := range rendered.sources {
			if source.dir == "app/adapters/persistence" && source.suffix == ".go" {
				return source.content
			}
		}
		t.Fatal("no persistence source")
		return ""
	}
	for _, database := range []string{"postgres", "mysql", "sqlite"} {
		scoped := persistence(render(database, true))
		global := persistence(render(database, false))
		if !strings.Contains(scoped, "tenancy.Require(ctx)") || !strings.Contains(scoped, "org_id") || !strings.Contains(scoped, "orgID") {
			t.Errorf("%s tenant-scoped repository does not bind the tenant", database)
		}
		if strings.Contains(global, "tenancy") || strings.Contains(global, "org_id") {
			t.Errorf("%s global repository mentions tenants", database)
		}
	}
	if up := render("postgres", true).migrationUp; !strings.Contains(up, "FORCE ROW LEVEL SECURITY") || !strings.Contains(up, `CREATE POLICY "products_org"`) ||
		!strings.Contains(up, "WITH CHECK") || !strings.Contains(up, "BYPASSRLS maintenance role") || !strings.Contains(persistence(render("postgres", true)), "InTenantTx") {
		t.Errorf("PostgreSQL tenant-scoped resources must enable forced RLS and run in InTenantTx:\n%s", up)
	}
	for _, database := range []string{"mysql", "sqlite"} {
		if up := render(database, true).migrationUp; strings.Contains(up, "ROW LEVEL SECURITY") || !strings.Contains(up, "org_id") || !strings.Contains(up, "_org_id_idx") {
			t.Errorf("%s: want an (org_id, id) index and no RLS:\n%s", database, up)
		}
	}
	if up := render("postgres", false).migrationUp; strings.Contains(up, "ROW LEVEL SECURITY") {
		t.Errorf("a global table has RLS:\n%s", up)
	}
}

func TestResourceWiringNeverOpensTheRoutes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		database         string
		identity, bearer bool
	}{{"postgres", true, true}, {"postgres", true, false}, {"postgres", false, false}, {"mysql", false, false}, {"sqlite", false, false}} {
		for _, scaffold := range []bool{false, true} {
			spec := resourceSpec{
				resourceRequest: resourceRequest{Name: "product", Plural: "products", TenantScoped: true, Scaffold: scaffold},
				Module:          "example.com/shop", Database: test.database, Identity: test.identity, BearerSignIn: test.bearer,
			}
			parts := buildResourceWiring(spec)
			wiring := resourceWiring(spec)
			if strings.Contains(wiring, "return next, nil") {
				t.Errorf("%+v: the wiring passes requests through:\n%s", test, wiring)
			}
			// With Forge identity the JSON guard checks the permission behind
			// the bearer token where the application issues them
			// (--api-auth), else behind the session cookie plus the CSRF
			// token of every change. Without identity it refuses everyone.
			if authenticates := strings.Contains(parts.Guard, "auth.RequireAudited(required, next, repository)"); authenticates != test.identity {
				t.Errorf("%+v: the JSON guard authenticates = %v:\n%s", test, authenticates, parts.Guard)
			}
			if test.identity && !test.bearer && (!strings.Contains(parts.Guard, "auth.RequireCSRF(guarded)") || !strings.Contains(parts.Guard, "middleware.AuthenticateCookie(checked)") ||
				!strings.Contains(wiring, "forge generate authentication --api-auth")) {
				t.Errorf("%+v: the JSON routes of an application without bearer sign-in do not use the session cookie and CSRF token, or bearer tokens are not explained:\n%s", test, wiring)
			}
			if test.identity && test.bearer && (!strings.Contains(parts.Guard, "middleware.Authenticate(guarded)") || strings.Contains(parts.Guard, "AuthenticateCookie")) {
				t.Errorf("%+v: the JSON routes of an application with bearer sign-in do not authenticate the bearer token:\n%s", test, parts.Guard)
			}
			if !test.identity && !strings.Contains(parts.Guard, "web.Error(w, r, fault.New(\"permission_denied\"") {
				t.Errorf("%+v: the guard of an application without identity does not refuse everyone:\n%s", test, parts.Guard)
			}
			if !strings.Contains(wiring, "httpapi.RegisterProducts(app, productService, protect)") {
				t.Errorf("%+v: wiring does not register the routes with a guard:\n%s", test, wiring)
			}
			if scaffold && test.identity && !strings.Contains(wiring, "middleware.AuthenticateBrowser(\"/session/new\", guarded)") {
				t.Errorf("%+v: the scaffold's pages lost their cookie guard:\n%s", test, wiring)
			}
		}
	}
}

// The bearer login is detected from bootstrap.go: forge new --api-auth
// mounts it, a default application does not.
func TestBearerSignInFollowsTheMountedLoginRoute(t *testing.T) {
	t.Parallel()
	for _, apiAuth := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "shop")
		args := []string{"new", "shop", "--dir", dir, "--database", "postgresql", "--skip-deps"}
		if apiAuth {
			args = append(args, "--api-auth")
		}
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("forge new: %d %s", code, stderr.String())
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := hasBearerSignIn(root); got != apiAuth {
			t.Errorf("--api-auth=%v: hasBearerSignIn = %v", apiAuth, got)
		}
		_ = root.Close()
	}
}

// TestReservedGoMembersCoverEveryGeneratedMethod derives the methods of the
// generated entity types from the rendered code and fails when one is missing
// from the list that field names are checked against.
func TestReservedGoMembersCoverEveryGeneratedMethod(t *testing.T) {
	t.Parallel()
	rendered, err := renderResource(resourceSpec{
		resourceRequest: resourceRequest{Name: "product", Plural: "products", Fields: allTypes, TenantScoped: true},
		Module:          "example.com/shop", Database: "postgres", Identity: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, source := range rendered.sources {
		if source.dir != "app/domain" || source.suffix != ".go" {
			continue
		}
		file, err := parser.ParseFile(fset, "product.go", source.content, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv != nil {
				checked++
				if !slices.Contains(reservedGoMembers, function.Name.Name) {
					t.Errorf("method %s of a generated type is not in reservedGoMembers", function.Name.Name)
				}
			}
		}
		// The entity's own fields are reserved too.
		for _, name := range []string{"ID", "Version", "CreatedAt", "UpdatedAt"} {
			if !strings.Contains(source.content, "\t"+name+" ") {
				t.Errorf("the domain no longer declares %s; update reservedGoMembers", name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no generated methods found")
	}
}

func TestLowerCamel(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{"product": "product", "order_item": "orderItem", "api_key": "apiKey", "id_card": "idCard"} {
		if got := lowerCamel(name); got != want {
			t.Errorf("lowerCamel(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestResourceVersionColumnIsWide: the version travels in the URL and the
// body, so a value beyond 32 bits must be comparable in SQL instead of
// overflowing an integer column.
func TestResourceVersionColumnIsWide(t *testing.T) {
	t.Parallel()
	want := map[string]*regexp.Regexp{
		"postgres": regexp.MustCompile(`"version"\s+bigint NOT NULL DEFAULT 1`),
		"mysql":    regexp.MustCompile("`version`\\s+BIGINT NOT NULL DEFAULT 1"),
		"sqlite":   regexp.MustCompile(`"version"\s+INTEGER NOT NULL DEFAULT 1`),
	}
	for database, pattern := range want {
		rendered, err := renderResource(resourceSpec{
			resourceRequest: resourceRequest{Name: "product", Plural: "products", Fields: minimalFields},
			Module:          "example.com/shop", Database: database,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !pattern.MatchString(rendered.migrationUp) {
			t.Errorf("%s: the version column is not 64-bit:\n%s", database, rendered.migrationUp)
		}
	}
}

// Roles are per tenant, so a tenant administrator can grant itself any
// permission: writes to global rows must not be guarded by a role alone.
func TestGlobalResourceWritesStayClosedInIdentityApplications(t *testing.T) {
	t.Parallel()
	identity := resourceSpec{
		resourceRequest: resourceRequest{Name: "category", Plural: "categories", Global: true},
		Module:          "example.com/shop", Database: "postgres", Identity: true, Organizations: true,
	}
	wiring := resourceWiring(identity)
	for _, want := range []string{
		"protectPlatformWrite := func(",
		`permission == "categories:write"`,
		"return protectPlatformWrite(permission, next)",
		"return protect(permission, next)",
		"auth.ErrPermissionDenied",
	} {
		if !strings.Contains(wiring, want) {
			t.Errorf("global wiring is missing %q:\n%s", want, wiring)
		}
	}
	// Tenant-scoped resources are written by their own tenant's roles.
	identity.TenantScoped = true
	if wiring := resourceWiring(identity); strings.Contains(wiring, "protectPlatformWrite") {
		t.Errorf("a tenant-scoped resource needs no platform guard:\n%s", wiring)
	}
	// So are the plain tables of an application with one organization
	// (decision D33): its owner writes them.
	identity.TenantScoped, identity.Global, identity.Organizations = false, false, false
	if wiring := resourceWiring(identity); strings.Contains(wiring, "protectPlatformWrite") {
		t.Errorf("a single-organization resource needs no platform guard:\n%s", wiring)
	}
	// Applications without identity already get a deny-all guard for everything.
	identity.TenantScoped, identity.Identity, identity.Database = false, false, "mysql"
	if wiring := resourceWiring(identity); strings.Contains(wiring, "protectPlatformWrite") || !strings.Contains(wiring, "no Forge identity") {
		t.Errorf("unexpected wiring without identity:\n%s", wiring)
	}
}

func TestLooksPlural(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{"products": "product", "categories": "category", "boxes": "box", "matches": "match", "order_items": "order_item"} {
		if got, ok := looksPlural(name); !ok || got != want {
			t.Errorf("looksPlural(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{"product", "status", "address", "bus", "analysis", "photos", "class", "key", "category"} {
		if got, ok := looksPlural(name); ok && name != "photos" {
			t.Errorf("looksPlural(%q) = %q, want a singular name to pass", name, got)
		}
	}
}

func TestPluralNameIsAcceptedWithExplicitPlural(t *testing.T) {
	t.Parallel()
	got, err := parseResourceArgs([]string{"news", "title:string", "--global", "--plural", "news_items"})
	if err != nil || got.Plural != "news_items" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestGenerateResourceRefusesAnExistingTableMigration(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	writeFile(t, filepath.Join(root, "db", "migrations", "20260101000000_create_products.up.sql"), "CREATE TABLE products (id TEXT);\n")
	before := snapshotTree(t, root)
	_, err := runResource(root, "product", "name:string", "--global")
	if err == nil || !strings.Contains(err.Error(), "already creates the table products") {
		t.Fatalf("err = %v, want a refusal naming the existing migration", err)
	}
	if after := snapshotTree(t, root); !slices.Equal(before, after) {
		t.Fatal("a refused run left files behind")
	}
}
