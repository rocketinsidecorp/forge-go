package mysql_test

import (
	"context"
	"database/sql"
	"io/fs"
	"net/url"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

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

// openFoundRows opens a replica's pool whose connections count matched rather
// than changed rows (CLIENT_FOUND_ROWS), which mysql.Open never does: the
// claim must stay exactly once on such a handle too.
func openFoundRows(t testing.TB, cfg config.Database) *sqldb.DB {
	t.Helper()
	parsed, err := url.Parse(cfg.URL.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	password, _ := parsed.User.Password()
	connector, err := driver.NewConnector(&driver.Config{
		User: parsed.User.Username(), Passwd: password, Net: "tcp", Addr: parsed.Host,
		DBName: strings.TrimPrefix(parsed.Path, "/"), ParseTime: true, Loc: time.UTC,
		AllowNativePasswords: true, ClientFoundRows: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool := sql.OpenDB(connector)
	cfg.MaxConns = 4
	db, err := sqldb.New(pool, cfg, func(error) sqldb.Classification { return sqldb.Classification{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return db
}

func TestTickClaimerConformanceWithClientFoundRows(t *testing.T) {
	ticktest.Run(t, func(t testing.TB) (jobs.TickClaimer, jobs.TickClaimer) {
		cfg := newDatabase(t)
		return jobsmysql.NewTickClaimer(openFoundRows(t, cfg)), jobsmysql.NewTickClaimer(openFoundRows(t, cfg))
	})
}

// holdingClaimer emulates the per-tick GET_LOCK this package replaced: after
// winning a tick it keeps one of the replica's connections for 2.5s, longer
// than a run may last (its job timeout, the 2s interval).
type holdingClaimer struct {
	claimer jobs.TickClaimer
	db      *sqldb.DB
}

func (h holdingClaimer) ClaimTick(ctx context.Context, job string, tick time.Time) (bool, error) {
	claimed, err := h.claimer.ClaimTick(ctx, job, tick)
	if err != nil || !claimed {
		return claimed, err
	}
	conn, err := h.db.SQL().Conn(ctx)
	if err != nil {
		return false, err
	}
	time.AfterFunc(2500*time.Millisecond, func() { _ = conn.Close() })
	return true, nil
}

// The negative control of TestBusyReplicasWithSmallPools: a claimer that
// keeps a connection after winning starves the jobs, so the scenario does
// detect the defect it guards against.
func TestBusyReplicasDetectAClaimerThatHoldsAConnection(t *testing.T) {
	cfg := newDatabase(t)
	a, b := open(t, cfg, 2), open(t, cfg, 2)
	work := func(db *sqldb.DB) func(context.Context) error {
		return func(ctx context.Context) error {
			_, err := db.ExecContext(ctx, "SELECT SLEEP(0.3)")
			return err
		}
	}
	failed := ticktest.BusyReplicaFailures(t,
		holdingClaimer{jobsmysql.NewTickClaimer(a), a}, holdingClaimer{jobsmysql.NewTickClaimer(b), b},
		[2]func(context.Context) error{work(a), work(b)})
	if len(failed) == 0 {
		t.Fatal("jobs ran fine although every won tick kept a connection: the regression scenario cannot detect pool starvation")
	}
}
