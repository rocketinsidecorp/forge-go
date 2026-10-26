package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/telemetry"
)

// Option configures Open.
type Option func(*openOptions)

type openOptions struct {
	telemetry *telemetry.Telemetry
	poolName  string
	// requireRowSecurity forces the row-level security check
	// (RequireRowSecurity outside development and test); it wins over
	// allowRowSecurityBypass, the only opt-out (AllowRowSecurityBypass).
	// Without either, the check follows the configuration's environment.
	requireRowSecurity     bool
	allowRowSecurityBypass bool
	// lazy defers connecting to the first query (Lazy).
	lazy bool
}

// poolSystem is the db.system.name of the pool's metrics.
const poolSystem = "postgresql"

// DefaultPoolName is the pool name reported when WithPoolName is not used.
const DefaultPoolName = sqldb.DefaultPoolName

// WithPoolName names the pool in the connection metrics' pool name label
// (default "default"). Name each pool when an application opens more than
// one PostgreSQL database; a name used twice replaces the earlier pool's
// series.
func WithPoolName(name string) Option {
	return func(o *openOptions) { o.poolName = name }
}

// observePool exports pool's connection statistics: db.client.connection.count,
// db.client.connection.max, forge.db.pool.waits and
// forge.db.pool.wait.duration. stop ends it when the pool closes.
func observePool(t *telemetry.Telemetry, name string, pool *pgxpool.Pool) (stop func(), err error) {
	if name == "" {
		name = DefaultPoolName
	}
	return t.ObservePool(poolSystem, name, func() telemetry.PoolStats {
		stat := pool.Stat()
		return telemetry.PoolStats{
			InUse:    int(stat.AcquiredConns()),
			Idle:     int(stat.IdleConns()),
			Max:      int(stat.MaxConns()),
			Waits:    stat.EmptyAcquireCount(),
			WaitTime: stat.EmptyAcquireWaitTime(),
		}
	})
}

// WithTelemetry instruments the pool: its connection statistics are exported
// (db.client.connection.count and the other pool metrics, see
// docs/OBSERVABILITY.md), and through pgx's query tracer every
// statement, including those inside transactions, is measured in
// db.client.operation.duration and, inside a traced operation, gets a span
// with db.system.name "postgresql" and db.operation.name. Statement text and
// arguments are never recorded. Batches (SendBatch) and COPY are not traced.
// A nil Telemetry leaves the pool uninstrumented, so
// Open(ctx, cfg, WithTelemetry(app.Telemetry())) is always safe.
func WithTelemetry(t *telemetry.Telemetry) Option {
	return func(o *openOptions) { o.telemetry = t }
}

// queryTracer implements pgx.QueryTracer. pgx calls TraceQueryEnd when a
// query's rows are closed, so query spans include reading the results.
type queryTracer struct {
	observer *sqldb.Observer
}

var _ pgx.QueryTracer = (*queryTracer)(nil)

type observationKey struct{}

func (t *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, observationKey{}, t.observer.Start(ctx, data.SQL))
}

func (t *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if observation, ok := ctx.Value(observationKey{}).(sqldb.Observation); ok {
		observation.End(data.Err)
	}
}
