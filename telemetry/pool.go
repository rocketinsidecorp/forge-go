package telemetry

import (
	"errors"
	"sync"
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
// OpenTelemetry db.system.name) and the pool name. Call stop when the pool is
// closed: the pool is no longer read, its connection counts and its limit
// drop to zero, and a collection that was reading it when stop ran does not
// write them back.
// Registering the same system and name again replaces the earlier pool,
// which is what reopening a database in the same process means. The wait
// counters keep growing across pools of one name: the totals of a stopped or
// replaced pool are kept and the new pool's totals, which start again at
// zero, are added to them. A nil Telemetry does nothing and returns a stop
// that does nothing.
func (t *Telemetry) ObservePool(system, name string, stats func() PoolStats) (stop func(), err error) {
	if t == nil {
		return func() {}, nil
	}
	if system == "" || name == "" || stats == nil {
		return nil, errors.New("telemetry: pool system, name and stats are required")
	}
	r := t.metrics
	count, err := r.Gauge(MetricDBConnectionCount, "Connections in the pool, by state (idle or used).", "{connection}",
		"db.system.name", "db.client.connection.pool.name", "db.client.connection.state")
	if err != nil {
		return nil, err
	}
	limit, err := r.Gauge(MetricDBConnectionMax, "Maximum number of connections the pool allows.", "{connection}",
		"db.system.name", "db.client.connection.pool.name")
	if err != nil {
		return nil, err
	}
	waits, err := r.Counter(MetricDBPoolWaits, "Connection acquisitions that had to wait for a connection.", "{wait}",
		"db.system.name", "db.client.connection.pool.name")
	if err != nil {
		return nil, err
	}
	waited, err := r.FloatCounter(MetricDBPoolWaitTime, "Time spent waiting for a connection.", "s",
		"db.system.name", "db.client.connection.pool.name")
	if err != nil {
		return nil, err
	}

	key := "pool/" + system + "/" + name
	t.poolsMu.Lock()
	if t.pools == nil {
		t.pools = make(map[string]*poolTotals)
	}
	totals := t.pools[key]
	if totals == nil {
		totals = &poolTotals{}
		t.pools[key] = totals
	}
	t.poolsMu.Unlock()

	totals.mu.Lock()
	defer totals.mu.Unlock()
	generation := totals.retire()
	// Registered under totals.mu, so registrations of one key take effect
	// in generation order.
	r.Observe(key, func() {
		s := stats() // outside the lock: reading a pool may wait
		// The generation check and the writes hold totals.mu together, so
		// a stop or a replacement that lands while this collection reads
		// the pool cannot be overwritten with the old pool's counts.
		totals.mu.Lock()
		defer totals.mu.Unlock()
		allWaits, allWaited, current := totals.add(generation, s.Waits, s.WaitTime.Seconds())
		if !current {
			return // a replaced or stopped pool read in a collection already under way
		}
		count.Set(float64(s.Idle), system, name, "idle")
		count.Set(float64(s.InUse), system, name, "used")
		limit.Set(float64(s.Max), system, name)
		waits.Observe(allWaits, system, name)
		waited.Observe(allWaited, system, name)
	})
	var once sync.Once
	return func() {
		once.Do(func() {
			totals.mu.Lock()
			defer totals.mu.Unlock()
			if totals.generation != generation {
				return // already replaced by a newer pool of this name
			}
			totals.retire()
			r.Observe(key, nil)
			count.Set(0, system, name, "idle")
			count.Set(0, system, name, "used")
			limit.Set(0, system, name)
		})
	}, nil
}

// poolTotals carries one pool name's wait totals across the pools that
// held the name, so the exported counters stay monotonic and keep counting
// when a pool is closed and another opened. Its mutex orders registrations,
// stops and observations of the name.
type poolTotals struct {
	mu sync.Mutex
	// generation identifies the pool currently observed; it changes when
	// the pool is replaced or stopped.
	generation uint64
	// baseWaits and baseWaited are the totals of earlier pools; waits and
	// waited the highest totals the current pool reported.
	baseWaits, waits   int64
	baseWaited, waited float64
}

// retire folds the current pool's totals into the base and starts a new
// generation, which it returns. The caller holds mu.
func (p *poolTotals) retire() uint64 {
	p.baseWaits += p.waits
	p.baseWaited += p.waited
	p.waits, p.waited = 0, 0
	p.generation++
	return p.generation
}

// add records the totals the pool of generation reported and returns the
// totals of every pool of the name, or current false when that pool is no
// longer the observed one. A pool's own totals never decrease here. The
// caller holds mu.
func (p *poolTotals) add(generation uint64, waits int64, waited float64) (int64, float64, bool) {
	if generation != p.generation {
		return 0, 0, false
	}
	p.waits = max(p.waits, waits)
	if waited > p.waited { // also skips NaN
		p.waited = waited
	}
	return p.baseWaits + p.waits, p.baseWaited + p.waited, true
}
