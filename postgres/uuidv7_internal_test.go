package postgres

import (
	"slices"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/migrate"
)

func TestUUIDv7MigrationsIgnoreComments(t *testing.T) {
	t.Parallel()
	migrations := []migrate.Migration{
		{Version: 1, Name: "default", Up: migrate.Script{SQL: "CREATE TABLE a (id uuid PRIMARY KEY DEFAULT uuidv7());"}},
		{Version: 2, Name: "spaced", Up: migrate.Script{SQL: "SELECT UUIDv7 ();"}},
		{Version: 3, Name: "line_comment", Up: migrate.Script{SQL: "-- the uuidv7() default is gone\nCREATE TABLE b (id uuid PRIMARY KEY);"}},
		{Version: 4, Name: "block_comment", Up: migrate.Script{SQL: "/* uuidv7()\n */ CREATE TABLE c (id uuid PRIMARY KEY);"}},
		{Version: 5, Name: "other_function", Up: migrate.Script{SQL: "SELECT my_uuidv7(); SELECT uuidv7_text;"}},
	}
	if got, want := uuidv7Migrations(migrations), []string{"1_default", "2_spaced"}; !slices.Equal(got, want) {
		t.Fatalf("uuidv7Migrations = %v, want %v", got, want)
	}
}

func TestRequireUUIDv7(t *testing.T) {
	t.Parallel()
	needs := []string{"20260927090000_identity", "20260927200000_totp_mfa"}
	if err := requireUUIDv7(180000, "18.0", needs); err != nil {
		t.Fatalf("PostgreSQL 18: %v", err)
	}
	if err := requireUUIDv7(160015, "16.15", nil); err != nil {
		t.Fatalf("no uuidv7() migration on PostgreSQL 16: %v", err)
	}
	err := requireUUIDv7(170006, "17.6", needs)
	if err == nil || !strings.Contains(err.Error(), "needs PostgreSQL 18 or later, and this server is PostgreSQL 17.6") ||
		!strings.Contains(err.Error(), "20260927090000_identity, 20260927200000_totp_mfa") {
		t.Fatalf("PostgreSQL 17: err = %v", err)
	}
}
