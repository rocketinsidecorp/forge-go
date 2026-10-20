package mysql

import (
	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/telemetry"
)

// Option configures Open.
type Option func(*openOptions)

type openOptions struct {
	telemetry *telemetry.Telemetry
}

// WithTelemetry instruments the pool: every statement is measured in
// db.client.operation.duration and, inside a traced operation, gets a span
// with db.system.name "mysql" and db.operation.name. Statement text and
// arguments are never recorded. A nil Telemetry leaves the pool
// uninstrumented: Open(ctx, cfg, WithTelemetry(app.Telemetry())) is always
// safe.
func WithTelemetry(t *telemetry.Telemetry) Option {
	return func(o *openOptions) { o.telemetry = t }
}

func observer(opts []Option) (*sqldb.Observer, error) {
	var settings openOptions
	for _, option := range opts {
		if option != nil {
			option(&settings)
		}
	}
	return sqldb.NewObserver(settings.telemetry, "mysql", classify)
}
