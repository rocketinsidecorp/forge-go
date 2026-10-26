// Package postgres records the ticks of package jobs' scheduler in
// PostgreSQL, so that each tick of a scheduled job runs on exactly one
// replica (ADR 0047).
//
// Compose the table's migration with the application's own and give the
// claimer to the scheduler:
//
//	postgres.CommandsFromSets(jobspostgres.Migrations(), db.Migrations())
//	scheduler := jobs.NewScheduler(jobs.SchedulerOptions{Ticks: jobspostgres.NewTickClaimer(database)})
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/rocketinsidecorp/forge-go/jobs"
	forgepostgres "github.com/rocketinsidecorp/forge-go/postgres"
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

// TickClaimer is the jobs.TickClaimer of PostgreSQL.
type TickClaimer struct {
	db *forgepostgres.DB
}

var _ jobs.TickClaimer = (*TickClaimer)(nil)

// NewTickClaimer returns a claimer on db's pool. The table comes from
// Migrations.
func NewTickClaimer(db *forgepostgres.DB) *TickClaimer { return &TickClaimer{db: db} }

// ClaimTick records tick as job's latest tick in one autocommit statement
// and reports whether this call advanced it. The database clock refuses a
// tick ahead of it with a *jobs.EarlyTickError. The statement borrows a
// pooled connection only while it runs.
func (c *TickClaimer) ClaimTick(ctx context.Context, job string, tick time.Time) (bool, error) {
	if c == nil || c.db == nil {
		return false, errors.New("tick claimer needs a database")
	}
	if job == "" || len(job) > jobs.MaxJobNameLength {
		return false, fmt.Errorf("job name must be 1 to %d bytes", jobs.MaxJobNameLength)
	}
	var (
		claimed bool
		aheadUS int64
	)
	err := c.db.QueryRow(ctx, `
		WITH clock AS (SELECT clock_timestamp() AS now),
		advanced AS (
			INSERT INTO forge_job_schedules AS s (name, last_tick_at)
			SELECT $1, $2::timestamptz FROM clock WHERE $2::timestamptz <= clock.now
			ON CONFLICT (name) DO UPDATE
				SET last_tick_at = EXCLUDED.last_tick_at, updated_at = clock_timestamp()
				WHERE s.last_tick_at < EXCLUDED.last_tick_at
			RETURNING 1
		)
		SELECT EXISTS (SELECT 1 FROM advanced),
		       GREATEST(0, (EXTRACT(EPOCH FROM ($2::timestamptz - clock.now)) * 1000000)::bigint)
		FROM clock
	`, job, tick.UTC().Truncate(time.Microsecond)).Scan(&claimed, &aheadUS)
	if err != nil {
		return false, fmt.Errorf("record scheduled tick: %w", forgepostgres.Translate(err))
	}
	if !claimed && aheadUS > 0 {
		return false, &jobs.EarlyTickError{Wait: time.Duration(aheadUS) * time.Microsecond}
	}
	return claimed, nil
}
