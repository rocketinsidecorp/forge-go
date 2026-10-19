package cache

import (
	"container/list"
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// MemoryOptions configures Memory.
type MemoryOptions struct {
	Options
	// MaxBytes bounds the memory of keys and values together (default
	// 64 MiB). It must be at least MaxValueBytes.
	MaxBytes int64
}

const (
	defaultMemoryBytes = 64 << 20
	// entryOverhead approximates the bookkeeping of one entry, so many tiny
	// entries cannot defeat MaxBytes.
	entryOverhead = 96
)

// Memory is a Cache in process memory with LRU eviction and per-entry
// expiry, bounded by entries and by bytes. It starts no goroutines: expired
// entries are dropped when read and when writes need room, and the least
// recently used entries leave first.
//
// It is local to one process: replicas do not share it, and it is empty after
// a restart.
type Memory struct {
	opts   MemoryOptions
	now    func() time.Time
	flight Flight

	mu      sync.Mutex
	entries map[string]*list.Element // key -> element holding *memoryEntry
	lru     *list.List               // front = most recently used
	bytes   int64
}

type memoryEntry struct {
	key       string
	value     []byte
	expiresAt time.Time
}

func (e *memoryEntry) cost() int64 { return int64(len(e.key) + len(e.value) + entryOverhead) }

var _ Cache = (*Memory)(nil)

// NewMemory returns an empty Memory.
func NewMemory(opts MemoryOptions) (*Memory, error) { return newMemory(opts, time.Now) }

func newMemory(opts MemoryOptions, now func() time.Time) (*Memory, error) {
	base, err := opts.Normalize()
	if err != nil {
		return nil, err
	}
	opts.Options = base
	if opts.MaxBytes == 0 {
		opts.MaxBytes = defaultMemoryBytes
	}
	if opts.MaxBytes < int64(opts.MaxValueBytes)+entryOverhead+MaxKeyLength {
		return nil, errors.New("cache max bytes must hold at least one maximum-size value")
	}
	return &Memory{opts: opts, now: now, entries: make(map[string]*list.Element), lru: list.New()}, nil
}

// Len returns the number of entries held, expired ones included until they are
// dropped.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// Get implements Cache.
func (m *Memory) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := CheckKey(key); err != nil {
		return nil, false, err
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.entries[key]
	if !ok {
		return nil, false, nil
	}
	e := el.Value.(*memoryEntry)
	if !now.Before(e.expiresAt) {
		m.remove(el)
		return nil, false, nil
	}
	m.lru.MoveToFront(el)
	return slices.Clone(emptyNotNil(e.value)), true, nil
}

// Set implements Cache.
func (m *Memory) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ttl, err := m.opts.CheckSet(key, value, ttl)
	if err != nil {
		return err
	}
	now := m.now()
	e := &memoryEntry{key: key, value: slices.Clone(value), expiresAt: now.Add(ttl)}
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.entries[key]; ok {
		m.remove(el)
	}
	m.entries[key] = m.lru.PushFront(e)
	m.bytes += e.cost()
	m.makeRoom(now)
	return nil
}

// makeRoom drops expired entries from the cold end, then least recently used
// ones, until the bounds hold again.
func (m *Memory) makeRoom(now time.Time) {
	// A few cold entries are checked on every write, so expired entries do not
	// wait for capacity pressure.
	el := m.lru.Back()
	for range 4 {
		if el == nil {
			break
		}
		prev := el.Prev()
		if !now.Before(el.Value.(*memoryEntry).expiresAt) {
			m.remove(el)
		}
		el = prev
	}
	for (len(m.entries) > m.opts.MaxEntries || m.bytes > m.opts.MaxBytes) && m.lru.Len() > 0 {
		m.remove(m.lru.Back())
	}
}

func (m *Memory) remove(el *list.Element) {
	e := m.lru.Remove(el).(*memoryEntry)
	delete(m.entries, e.key)
	m.bytes -= e.cost()
}

// Delete implements Cache.
func (m *Memory) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := CheckKey(key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.entries[key]; ok {
		m.remove(el)
	}
	return nil
}

// Fetch implements Cache.
func (m *Memory) Fetch(ctx context.Context, key string, ttl time.Duration, compute func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	return FetchThrough(ctx, m, &m.flight, key, ttl, compute)
}

func emptyNotNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}
