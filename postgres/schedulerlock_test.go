package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/jobs"
	"github.com/rocketinsidecorp/forge-go/jobs/locktest"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

var _ jobs.Lock = (*postgres.SchedulerLock)(nil)

func TestSchedulerLockConformance(t *testing.T) {
	locktest.Run(t, func(t testing.TB) (jobs.Lock, jobs.Lock) {
		first, cfg := postgrestest.NewWithConfig(t)
		// A second pool on the same database: another replica.
		second, err := postgres.Open(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(second.Close)
		return postgres.NewSchedulerLock(first), postgres.NewSchedulerLock(second)
	})
}

func TestSchedulerLockRefusesANilDatabase(t *testing.T) {
	if _, _, err := postgres.NewSchedulerLock(nil).TryLock(context.Background(), "job", time.Now()); err == nil {
		t.Fatal("a lock without a database was taken")
	}
}
