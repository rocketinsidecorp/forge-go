package postgres_test

import (
	"context"
	"io/fs"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/jobs"
	jobspostgres "github.com/rocketinsidecorp/forge-go/jobs/postgres"
	"github.com/rocketinsidecorp/forge-go/jobs/ticktest"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

// newDatabase returns the configuration of a fresh database holding the
// forge_job_schedules table.
func newDatabase(t testing.TB) config.Database {
	t.Helper()
	db, cfg := postgrestest.NewWithConfig(t)
	up, err := fs.ReadFile(jobspostgres.Migrations(), "20261026120000_job_schedules.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(context.Background(), string(up)); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// open is one replica's pool on the database.
func open(t testing.TB, cfg config.Database, maxConns int32) *postgres.DB {
	t.Helper()
	cfg.MaxConns, cfg.MinConns = maxConns, 0
	db, err := postgres.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func TestTickClaimerConformance(t *testing.T) {
	ticktest.Run(t, func(t testing.TB) (jobs.TickClaimer, jobs.TickClaimer) {
		cfg := newDatabase(t)
		return jobspostgres.NewTickClaimer(open(t, cfg, 4)), jobspostgres.NewTickClaimer(open(t, cfg, 4))
	})
}

// Two replicas with two connections each run four aligned jobs that each
// hold a connection for 300ms: no run starves (a tick lock held per run
// would leave the jobs no connection).
func TestBusyReplicasWithSmallPools(t *testing.T) {
	cfg := newDatabase(t)
	a, b := open(t, cfg, 2), open(t, cfg, 2)
	work := func(db *postgres.DB) func(context.Context) error {
		return func(ctx context.Context) error {
			_, err := db.Exec(ctx, "SELECT pg_sleep(0.3)")
			return err
		}
	}
	ticktest.BusyReplicas(t, jobspostgres.NewTickClaimer(a), jobspostgres.NewTickClaimer(b), [2]func(context.Context) error{work(a), work(b)})
}

func TestTickClaimerRefusesBadInput(t *testing.T) {
	if _, err := jobspostgres.NewTickClaimer(nil).ClaimTick(context.Background(), "job", time.Now()); err == nil {
		t.Fatal("a claimer without a database claimed")
	}
	claimer := jobspostgres.NewTickClaimer(open(t, newDatabase(t), 1))
	for _, name := range []string{"", string(make([]byte, jobs.MaxJobNameLength+1))} {
		if _, err := claimer.ClaimTick(context.Background(), name, time.Now().Add(-time.Hour)); err == nil {
			t.Errorf("a job name of %d bytes was accepted", len(name))
		}
	}
}
