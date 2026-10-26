package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

func serverVersion(t *testing.T, db *postgres.DB) int {
	t.Helper()
	var version int
	if err := db.QueryRow(context.Background(), "SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func uuidv7Migrator(t *testing.T, cfg config.Database, set fstest.MapFS) *migrate.Migrator {
	t.Helper()
	loaded, err := migrate.LoadMigrations(set)
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := postgres.NewMigrator(cfg, loaded, discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = migrator.Close() })
	return migrator
}

// A migration calling uuidv7() on a server without it fails with the
// server version and the way out, keeps the PostgreSQL error, and leaves
// nothing applied. A wrong argument list raises the same SQLSTATE 42883 on
// every version.
func TestMissingUUIDv7FailsWithTheServerVersion(t *testing.T) {
	db, cfg := postgrestest.NewWithConfig(t)
	ctx := context.Background()
	migrator := uuidv7Migrator(t, cfg, fstest.MapFS{
		"20260101000000_things.up.sql": {Data: []byte("CREATE TABLE things (id uuid PRIMARY KEY);\nSELECT uuidv7('not', 'a', 'signature');")},
	})
	_, err := migrator.Up(ctx)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42883" ||
		!strings.Contains(err.Error(), "uuidv7() needs PostgreSQL 18, or a uuidv7() function defined by an earlier migration") ||
		!strings.Contains(err.Error(), "this server is PostgreSQL ") || strings.Contains(err.Error(), "PostgreSQL unknown") {
		t.Fatalf("err = %v, want the PostgreSQL error explained with the server version", err)
	}
	var exists bool
	if err := db.QueryRow(ctx, "SELECT to_regclass('things') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("the failed migration left its table behind: %v, %v", exists, err)
	}

	plain := uuidv7Migrator(t, postgrestest.Config(t), fstest.MapFS{
		"20260101000000_things.up.sql": {Data: []byte("CREATE TABLE things (id uuid PRIMARY KEY DEFAULT uuidv7());")},
	})
	_, err = plain.Up(ctx)
	if serverVersion(t, db) >= 180000 {
		if err != nil {
			t.Fatalf("DEFAULT uuidv7() on PostgreSQL 18: %v", err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "uuidv7() needs PostgreSQL 18") {
		t.Fatalf("DEFAULT uuidv7() before PostgreSQL 18: err = %v, want the explanation", err)
	}
}

// Regression: an application that defines its own uuidv7() (a polyfill on
// PostgreSQL 17 or older) and then uses it migrates, reports its status and
// rolls back; nothing scans the scripts in advance.
func TestUUIDv7PolyfillMigratesOnAnyVersion(t *testing.T) {
	db, cfg := postgrestest.NewWithConfig(t)
	ctx := context.Background()
	set := fstest.MapFS{
		"20260101000000_uuidv7.up.sql": {Data: []byte("CREATE FUNCTION uuidv7() RETURNS uuid LANGUAGE sql AS $$ SELECT gen_random_uuid() $$;\n" +
			"SELECT 'uuidv7(' AS in_a_string_literal;")},
		"20260101000000_uuidv7.down.sql": {Data: []byte("DROP FUNCTION uuidv7();")},
		"20260102000000_things.up.sql":   {Data: []byte("CREATE TABLE things (id uuid PRIMARY KEY DEFAULT uuidv7(), name text);\nINSERT INTO things (name) VALUES ('a');")},
		"20260102000000_things.down.sql": {Data: []byte("DROP TABLE things;")},
	}
	if applied, err := uuidv7Migrator(t, cfg, set).Up(ctx); err != nil || len(applied) != 2 {
		t.Fatalf("Up = %d migrations, %v; want both applied (server %d)", len(applied), err, serverVersion(t, db))
	}
	statuses, err := uuidv7Migrator(t, cfg, set).Status(ctx)
	if err != nil || len(statuses) != 2 || statuses[1].State != migrate.StateApplied {
		t.Fatalf("Status = %v, %v", statuses, err)
	}
	if reverted, err := uuidv7Migrator(t, cfg, set).Down(ctx, 1); err != nil || len(reverted) != 1 {
		t.Fatalf("Down = %v, %v", reverted, err)
	}
}
