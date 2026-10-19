package ratelimit

import (
	"sync/atomic"
	"time"
)

// MaxPruneRounds bounds how many batches one Prune call removes, so a call is
// bounded work (at most MaxPruneRounds x PruneBatch rows) yet keeps up with a
// sustained stream of new keys.
const MaxPruneRounds = 20

// PruneGate decides when a store prunes inline after a hit: when PruneInterval
// has passed or when PruneBatch hits have happened since the last prune,
// whichever comes first, so expired buckets cannot pile up faster than they are
// removed. The zero value is ready to use.
type PruneGate struct {
	last   atomic.Int64 // unix nanoseconds of the last prune
	writes atomic.Int64 // hits since the last prune
}

// Due records one hit and reports whether the caller should prune now.
func (g *PruneGate) Due(opts StoreOptions) bool {
	if opts.PruneInterval < 0 {
		return false
	}
	hits := g.writes.Add(1)
	now := time.Now().UnixNano()
	last := g.last.Load()
	if hits < int64(opts.PruneBatch) && now-last < int64(opts.PruneInterval) {
		return false
	}
	if !g.last.CompareAndSwap(last, now) {
		return false
	}
	g.writes.Store(0)
	return true
}
