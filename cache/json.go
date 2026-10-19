package cache

import (
	"context"
	"encoding/json"
	"time"
)

// GetJSON decodes the value under key into a T. A value that no longer
// decodes (for example after the type changed in a deploy) is a miss, so a
// stale entry is recomputed, not served or turned into an error.
func GetJSON[T any](ctx context.Context, c Cache, key string) (T, bool, error) {
	var zero T
	raw, ok, err := c.Get(ctx, key)
	if err != nil || !ok {
		return zero, false, err
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return zero, false, nil
	}
	return value, true, nil
}

// SetJSON stores value as JSON under key for ttl.
func SetJSON[T any](ctx context.Context, c Cache, key string, value T, ttl time.Duration) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.Set(ctx, key, raw, ttl)
}

// FetchJSON is Fetch for a typed value: the result of compute is stored as
// JSON, and a cached value that no longer decodes is recomputed.
func FetchJSON[T any](ctx context.Context, c Cache, key string, ttl time.Duration, compute func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	if value, ok, err := GetJSON[T](ctx, c, key); err == nil && ok {
		return value, nil
	}
	raw, err := c.Fetch(ctx, key, ttl, func(ctx context.Context) ([]byte, error) {
		value, err := compute(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(value)
	})
	if err != nil {
		return zero, err
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		// Fetch returned an entry that does not decode: replace it.
		value, err = compute(ctx)
		if err != nil {
			return zero, err
		}
		if err := SetJSON(ctx, c, key, value, ttl); err != nil {
			return zero, err
		}
	}
	return value, nil
}
