package telemetry

import (
	"errors"
	"time"
)

// Connection pool metric names. The count and max follow the OpenTelemetry
// database client conventions; the wait totals are Forge's.
const (
	MetricDBConnectionCount = "db.client.connection.count"
	MetricDBConnectionMax   = "db.client.connection.max"
	MetricDBPoolWaits       = "forge.db.pool.waits"
	MetricDBPoolWaitTime    = "forge.db.pool.wait.duration"
)

// PoolStats is a snapshot of a database connection pool.
type PoolStats struct {
	// InUse and Idle are the connections handed out and the connections
	// waiting in the pool; Max is the configured limit.
	InUse, Idle, Max int
	// Waits is the running number of acquisitions that had to wait, and
	// WaitTime the running time they waited. database/sql counts waits for
	// a connection when the pool is at its limit; pgx also counts an
	// acquisition that waited for a new connection to be established.
	Waits    int64
	WaitTime time.Duration
}

// ObservePool exports the pool's statistics, read by stats at every
// collection, as db.client.connection.count (state idle or used),
// db.client.connection.max, forge.db.pool.waits and
// forge.db.pool.wait.duration, labelled with the database system (the
// OpenTelemetry db.system.name) and the pool name. Registering the same
// system and name again replaces the earlier pool, which is what reopening a
// database in the same process means. A nil Telemetry does nothing.
func (t *Telemetry) ObservePool(system, name string, stats func() PoolStats) error {
	if t == nil {
		return nil
	}
	if system == "" || name == "" || stats == nil {
		return errors.New("telemetry: pool system, name and stats are required")
	}
	r := t.metrics
	count, err := r.Gauge(MetricDBConnectionCount, "Connections in the pool, by state (idle or used).", "{connection}",
		"db.system.name", "db.client.connection.pool.name", "db.client.connection.state")
	if err != nil {
		return err
	}
	limit, err := r.Gauge(MetricDBConnectionMax, "Maximum number of connections the pool allows.", "{connection}",
		"db.system.name", "db.client.connection.pool.name")
	if err != nil {
		return err
	}
	waits, err := r.Counter(MetricDBPoolWaits, "Connection acquisitions that had to wait for a connection.", "{wait}",
		"db.system.name", "db.client.connection.pool.name")
	if err != nil {
		return err
	}
	waited, err := r.FloatCounter(MetricDBPoolWaitTime, "Time spent waiting for a connection.", "s",
		"db.system.name", "db.client.connection.pool.name")
	if err != nil {
		return err
	}
	r.Observe("pool/"+system+"/"+name, func() {
		s := stats()
		count.Set(float64(s.Idle), system, name, "idle")
		count.Set(float64(s.InUse), system, name, "used")
		limit.Set(float64(s.Max), system, name)
		waits.Observe(s.Waits, system, name)
		waited.Observe(s.WaitTime.Seconds(), system, name)
	})
	return nil
}
