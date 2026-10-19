package ratelimit

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"time"
)

// WhenFull selects what Memory does when it holds MaxKeys live buckets and a
// new key arrives.
type WhenFull int

const (
	// EvictSoonest drops the bucket closest to expiring to make room. Under a
	// flood of distinct keys this resets the counters of the evicted keys (an
	// attacker needs more than MaxKeys keys in one window to do so), but
	// legitimate new clients are never refused.
	EvictSoonest WhenFull = iota
	// Deny refuses the new key (as rate limited) until a bucket expires. No
	// counter is ever lost, but a flood of distinct keys locks out new
	// clients. Prefer it where a bypass is worse than an outage.
	Deny
)

// MemoryOptions configures Memory. Zero values select the defaults.
type MemoryOptions struct {
	// MaxKeys bounds the live buckets. Zero means 100000.
	MaxKeys int
	// WhenFull defaults to EvictSoonest.
	WhenFull WhenFull
}

// DefaultMemoryOptions returns the defaults NewMemory applies to a zero
// MemoryOptions.
func DefaultMemoryOptions() MemoryOptions { return MemoryOptions{MaxKeys: 100_000} }

const maxMemoryKeys = 10_000_000

// Memory is a Limiter that keeps its buckets in process memory. Its size is
// bounded by MaxKeys. It starts no goroutines: expired buckets leave
// through a deadline-ordered heap during later calls, so there is nothing to
// stop and nothing that can leak.
//
// State is lost on restart and not shared between replicas; with N replicas
// the effective limit is up to N times higher. Use a database-backed limiter
// when that matters.
type Memory struct {
	mu       sync.Mutex
	opts     MemoryOptions
	now      func() time.Time
	buckets  map[string]*bucket
	deadline deadlineHeap
}

var _ Limiter = (*Memory)(nil)

type bucket struct {
	key     string
	hits    int
	resetAt time.Time
	index   int // position in the heap
}

// NewMemory returns an empty Memory.
func NewMemory(opts MemoryOptions) (*Memory, error) {
	return newMemory(opts, time.Now)
}

func newMemory(opts MemoryOptions, now func() time.Time) (*Memory, error) {
	if opts.MaxKeys == 0 {
		opts.MaxKeys = DefaultMemoryOptions().MaxKeys
	}
	if opts.MaxKeys < 1 || opts.MaxKeys > maxMemoryKeys {
		return nil, errors.New("rate limit max keys must be between 1 and 10000000")
	}
	if opts.WhenFull != EvictSoonest && opts.WhenFull != Deny {
		return nil, errors.New("rate limit WhenFull is invalid")
	}
	return &Memory{opts: opts, now: now, buckets: make(map[string]*bucket)}, nil
}

// Len returns the number of buckets currently held, expired ones included.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.buckets)
}

// Allow implements Limiter.
func (m *Memory) Allow(ctx context.Context, key string, limit Limit) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if err := CheckRequest(key, limit); err != nil {
		return Decision{}, err
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expire(now)

	b, ok := m.buckets[key]
	if !ok {
		if len(m.buckets) >= m.opts.MaxKeys {
			if m.opts.WhenFull == Deny {
				return Decision{Allowed: false, RetryAfter: m.untilFirstExpiry(now)}, nil
			}
			m.remove(m.deadline[0])
		}
		b = &bucket{key: key, resetAt: now.Add(limit.Per)}
		m.buckets[key] = b
		heap.Push(&m.deadline, b)
	}
	if b.hits < limit.N+1 { // saturate: a flood cannot overflow the counter
		b.hits++
	}
	if b.hits > limit.N {
		return Decision{Allowed: false, RetryAfter: max(b.resetAt.Sub(now), time.Millisecond)}, nil
	}
	return Decision{Allowed: true, Remaining: limit.N - b.hits}, nil
}

// expire removes buckets whose window ended. The work per call is bounded
// except right after a long idle period, when it amortizes against the calls
// that created the buckets.
func (m *Memory) expire(now time.Time) {
	for len(m.deadline) > 0 && !now.Before(m.deadline[0].resetAt) {
		m.remove(m.deadline[0])
	}
}

func (m *Memory) untilFirstExpiry(now time.Time) time.Duration {
	if len(m.deadline) == 0 {
		return time.Second
	}
	return max(m.deadline[0].resetAt.Sub(now), time.Millisecond)
}

func (m *Memory) remove(b *bucket) {
	heap.Remove(&m.deadline, b.index)
	delete(m.buckets, b.key)
}

// deadlineHeap orders buckets by the end of their window.
type deadlineHeap []*bucket

func (h deadlineHeap) Len() int           { return len(h) }
func (h deadlineHeap) Less(i, j int) bool { return h[i].resetAt.Before(h[j].resetAt) }
func (h deadlineHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *deadlineHeap) Push(x any) {
	b := x.(*bucket)
	b.index = len(*h)
	*h = append(*h, b)
}
func (h *deadlineHeap) Pop() any {
	old := *h
	n := len(old)
	b := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return b
}
