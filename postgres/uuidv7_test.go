package postgres_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

// Migrations that call uuidv7() run on PostgreSQL 18 and are refused with a
// clear error, before anything is applied, on an older server.
func TestMigrationsCallingUUIDv7NeedPostgreSQL18(t *testing.T) {
	db, cfg := postgrestest.NewWithConfig(t)
	ctx := context.Background()
	var version int
	if err := db.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatal(err)
	}
	migrations := fstest.MapFS{
		"20260101000000_things.up.sql": {Data: []byte("CREATE TABLE things (id uuid PRIMARY KEY DEFAULT uuidv7());")},
	}
	loaded, err := migrate.LoadMigrations(migrations)
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := postgres.NewMigrator(cfg, loaded, discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrator.Close() }()
	_, err = migrator.Up(ctx)
	if version >= 180000 {
		if err != nil {
			t.Fatalf("PostgreSQL %d: %v", version, err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "20260101000000_things call uuidv7(), which needs PostgreSQL 18") {
		t.Fatalf("PostgreSQL %d: err = %v, want the PostgreSQL 18 requirement", version, err)
	}
}
