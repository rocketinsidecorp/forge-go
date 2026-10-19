package cache

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/rocketinsidecorp/forge-go/web"
)

// Flight collapses concurrent computations of the same key in one process
// into one: the first caller computes, the others wait for its result. It
// owns no goroutines: the computation runs on the first caller's goroutine,
// and waiters stop waiting when their own context ends.
//
// A Flight is the stampede protection of every Cache in this module. The zero
// value is ready to use.
type Flight struct {
	mu    sync.Mutex
	calls map[string]*call
}

type call struct {
	done  chan struct{}
	value []byte
	err   error
}

// Do runs fn for key, or waits for the run already in progress. Every caller
// gets its own copy of the value. If the caller that runs fn is cancelled
// (its own context ended), the waiters whose contexts are still alive run fn
// themselves instead of inheriting that cancellation.
func (f *Flight) Do(ctx context.Context, key string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	for {
		f.mu.Lock()
		if f.calls == nil {
			f.calls = make(map[string]*call)
		}
		if c, ok := f.calls[key]; ok {
			f.mu.Unlock()
			select {
			case <-c.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if c.err != nil && isContextError(c.err) && ctx.Err() == nil {
				continue // the leader was cancelled, not us: take over
			}
			if c.err != nil {
				return nil, c.err
			}
			return slices.Clone(c.value), nil
		}
		c := &call{done: make(chan struct{})}
		f.calls[key] = c
		f.mu.Unlock()
		return f.lead(ctx, key, c, fn)
	}
}

func (f *Flight) lead(ctx context.Context, key string, c *call, fn func(ctx context.Context) ([]byte, error)) (value []byte, err error) {
	completed := false
	defer func() {
		if !completed { // fn panicked: release the waiters, then let it propagate
			c.err = errors.New("cache: compute panicked")
		}
		f.mu.Lock()
		delete(f.calls, key)
		f.mu.Unlock()
		close(c.done)
	}()
	c.value, c.err = fn(ctx)
	completed = true
	if c.err != nil {
		return nil, c.err
	}
	return slices.Clone(c.value), nil
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// Basic is the part of a Cache that FetchThrough needs.
type Basic interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// FetchThrough implements Cache.Fetch for any store: read, and on a miss run
// compute once per key in this process through flight, then write. Failures
// of the store degrade to a miss (and a failed write is logged without the key
// or value), so an unavailable cache slows the application down instead of
// taking it down.
func FetchThrough(ctx context.Context, store Basic, flight *Flight, key string, ttl time.Duration, compute func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	if err := CheckKey(key); err != nil {
		return nil, err
	}
	if compute == nil {
		return nil, errors.New("cache: compute function is required")
	}
	value, ok, err := store.Get(ctx, key)
	switch {
	case err == nil && ok:
		return value, nil
	case err != nil && ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		web.Logger(ctx).Warn("cache read failed; computing without the cache", "error", err)
	}
	return flight.Do(ctx, key, func(ctx context.Context) ([]byte, error) {
		// A caller that finished just before us may have filled the cache.
		if value, ok, err := store.Get(ctx, key); err == nil && ok {
			return value, nil
		}
		value, err := compute(ctx)
		if err != nil {
			return nil, err
		}
		if err := store.Set(ctx, key, value, ttl); err != nil {
			if errors.Is(err, ErrInvalidTTL) || errors.Is(err, ErrValueTooLarge) {
				return nil, err // the caller's bug, not an outage
			}
			web.Logger(ctx).Warn("cache write failed; returning the computed value", "error", err)
		}
		return value, nil
	})
}
