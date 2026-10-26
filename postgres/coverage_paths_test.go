package postgres_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

// Every configuration problem is reported before any connection is tried,
// as an error that names the problem and never the URL or its password.
func TestOpenAndNewMigratorRefuseConfigurationTheyCannotUse(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		url      string
		maxConns int32
		want     string
	}{
		"a pool with no connections": {"postgres://app:hunter2@127.0.0.1:1/app?sslmode=disable", 0, "create connection pool"},
		"a malformed query":          {"postgres://app:hunter2@127.0.0.1:1/app?application_name=%zz", 4, "not a valid PostgreSQL connection string"},
		"a mode pgx rejects":         {"postgres://app:hunter2@127.0.0.1:1/app?sslmode=bogus", 4, "not a valid PostgreSQL connection string"},
		"hosts net/url reads as one": {"postgres://app:hunter2@db1,db2:5432/app?sslmode=disable", 4, "ambiguous"},
	} {
		cfg := config.Default().Database
		cfg.URL = config.NewSecret(tt.url)
		cfg.MaxConns = tt.maxConns
		db, err := postgres.Open(context.Background(), cfg)
		if err == nil || db != nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: Open = %v, %v; want an error saying %q", name, db, err, tt.want)
		}
		if tt.maxConns == 0 {
			continue // the migrator does not use the pool size
		}
		migrator, err := postgres.NewMigrator(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err == nil || migrator != nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: NewMigrator = %v, %v; want an error saying %q", name, migrator, err, tt.want)
		}
	}
}

func TestSeedCommandRefusesWithoutASeedFunctionOrADatabase(t *testing.T) {
	t.Parallel()
	run := func(seed postgres.SeedFunc, cfg config.Config) (string, error) {
		var stdout bytes.Buffer
		err := postgres.SeedCommand(seed).Run(context.Background(), forge.CommandEnv{
			Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Stdout: &stdout, Stderr: io.Discard,
		}, nil)
		return stdout.String(), err
	}
	seed := func(context.Context, pgx.Tx) error { t.Error("the seed ran"); return nil }
	if out, err := run(nil, config.Default()); err == nil || !strings.Contains(err.Error(), "no seed function") || out != "" {
		t.Errorf("no seed function: %q, %v", out, err)
	}
	// Default() has no database URL: Open's error is returned, nothing is
	// reported as loaded.
	if out, err := run(seed, config.Default()); err == nil || !strings.Contains(err.Error(), "FORGE_DATABASE_URL") || out != "" {
		t.Errorf("no database: %q, %v", out, err)
	}
}

// A lock taken on a transaction that has finished is an error naming the
// lock, not a silent success: the caller must not believe it holds one.
func TestAdvisoryLocksReportAFinishedTransaction(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	ctx := context.Background()
	var finished pgx.Tx
	if err := db.InTx(ctx, func(tx pgx.Tx) error { finished = tx; return nil }); err != nil {
		t.Fatal(err)
	}
	key := postgres.LockKey("test:finished")
	if err := postgres.AdvisoryXactLock(ctx, finished, key); err == nil || !strings.Contains(err.Error(), "acquire advisory lock") {
		t.Errorf("AdvisoryXactLock on a finished transaction: %v", err)
	}
	if acquired, err := postgres.TryAdvisoryXactLock(ctx, finished, key); err == nil || acquired || !strings.Contains(err.Error(), "try advisory lock") {
		t.Errorf("TryAdvisoryXactLock on a finished transaction = %v, %v", acquired, err)
	}
}

// A constraint checked at commit (a deferred foreign key) fails the commit,
// and the transaction leaves nothing behind.
func TestCommitFailureIsReportedAndRollsBack(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE parents (id int PRIMARY KEY)`,
		`CREATE TABLE children (id int PRIMARY KEY, parent_id int REFERENCES parents (id) DEFERRABLE INITIALLY DEFERRED)`,
	} {
		if _, err := db.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	err := db.InTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO children (id, parent_id) VALUES (1, 404)`)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "commit transaction") {
		t.Fatalf("a deferred violation at commit: %v", err)
	}
	if public := fault.From(postgres.Translate(err)); public == nil || public.Code != "reference_violation" {
		t.Errorf("Translate(%v) = %v, want reference_violation", err, public)
	}
	var children int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM children`).Scan(&children); err != nil || children != 0 {
		t.Errorf("the failed commit left %d rows (%v)", children, err)
	}
}

// An EXCLUDE constraint is a conflict (409) that names the constraint.
func TestExclusionViolationTranslatesToAConflict(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE bookings (id int PRIMARY KEY, during int4range,
		CONSTRAINT bookings_during_excl EXCLUDE USING gist (during WITH &&))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO bookings VALUES (1, '[1,10)')`); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(ctx, `INSERT INTO bookings VALUES (2, '[5,15)')`)
	public := fault.From(postgres.Translate(err))
	if public == nil || public.Code != "conflict" || public.Status() != 409 || public.Metadata["constraint"] != "bookings_during_excl" {
		t.Fatalf("Translate(%v) = %+v", err, public)
	}
	if strings.Contains(public.Message, "bookings") {
		t.Errorf("the public message names the table: %q", public.Message)
	}
}
