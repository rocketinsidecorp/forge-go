package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
)

func TestSeedCommandCommitsOnlyASuccessfulSeed(t *testing.T) {
	t.Parallel()
	db, database := sqlitetest.NewWithConfig(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE colors (name TEXT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Database = database
	run := func(seed sqlite.SeedFunc, args ...string) (string, error) {
		var stdout bytes.Buffer
		err := sqlite.SeedCommand(seed).Run(ctx, forge.CommandEnv{
			Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Stdout: &stdout, Stderr: io.Discard,
		}, args)
		return stdout.String(), err
	}
	idempotent := func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO colors (name) VALUES ('red'), ('blue') ON CONFLICT (name) DO NOTHING")
		return err
	}
	for range 2 {
		if output, err := run(idempotent); err != nil || output != "seed data loaded\n" {
			t.Fatalf("seed: output=%q err=%v", output, err)
		}
	}
	failure := errors.New("seed failed halfway")
	if _, err := run(func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO colors (name) VALUES ('green')"); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("failing seed: %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM colors").Scan(&count); err != nil || count != 2 {
		t.Fatalf("rows=%d err=%v; want the two idempotent rows and no partial seed", count, err)
	}
	if _, err := run(idempotent, "extra"); !errors.Is(err, forge.ErrUsage) {
		t.Fatalf("arguments: %v", err)
	}
	if _, err := run(nil); err == nil {
		t.Fatal("a nil seed function must fail")
	}
}
