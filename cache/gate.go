package cache

import (
	"sync/atomic"
	"time"
)

// MaxPruneRounds bounds how many batches of each kind one Prune call removes,
// so a call is bounded work (at most MaxPruneRounds x PruneBatch rows per kind)
// yet keeps up with a sustained stream of new keys.
const MaxPruneRounds = 20

// PruneGate decides when a store prunes inline after a write: when PruneInterval
// has passed or when PruneBatch writes have happened since the last prune,
// whichever comes first. The second condition is what keeps the entry bound
// real under a sustained stream of new keys: the table can exceed MaxEntries by
// about one batch per process, not by the writes of a whole interval. The zero
// value is ready to use.
type PruneGate struct {
	last   atomic.Int64 // unix nanoseconds of the last prune
	writes atomic.Int64 // writes since the last prune
}

// Due records one write and reports whether the caller should prune now.
func (g *PruneGate) Due(opts StoreOptions) bool {
	if opts.PruneInterval < 0 {
		return false
	}
	writes := g.writes.Add(1)
	now := time.Now().UnixNano()
	last := g.last.Load()
	if writes < int64(opts.PruneBatch) && now-last < int64(opts.PruneInterval) {
		return false
	}
	if !g.last.CompareAndSwap(last, now) {
		return false
	}
	g.writes.Store(0)
	return true
}
