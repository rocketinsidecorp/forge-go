package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/telemetry"
)

// Option configures Open.
type Option func(*openOptions)

type openOptions struct {
	telemetry *telemetry.Telemetry
}

// WithTelemetry instruments the pool through pgx's query tracer: every
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
