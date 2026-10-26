package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/mysql/mysqltest"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
)

var migrationDatabases = []struct{ adapter, url string }{
	{"postgres", "postgres://app:app@127.0.0.1:5432/app?sslmode=disable"},
	{"mysql", "mysql://app:app@127.0.0.1:3306/app"},
	{"sqlite", "sqlite:storage/development.db"},
}

func generateMigrationFiles(t *testing.T, root string, args ...string) (output, up, down string) {
	t.Helper()
	var out bytes.Buffer
	if err := runGenerateEnv(append([]string{"migration"}, args...), &out, root, nil, seamNow); err != nil {
		t.Fatalf("forge generate migration %v: %v\n%s", args, err, out.String())
	}
	ups, err := filepath.Glob(filepath.Join(root, "db", "migrations", "*_"+args[0]+".up.sql"))
	if err != nil || len(ups) != 1 {
		t.Fatalf("up migrations: %v %v", ups, err)
	}
	return out.String(), readFile(t, ups[0]), readFile(t, strings.TrimSuffix(ups[0], ".up.sql")+".down.sql")
}

func TestGenerateMigrationInfersAddAndRemoveColumns(t *testing.T) {
	t.Parallel()
	want := map[string][2]string{
		"postgres": {
			`ALTER TABLE "products" ADD COLUMN "sku" text NOT NULL DEFAULT '' CONSTRAINT "products_sku_length" CHECK (char_length("sku") <= 255);` + "\n" +
				`ALTER TABLE "products" ADD COLUMN "stock" bigint NOT NULL DEFAULT 0;` + "\n" +
				`ALTER TABLE "products" ADD COLUMN "released_at" timestamptz;`,
			`ALTER TABLE "products" DROP COLUMN "sku";`,
		},
		"mysql": {
			"ALTER TABLE `products` ADD COLUMN `sku` VARCHAR(255) NOT NULL DEFAULT '';\n" +
				"ALTER TABLE `products` ADD COLUMN `stock` BIGINT NOT NULL DEFAULT 0;\n" +
				"ALTER TABLE `products` ADD COLUMN `released_at` DATETIME(6);",
			"ALTER TABLE `products` DROP COLUMN `sku`;",
		},
		"sqlite": {
			`ALTER TABLE "products" ADD COLUMN "sku" TEXT NOT NULL DEFAULT '' CONSTRAINT "products_sku_length" CHECK (length("sku") <= 255);` + "\n" +
				`ALTER TABLE "products" ADD COLUMN "stock" INTEGER NOT NULL DEFAULT 0;` + "\n" +
				`ALTER TABLE "products" ADD COLUMN "released_at" TEXT;`,
			`ALTER TABLE "products" DROP COLUMN "sku";`,
		},
	}
	for _, database := range migrationDatabases {
		t.Run(database.adapter, func(t *testing.T) {
			t.Parallel()
			root := resourceProject(t, database.url)
			output, up, down := generateMigrationFiles(t, root, "add_sku_to_products", "sku:string", "stock:int", "released_at:time?")
			if !strings.Contains(up, want[database.adapter][0]) || !strings.Contains(down, want[database.adapter][1]) || strings.Contains(output, "WARNING") {
				t.Errorf("add:\n%s\n%s\n%s", output, up, down)
			}
			_, up, down = generateMigrationFiles(t, root, "remove_sku_from_products", "sku:string")
			if !strings.Contains(up, want[database.adapter][1]) || !strings.Contains(down, strings.SplitN(want[database.adapter][0], "\n", 2)[0]) {
				t.Errorf("remove:\n%s\n%s", up, down)
			}
		})
	}
}

func TestGenerateMigrationWithoutInferenceIsBlankAndWarns(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	for _, name := range []string{"backfill_prices", "add_sku_to_products"} {
		output, up, _ := generateMigrationFiles(t, root, name)
		if !strings.Contains(output, "WARNING: fill in db/migrations/") || !strings.Contains(output, "_"+name+".up.sql and db/migrations/") || !strings.Contains(output, "an empty migration is refused") || strings.Contains(up, "ALTER TABLE") {
			t.Errorf("%s:\n%s\n%s", name, output, up)
		}
	}
	for _, args := range [][]string{
		{"create_widgets", "name:string"},
		{"add_at_to_products", "at:time"},
		{"add_id_to_products", "id:uuid?"},
		{"add_x_to_products", "x:string", "x:int"},
		{"add_x_to_forge_users", "x:string"},
		{"Add_X", "x:string"},
		{"add_x_to_products", "x:float"},
		{},
	} {
		var out bytes.Buffer
		if err := runGenerateEnv(append([]string{"migration"}, args...), &out, root, nil, seamNow); err == nil {
			t.Errorf("forge generate migration %v succeeded:\n%s", args, out.String())
		}
	}
	none := resourceProject(t, "")
	var out bytes.Buffer
	if err := runGenerateEnv([]string{"migration", "add_x_to_products", "x:string"}, &out, none, withoutDatabaseURL(), seamNow); err == nil || !strings.Contains(err.Error(), "FORGE_DATABASE_URL") {
		t.Errorf("an inferred migration without a database: %v", err)
	}
	if err := runGenerateEnv([]string{"migration", "add_x_to_products", "x:string", "--dry-run"}, &out, root, nil, seamNow); err != nil || !strings.Contains(out.String(), "would create") {
		t.Errorf("--dry-run: %v\n%s", err, out.String())
	}
}

// The generated statements run on every database: add to a table with rows,
// then revert, and remove then revert.
func TestGeneratedColumnMigrationsRunOnEveryDatabase(t *testing.T) {
	t.Parallel()
	for _, database := range migrationDatabases {
		t.Run(database.adapter, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			var exec func(string) error
			switch database.adapter {
			case "postgres":
				db := postgrestest.New(t)
				exec = func(sql string) error { _, err := db.Exec(ctx, sql); return err }
			case "mysql":
				db := mysqltest.New(t)
				exec = func(sql string) error { _, err := db.ExecContext(ctx, sql); return err }
			default:
				db := sqlitetest.New(t)
				exec = func(sql string) error { _, err := db.ExecContext(ctx, sql); return err }
			}
			run := func(script string) {
				t.Helper()
				for _, statement := range strings.Split(script, ";\n") {
					var code []string
					for _, line := range strings.Split(statement, "\n") {
						if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "--") {
							code = append(code, line)
						}
					}
					if len(code) == 0 {
						continue
					}
					if err := exec(strings.Join(code, "\n")); err != nil {
						t.Fatalf("%s: %v", statement, err)
					}
				}
			}
			table := "CREATE TABLE products (id VARCHAR(36) NOT NULL PRIMARY KEY, name VARCHAR(255) NOT NULL)"
			if database.adapter == "sqlite" {
				table = "CREATE TABLE products (id TEXT NOT NULL PRIMARY KEY, name TEXT NOT NULL) STRICT"
			}
			run(table + ";\nINSERT INTO products (id, name) VALUES ('1', 'Lamp');\n")
			root := resourceProject(t, database.url)
			_, up, down := generateMigrationFiles(t, root, "add_details_to_products", "sku:string", "notes:text", "stock:int", "active:bool", "released_at:time?", "owner_id:uuid?")
			run(up)
			run(down)
			run(up)
			_, removeUp, removeDown := generateMigrationFiles(t, root, "remove_sku_from_products", "sku:string")
			run(removeUp)
			run(removeDown)
		})
	}
}
