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
// *jobs.EarlyTickError, then records tick as job's latest tick in one
// autocommit upsert and reports whether this call advanced it. Each statement
// borrows a pooled connection only while it runs.
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
	// The assignments run in order: updated_at still sees the old
	// last_tick_at. MySQL reports 1 for an insert, 2 for an update that
	// changed the row and 0 for none.
	result, err := c.db.ExecContext(ctx, `
		INSERT INTO forge_job_schedules (name, last_tick_at, updated_at)
		VALUES (?, ?, UTC_TIMESTAMP(6))
		ON DUPLICATE KEY UPDATE
			updated_at = IF(last_tick_at < ?, UTC_TIMESTAMP(6), updated_at),
			last_tick_at = IF(last_tick_at < ?, ?, last_tick_at)
	`, job, tick, tick, tick, tick)
	if err != nil {
		return false, fmt.Errorf("record scheduled tick: %w", c.db.Translate(err))
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record scheduled tick: %w", err)
	}
	return changed > 0, nil
}
