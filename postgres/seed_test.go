package postgres_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

func TestSeedCommandCommitsOnlyASuccessfulSeed(t *testing.T) {
	t.Parallel()
	db, database := postgrestest.NewWithConfig(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, "CREATE TABLE colors (name text PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Database = database
	run := func(seed postgres.SeedFunc, args ...string) (string, error) {
		var stdout bytes.Buffer
		err := postgres.SeedCommand(seed).Run(ctx, forge.CommandEnv{
			Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Stdout: &stdout, Stderr: io.Discard,
		}, args)
		return stdout.String(), err
	}
	idempotent := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO colors (name) VALUES ('red'), ('blue') ON CONFLICT (name) DO NOTHING")
		return err
	}
	for range 2 {
		if output, err := run(idempotent); err != nil || output != "seed data loaded\n" {
			t.Fatalf("seed: output=%q err=%v", output, err)
		}
	}
	failure := errors.New("seed failed halfway")
	if _, err := run(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO colors (name) VALUES ('green')"); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("failing seed: %v", err)
	}
	var count int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM colors").Scan(&count); err != nil || count != 2 {
		t.Fatalf("rows=%d err=%v; want the two idempotent rows and no partial seed", count, err)
	}
	if _, err := run(idempotent, "extra"); !errors.Is(err, forge.ErrUsage) {
		t.Fatalf("arguments: %v", err)
	}
}
