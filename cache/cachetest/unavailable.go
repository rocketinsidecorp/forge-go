package cachetest

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/cache"
)

// NewSabotagedCache returns a cache on fresh storage and a function that
// makes that storage unavailable (a dropped table, a lost connection). The
// function returns the one that makes the storage available again.
type NewSabotagedCache func(t testing.TB) (c cache.Cache, sabotage func(t testing.TB) (restore func()))

// RunUnavailable checks what every database-backed cache must do when its
// storage fails: reads, writes, deletes and pruning report the failure (a
// failed read is never an ordinary miss), Fetch still returns the computed
// value because failures of the cache never fail it, and the entries stored
// before the outage are intact afterwards.
func RunUnavailable(t *testing.T, newCache NewSabotagedCache) {
	t.Helper()
	c, sabotage := newCache(t)
	mustSet(t, c, "kept", []byte("value"), time.Hour)

	restore := sabotage(t)
	if v, ok, err := c.Get(ctx, "kept"); err == nil || ok || v != nil {
		t.Errorf("Get during the outage = %q, %v, %v; want the failure, never a miss or a value", v, ok, err)
	}
	if err := c.Set(ctx, "new", []byte("x"), time.Hour); err == nil {
		t.Error("Set succeeded during the outage")
	}
	if err := c.Delete(ctx, "kept"); err == nil {
		t.Error("Delete succeeded during the outage")
	}
	if pruner, ok := c.(cache.Pruner); ok {
		if _, err := pruner.Prune(ctx); err == nil {
			t.Error("Prune succeeded during the outage")
		}
	}
	computed := 0
	got, err := c.Fetch(ctx, "fetched", time.Hour, func(context.Context) ([]byte, error) {
		computed++
		return []byte("computed"), nil
	})
	if err != nil || !bytes.Equal(got, []byte("computed")) || computed != 1 {
		t.Errorf("Fetch during the outage = %q, %v (computed %d times); want the computed value", got, err, computed)
	}
	computeErr := errors.New("compute failed")
	if _, err := c.Fetch(ctx, "fetched-too", time.Hour, func(context.Context) ([]byte, error) { return nil, computeErr }); !errors.Is(err, computeErr) {
		t.Errorf("Fetch with a failing compute = %v, want the compute error", err)
	}
	restore()

	if v, ok := mustGet(t, c, "kept"); !ok || string(v) != "value" {
		t.Errorf("the entry stored before the outage = %q, %v", v, ok)
	}
	if _, ok := mustGet(t, c, "new"); ok {
		t.Error("a write that failed during the outage is visible afterwards")
	}
	if _, ok := mustGet(t, c, "fetched"); ok {
		t.Error("Fetch stored a value although the store was unavailable")
	}
}
