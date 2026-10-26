package mysql_test

import (
	"context"
	"io/fs"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/jobs"
	jobsmysql "github.com/rocketinsidecorp/forge-go/jobs/mysql"
	"github.com/rocketinsidecorp/forge-go/jobs/ticktest"
	"github.com/rocketinsidecorp/forge-go/mysql"
	"github.com/rocketinsidecorp/forge-go/mysql/mysqltest"
	"github.com/rocketinsidecorp/forge-go/sqldb"
)

// newDatabase returns the configuration of a fresh database holding the
// forge_job_schedules table.
func newDatabase(t testing.TB) config.Database {
	t.Helper()
	db, cfg := mysqltest.NewWithConfig(t)
	up, err := fs.ReadFile(jobsmysql.Migrations(), "20261026120000_job_schedules.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), string(up)); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// open is one replica's pool on the database.
func open(t testing.TB, cfg config.Database, maxConns int32) *sqldb.DB {
	t.Helper()
	cfg.MaxConns, cfg.MinConns = maxConns, 0
	db, err := mysql.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestTickClaimerConformance(t *testing.T) {
	ticktest.Run(t, func(t testing.TB) (jobs.TickClaimer, jobs.TickClaimer) {
		cfg := newDatabase(t)
		return jobsmysql.NewTickClaimer(open(t, cfg, 4)), jobsmysql.NewTickClaimer(open(t, cfg, 4))
	})
}

// Two replicas with two connections each run four aligned jobs that each
// hold a connection for 300ms: no run starves.
func TestBusyReplicasWithSmallPools(t *testing.T) {
	cfg := newDatabase(t)
	a, b := open(t, cfg, 2), open(t, cfg, 2)
	work := func(db *sqldb.DB) func(context.Context) error {
		return func(ctx context.Context) error {
			_, err := db.ExecContext(ctx, "SELECT SLEEP(0.3)")
			return err
		}
	}
	ticktest.BusyReplicas(t, jobsmysql.NewTickClaimer(a), jobsmysql.NewTickClaimer(b), [2]func(context.Context) error{work(a), work(b)})
}

func TestTickClaimerRefusesBadInput(t *testing.T) {
	if _, err := jobsmysql.NewTickClaimer(nil).ClaimTick(context.Background(), "job", time.Now()); err == nil {
		t.Fatal("a claimer without a database claimed")
	}
	claimer := jobsmysql.NewTickClaimer(open(t, newDatabase(t), 1))
	for _, name := range []string{"", string(make([]byte, jobs.MaxJobNameLength+1))} {
		if _, err := claimer.ClaimTick(context.Background(), name, time.Now().Add(-time.Hour)); err == nil {
			t.Errorf("a job name of %d bytes was accepted", len(name))
		}
	}
}
