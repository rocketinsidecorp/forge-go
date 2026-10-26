package config_test

import (
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
)

func migrationLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
}

func TestMigrationDatabaseUsesTheMigrationRoleOnTheServedDatabase(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Database.URL = config.NewSecret("postgres://app_runtime:rt@db:5432/shop?sslmode=disable")
	cfg.Database.MaxConns = 7

	got, err := cfg.MigrationDatabase(migrationLookup(map[string]string{
		config.EnvMigrationDatabaseURL: "postgres://app_migrator:m%40pw@direct-db:6432/other?sslmode=disable&application_name=migrate",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := "postgres://app_migrator:m%40pw@direct-db:6432/shop?sslmode=disable&application_name=migrate"
	if got.URL.Reveal() != want {
		t.Fatalf("URL = %q, want %q (the migration role on the served database)", got.URL.Reveal(), want)
	}
	if got.MaxConns != 7 || cfg.Database.URL.Reveal() != "postgres://app_runtime:rt@db:5432/shop?sslmode=disable" {
		t.Fatalf("pool settings not kept, or the serving configuration changed: %#v", got)
	}

	mysql := config.Default()
	mysql.Database.URL = config.NewSecret("mysql://app:pw@db:3306/shop")
	got, err = mysql.MigrationDatabase(migrationLookup(map[string]string{config.EnvMigrationDatabaseURL: "mysql://owner:pw@db:3306/ignored"}))
	if err != nil || got.URL.Reveal() != "mysql://owner:pw@db:3306/shop" {
		t.Fatalf("mysql: %v, %v", got.URL.Reveal(), err)
	}
}

func TestMigrationDatabaseFallsBackToTheServingURL(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		served string
		env    map[string]string
	}{
		"unset":               {"postgres://app:pw@db/shop?sslmode=disable", nil},
		"empty":               {"postgres://app:pw@db/shop?sslmode=disable", map[string]string{config.EnvMigrationDatabaseURL: ""}},
		"no database":         {"", map[string]string{config.EnvMigrationDatabaseURL: "postgres://m:pw@db/shop?sslmode=disable"}},
		"sqlite has no roles": {"sqlite:storage/app.db", map[string]string{config.EnvMigrationDatabaseURL: "sqlite:storage/other.db"}},
	} {
		cfg := config.Default()
		cfg.Database.URL = config.NewSecret(test.served)
		got, err := cfg.MigrationDatabase(migrationLookup(test.env))
		if err != nil || got.URL.Reveal() != test.served {
			t.Errorf("%s: URL = %q, %v; want the serving URL", name, got.URL.Reveal(), err)
		}
	}
}

func TestMigrationDatabaseRefusesInvalidURLsWithoutLeakingThem(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		environment config.Environment
		migration   string
		want        string
	}{
		"other adapter":        {config.Development, "mysql://m:secret-pw@db/shop", "selects the mysql adapter"},
		"invalid":              {config.Development, "postgres://m:secret-pw@/shop", "FORGE_MIGRATION_DATABASE_URL is invalid"},
		"production plaintext": {config.Production, "postgres://m:secret-pw@db/shop?sslmode=disable", "FORGE_MIGRATION_DATABASE_URL is invalid: production requires"},
		"dbname parameter":     {config.Development, "postgres://m:secret-pw@db/shop?sslmode=disable&dbname=other", `"dbname"`},
		"service parameter":    {config.Development, "postgres://m:secret-pw@db/shop?sslmode=disable&Service=x", `"service"`},
	} {
		cfg := config.Default()
		cfg.Environment = test.environment
		cfg.Database.URL = config.NewSecret("postgres://app:pw@db/shop?sslmode=verify-full")
		_, err := cfg.MigrationDatabase(migrationLookup(map[string]string{config.EnvMigrationDatabaseURL: test.migration}))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: err = %v, want %q", name, err, test.want)
		}
		if err != nil && strings.Contains(err.Error(), "secret-pw") {
			t.Errorf("%s: the error leaks the URL: %v", name, err)
		}
	}
}
