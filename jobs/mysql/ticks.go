// Package mysql records the ticks of package jobs' scheduler in MySQL or
// MariaDB, so that each tick of a scheduled job runs on exactly one replica
// (ADR 0047).
//
// Compose the table's migration with the application's own and give the
// claimer to the scheduler:
//
//	mysql.CommandsFromSets(jobsmysql.Migrations(), db.Migrations())
//	scheduler := jobs.NewScheduler(jobs.SchedulerOptions{Ticks: jobsmysql.NewTickClaimer(database)})
package mysql

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/rocketinsidecorp/forge-go/jobs"
	"github.com/rocketinsidecorp/forge-go/sqldb"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations returns the forge_job_schedules migration rooted at ".".
func Migrations() fs.FS {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

// TickClaimer is the jobs.TickClaimer of MySQL and MariaDB.
type TickClaimer struct {
	db *sqldb.DB
}

var _ jobs.TickClaimer = (*TickClaimer)(nil)

// NewTickClaimer returns a claimer on db (from mysql.Open). The table comes
// from Migrations.
func NewTickClaimer(db *sqldb.DB) *TickClaimer { return &TickClaimer{db: db} }

// ClaimTick reads the database clock, refusing a tick ahead of it with a
// *jobs.EarlyTickError, then records tick as job's latest tick and reports
// whether this call advanced it. Recording is two autocommit statements: an
// INSERT IGNORE for a job's first tick, else an UPDATE whose WHERE clause
// requires an earlier recorded tick. Both report one affected row exactly
// when this call recorded the tick, whether or not the connection counts
// matched rather than changed rows (CLIENT_FOUND_ROWS), so any *sqldb.DB on
// MySQL or MariaDB is safe. Each statement borrows a pooled connection only
// while it runs.
func (c *TickClaimer) ClaimTick(ctx context.Context, job string, tick time.Time) (bool, error) {
	if c == nil || c.db == nil {
		return false, errors.New("tick claimer needs a database")
	}
	if job == "" || len(job) > jobs.MaxJobNameLength {
		return false, fmt.Errorf("job name must be 1 to %d bytes", jobs.MaxJobNameLength)
	}
	tick = tick.UTC().Truncate(time.Microsecond)
	var aheadUS int64
	if err := c.db.QueryRowContext(ctx, "SELECT TIMESTAMPDIFF(MICROSECOND, UTC_TIMESTAMP(6), ?)", tick).Scan(&aheadUS); err != nil {
		return false, fmt.Errorf("read the database clock: %w", c.db.Translate(err))
	}
	if aheadUS > 0 {
		return false, &jobs.EarlyTickError{Wait: time.Duration(aheadUS) * time.Microsecond}
	}
	// The job name is validated above, so IGNORE can only skip the
	// duplicate key of a job that already has a row.
	inserted, err := c.affected(ctx, `
		INSERT IGNORE INTO forge_job_schedules (name, last_tick_at, updated_at)
		VALUES (?, ?, UTC_TIMESTAMP(6))
	`, job, tick)
	if err != nil || inserted == 1 {
		return inserted == 1, err
	}
	// Every row the WHERE clause matches is changed, so matched and changed
	// row counts agree.
	advanced, err := c.affected(ctx, `
		UPDATE forge_job_schedules
		SET last_tick_at = ?, updated_at = UTC_TIMESTAMP(6)
		WHERE name = ? AND last_tick_at < ?
	`, tick, job, tick)
	return advanced == 1, err
}

func (c *TickClaimer) affected(ctx context.Context, query string, args ...any) (int64, error) {
	result, err := c.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("record scheduled tick: %w", c.db.Translate(err))
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("record scheduled tick: %w", err)
	}
	return n, nil
}
