package mysql_test

import (
	"context"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/jobs"
	"github.com/rocketinsidecorp/forge-go/jobs/locktest"
	"github.com/rocketinsidecorp/forge-go/mysql"
	"github.com/rocketinsidecorp/forge-go/mysql/mysqltest"
)

var _ jobs.Lock = (*mysql.SchedulerLock)(nil)

func TestSchedulerLockConformance(t *testing.T) {
	locktest.Run(t, func(t testing.TB) (jobs.Lock, jobs.Lock) {
		first, cfg := mysqltest.NewWithConfig(t)
		// A second pool on the same database: another replica.
		second, err := mysql.Open(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = second.Close() })
		return mysql.NewSchedulerLock(first), mysql.NewSchedulerLock(second)
	})
}

func TestSchedulerLockRefusesANilDatabase(t *testing.T) {
	if _, _, err := mysql.NewSchedulerLock(nil).TryLock(context.Background(), "job", time.Now()); err == nil {
		t.Fatal("a lock without a database was taken")
	}
}
