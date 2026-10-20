package mysql

import (
	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/telemetry"
)

// Option configures Open.
type Option func(*openOptions)

type openOptions struct {
	telemetry *telemetry.Telemetry
	poolName  string
}

// WithTelemetry instruments the pool: its connection statistics are exported
// (db.client.connection.count and the other pool metrics, see
// docs/OBSERVABILITY.md), every statement is measured in
// db.client.operation.duration and, inside a traced operation, gets a span
// with db.system.name "mysql" and db.operation.name. Statement text and
// arguments are never recorded. A nil Telemetry leaves the pool
// uninstrumented: Open(ctx, cfg, WithTelemetry(app.Telemetry())) is always
// safe.
func WithTelemetry(t *telemetry.Telemetry) Option {
	return func(o *openOptions) { o.telemetry = t }
}

// WithPoolName names the pool in the connection metrics' pool name label
// (default "default"). Name each pool when an application opens more than
// one database of the same kind; a name used twice replaces the earlier
// pool's series.
func WithPoolName(name string) Option {
	return func(o *openOptions) { o.poolName = name }
}

func observer(opts []Option) (*sqldb.Observer, string, error) {
	var settings openOptions
	for _, option := range opts {
		if option != nil {
			option(&settings)
		}
	}
	obs, err := sqldb.NewObserver(settings.telemetry, "mysql", classify)
	return obs, settings.poolName, err
}
