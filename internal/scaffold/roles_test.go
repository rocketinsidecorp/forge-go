package scaffold_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

// composeEnvironment returns each compose service's environment, with the
// YAML merge keys (<<: *app, <<: *app-environment) resolved as compose
// resolves them.
func composeEnvironment(t *testing.T, dir string) map[string]map[string]string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(content, &compose); err != nil {
		t.Fatalf("compose.yaml: %v\n%s", err, content)
	}
	environments := make(map[string]map[string]string, len(compose.Services))
	for name, service := range compose.Services {
		environments[name] = service.Environment
	}
	return environments
}

// A PostgreSQL application serves as app_runtime and migrates as
// app_migrator: only the one-off migrate container receives the migration
// credential, and the long-running app never does.
func TestPostgreSQLServesAndMigratesAsSeparateRoles(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "shop")
	if _, err := scaffold.Generate(scaffold.Options{Name: "shop", Dir: dir, Database: "postgresql"}); err != nil {
		t.Fatal(err)
	}
	environments := composeEnvironment(t, dir)
	runtime := "postgres://app_runtime:app_runtime@postgres:5432/shop_development?sslmode=disable"
	migration := "postgres://app_migrator:app_migrator@postgres:5432/shop_development?sslmode=disable"
	if app := environments["app"]; app["FORGE_DATABASE_URL"] != runtime || app["FORGE_MIGRATION_DATABASE_URL"] != "" {
		t.Errorf("app environment = %v, want only the runtime role", app)
	}
	if migrate := environments["migrate"]; migrate["FORGE_DATABASE_URL"] != runtime || migrate["FORGE_MIGRATION_DATABASE_URL"] != migration || migrate["FORGE_ENV"] != "development" {
		t.Errorf("migrate environment = %v, want the app's plus the migration role", migrate)
	}
	if database := environments["postgres"]; database["POSTGRES_USER"] != "postgres" || database["POSTGRES_DB"] != "postgres" {
		t.Errorf("postgres environment = %v, want the image's superuser bootstrapping init.sql", database)
	}
}

func TestOnlyPostgreSQLApplicationsHaveSeparateRoles(t *testing.T) {
	t.Parallel()
	for _, database := range []string{"", "sqlite", "mysql", "mariadb"} {
		dir := filepath.Join(t.TempDir(), "shop")
		if _, err := scaffold.Generate(scaffold.Options{Name: "shop", Dir: dir, Database: database}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "docker", "postgres", "init.sql")); !os.IsNotExist(err) {
			t.Errorf("database %q: docker/postgres/init.sql exists (%v)", database, err)
		}
		for _, name := range []string{"compose.yaml", ".env.development", filepath.Join("app", "bootstrap", "bootstrap.go")} {
			content, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			for _, unexpected := range []string{"FORGE_MIGRATION_DATABASE_URL", "app_runtime", "RequireRowSecurity"} {
				if strings.Contains(string(content), unexpected) {
					t.Errorf("database %q: %s mentions %s", database, name, unexpected)
				}
			}
		}
	}
}
