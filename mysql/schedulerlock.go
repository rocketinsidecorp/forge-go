package mysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/rocketinsidecorp/forge-go/sqldb"
)

// SchedulerLock is the jobs.Lock of MySQL and MariaDB: one named lock
// (GET_LOCK) per tick of a scheduled job. Named locks are server-wide, so the
// name carries a digest of the current database, the job name and the tick
// time: applications on other databases of the same server never contend.
type SchedulerLock struct {
	db *sqldb.DB
}

// NewSchedulerLock returns a scheduler lock on db (from mysql.Open), for
// jobs.SchedulerOptions.Lock.
func NewSchedulerLock(db *sqldb.DB) *SchedulerLock { return &SchedulerLock{db: db} }

// schedulerLockSuffix identifies one tick of one job; the database name is
// added by the server (DATABASE()) when the lock is taken.
func schedulerLockSuffix(job string, tick time.Time) string {
	return job + "\x00" + strconv.FormatInt(tick.UTC().UnixNano(), 10)
}

// lockNameSQL is the lock name: a 16-byte prefix and 40 hexadecimal digits,
// within the 64-character limit of named locks.
const lockNameSQL = "CONCAT('forge_jobs_tick:', LEFT(SHA2(CONCAT_WS(0x00, COALESCE(DATABASE(), ''), ?), 256), 40))"

// TryLock takes the tick's lock without waiting (timeout 0) on a connection
// it keeps until unlock. Unlock releases the lock and returns the connection
// to the pool; if the release fails the connection is discarded instead,
// which ends its session and the lock with it, so the lock cannot leak into
// the pool.
func (l *SchedulerLock) TryLock(ctx context.Context, job string, tick time.Time) (func(context.Context) error, bool, error) {
	if l == nil || l.db == nil {
		return nil, false, errors.New("scheduler lock needs a database")
	}
	conn, err := l.db.SQL().Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire scheduler lock connection: %w", err)
	}
	suffix := schedulerLockSuffix(job, tick)
	var acquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK("+lockNameSQL+", 0)", suffix).Scan(&acquired); err != nil {
		discardConn(conn)
		return nil, false, fmt.Errorf("take scheduler lock: %w", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		_ = conn.Close()
		return nil, false, nil
	}
	unlock := func(ctx context.Context) error {
		var released sql.NullInt64
		err := conn.QueryRowContext(ctx, "SELECT RELEASE_LOCK("+lockNameSQL+")", suffix).Scan(&released)
		if err == nil && (!released.Valid || released.Int64 != 1) {
			err = errors.New("the lock was not held by this session")
		}
		if err != nil {
			discardConn(conn)
			return fmt.Errorf("release scheduler lock: %w", err)
		}
		return conn.Close()
	}
	return unlock, true, nil
}

// discardConn closes conn's session instead of returning it to the pool.
func discardConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}
