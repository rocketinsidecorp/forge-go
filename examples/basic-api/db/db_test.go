package db_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/rocketinsidecorp/forge-go/examples/basic-api/db"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

func TestMigrationsLoad(t *testing.T) {
	t.Parallel()
	migrations, err := migrate.LoadMigrations(db.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) == 0 || migrations[0].Name != "create_tasks" || migrations[0].Down.SQL == "" {
		t.Fatalf("migrations = %+v", migrations)
	}
}

// TestMigrationsRoundTrip proves every migration can be rolled back and
// reapplied, so "rollback" is safe to rely on in production.
func TestMigrationsRoundTrip(t *testing.T) {
	t.Parallel()
	database, cfg := postgrestest.NewWithConfig(t)
	migrations, err := migrate.LoadMigrations(db.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := postgres.NewMigrator(cfg, migrations, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = migrator.Close() })
	ctx := context.Background()

	if applied, err := migrator.Up(ctx); err != nil || len(applied) != len(migrations) {
		t.Fatalf("up: applied %d, err %v", len(applied), err)
	}
	if _, err := migrator.Down(ctx, len(migrations)); err != nil {
		t.Fatalf("down: %v", err)
	}
	var tables int
	if err := database.QueryRow(ctx, "SELECT count(*) FROM pg_tables WHERE tablename = 'tasks'").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("tasks table after rollback: count %d, err %v", tables, err)
	}
	if applied, err := migrator.Up(ctx); err != nil || len(applied) != len(migrations) {
		t.Fatalf("up again: applied %d, err %v", len(applied), err)
	}
}
