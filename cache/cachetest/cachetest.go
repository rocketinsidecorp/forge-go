// Package cachetest is the conformance suite every cache.Cache must pass.
// Run it against a real database for the shared stores:
//
//	func TestCache(t *testing.T) {
//	    cachetest.Run(t, func(t testing.TB, o cache.Options) cache.Cache { ... a cache on a fresh, migrated database ... })
//	}
package cachetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/cache"
)

// NewCache returns a cache on fresh storage that the test cleans up, bounded
// by o. Stores with deferred maintenance must disable their opportunistic
// pruning (the suite calls Prune itself when the cache has one).
type NewCache func(t testing.TB, o cache.Options) cache.Cache

// shortTTL is short so expiry can be observed with real clocks, including the
// database clocks of the shared stores.
const shortTTL = 400 * time.Millisecond

// Run executes the suite.
func Run(t *testing.T, newCache NewCache) {
	t.Helper()
	tests := []struct {
		name string
		run  func(*testing.T, NewCache)
	}{
		{"SetGetDelete", setGetDelete},
		{"ValuesAreCopied", valuesAreCopied},
		{"BinaryAndEmptyValues", binaryAndEmptyValues},
		{"OverwriteReplacesValueAndTTL", overwriteReplaces},
		{"ValuesExpire", valuesExpire},
		{"ZeroTTLUsesTheDefault", zeroTTLUsesDefault},
		{"RejectsInvalidInput", rejectsInvalidInput},
		{"KeysAreExactAndIndependent", keysAreExact},
		{"FetchComputesOnceAndCaches", fetchComputesOnce},
		{"FetchCollapsesConcurrentMisses", fetchCollapses},
		{"FetchDoesNotCacheErrors", fetchDoesNotCacheErrors},
		{"FetchWaiterSurvivesCancelledLeader", fetchWaiterSurvivesCancelledLeader},
		{"HonorsCancelledContext", honorsCancelledContext},
		{"EnforcesTheEntryBound", enforcesEntryBound},
		{"PrunesExpiredEntries", prunesExpired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { test.run(t, newCache) })
	}
}

var ctx = context.Background()

func mustSet(t *testing.T, c cache.Cache, key string, value []byte, ttl time.Duration) {
	t.Helper()
	if err := c.Set(ctx, key, value, ttl); err != nil {
		t.Fatalf("Set(%q): %v", key, err)
	}
}

func mustGet(t *testing.T, c cache.Cache, key string) ([]byte, bool) {
	t.Helper()
	value, ok, err := c.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	return value, ok
}

func setGetDelete(t *testing.T, newCache NewCache) {
	c := newCache(t, cache.Options{})
	if _, ok := mustGet(t, c, "k"); ok {
		t.Fatal("empty cache reported a hit")
	}
	mustSet(t, c, "k", []byte("v"), time.Minute)
	if v, ok := mustGet(t, c, "k"); !ok || string(v) != "v" {
		t.Fatalf("Get = %q, %v; want v, true", v, ok)
	}
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok := mustGet(t, c, "k"); ok {
		t.Fatal("deleted key still present")
	}
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatalf("deleting a missing key: %v", err)
	}
}

func valuesAreCopied(t *testing.T, newCache NewCache) {
	c := newCache(t, cache.Options{})
	in := []byte("abc")
	mustSet(t, c, "k", in, time.Minute)
	in[0] = 'X'
	out, _ := mustGet(t, c, "k")
	if string(out) != "abc" {
		t.Fatalf("mutating the slice passed to Set changed the cache: %q", out)
	}
	out[0] = 'Y'
	again, _ := mustGet(t, c, "k")
	if string(again) != "abc" {
		t.Fatalf("mutating a slice returned by Get changed the cache: %q", again)
	}
}

func binaryAndEmptyValues(t *testing.T, newCache NewCache) {
	c := newCache(t, cache.Options{})
	binary := []byte{0, 1, 2, 0xff, 0xfe, 0, '\n', '\'', '"', '\\', 0}
	mustSet(t, c, "bin", binary, time.Minute)
	if v, ok := mustGet(t, c, "bin"); !ok || !bytes.Equal(v, binary) {
		t.Fatalf("binary value = %v, %v", v, ok)
	}
	mustSet(t, c, "empty", nil, time.Minute)
	if v, ok := mustGet(t, c, "empty"); !ok || v == nil || len(v) != 0 {
		t.Fatalf("empty value = %#v, %v; want an empty non-nil slice and a hit", v, ok)
	}
	big := bytes.Repeat([]byte{0xab}, 512<<10)
	mustSet(t, c, "big", big, time.Minute)
	if v, ok := mustGet(t, c, "big"); !ok || !bytes.Equal(v, big) {
		t.Fatalf("512 KiB value did not round-trip (len %d, hit %v)", len(v), ok)
	}
	keyWith := "ключ/é/ \t'\";--"
	mustSet(t, c, keyWith, []byte("u"), time.Minute)
	if v, ok := mustGet(t, c, keyWith); !ok || string(v) != "u" {
		t.Fatal("unusual key did not round-trip")
	}
}

func overwriteReplaces(t *testing.T, newCache NewCache) {
	c := newCache(t, cache.Options{})
	mustSet(t, c, "k", []byte("one"), shortTTL)
	time.Sleep(shortTTL / 2)
	mustSet(t, c, "k", []byte("two"), time.Minute)
	time.Sleep(shortTTL)
	if v, ok := mustGet(t, c, "k"); !ok || string(v) != "two" {
		t.Fatalf("after overwrite Get = %q, %v; want the new value with the new ttl", v, ok)
	}
}

func valuesExpire(t *testing.T, newCache NewCache) {
	c := newCache(t, cache.Options{})
	mustSet(t, c, "k", []byte("v"), shortTTL)
	if _, ok := mustGet(t, c, "k"); !ok {
		t.Fatal("entry must be readable before it expires")
	}
	time.Sleep(shortTTL + 150*time.Millisecond)
	if _, ok := mustGet(t, c, "k"); ok {
		t.Fatal("expired entry was returned")
	}
}

func zeroTTLUsesDefault(t *testing.T, newCache NewCache) {
	c := newCache(t, cache.Options{DefaultTTL: shortTTL})
	mustSet(t, c, "k", []byte("v"), 0)
	if _, ok := mustGet(t, c, "k"); !ok {
		t.Fatal("entry with the default ttl must be readable")
	}
	time.Sleep(shortTTL + 150*time.Millisecond)
	if _, ok := mustGet(t, c, "k"); ok {
		t.Fatal("zero ttl must use the (short) default and expire")
	}
}

func rejectsInvalidInput(t *testing.T, newCache NewCache) {
	c := newCache(t, cache.Options{MaxValueBytes: 1024})
	for name, key := range map[string]string{
		"empty key":  "",
		"NUL in key": "a\x00b",
		"too long":   strings.Repeat("k", cache.MaxKeyLength+1),
	} {
		if err := c.Set(ctx, key, []byte("v"), time.Minute); !errors.Is(err, cache.ErrInvalidKey) {
			t.Errorf("%s: Set = %v, want ErrInvalidKey", name, err)
		}
		if _, _, err := c.Get(ctx, key); !errors.Is(err, cache.ErrInvalidKey) {
			t.Errorf("%s: Get = %v, want ErrInvalidKey", name, err)
		}
		if err := c.Delete(ctx, key); !errors.Is(err, cache.ErrInvalidKey) {
			t.Errorf("%s: Delete = %v, want ErrInvalidKey", name, err)
		}
	}
	if err := c.Set(ctx, strings.Repeat("k", cache.MaxKeyLength), []byte("v"), time.Minute); err != nil {
		t.Errorf("maximum-length key: %v", err)
	}
	if err := c.Set(ctx, "k", make([]byte, 1025), time.Minute); !errors.Is(err, cache.ErrValueTooLarge) {
		t.Errorf("oversized value: %v, want ErrValueTooLarge", err)
	}
	if err := c.Set(ctx, "k", make([]byte, 1024), time.Minute); err != nil {
		t.Errorf("value at the limit: %v", err)
	}
	if err := c.Set(ctx, "k", []byte("v"), -time.Second); !errors.Is(err, cache.ErrInvalidTTL) {
		t.Errorf("negative ttl: %v, want ErrInvalidTTL", err)
	}
	if _, ok := mustGet(t, c, strings.Repeat("z", 10)); ok {
		t.Error("unexpected hit")
	}
}

func keysAreExact(t *testing.T, newCache NewCache) {
	c := newCache(t, cache.Options{})
	mustSet(t, c, "a", []byte("1"), time.Minute)
	mustSet(t, c, "A", []byte("2"), time.Minute)
	mustSet(t, c, "a ", []byte("3"), time.Minute)
	for key, want := range map[string]string{"a": "1", "A": "2", "a ": "3"} { //nolint:gocritic // mapKey: the key "a " proves keys are space-sensitive.
		if v, ok := mustGet(t, c, key); !ok || string(v) != want {
			t.Errorf("Get(%q) = %q, %v; want %q (keys are case- and space-sensitive)", key, v, ok, want)
		}
	}
	if _, ok := mustGet(t, c, "a  "); ok {
		t.Error("trailing spaces must not collide")
	}
}

func fetchComputesOnce(t *testing.T, newCache NewCache) {
	c := newCache(t, cache.Options{})
	var calls atomic.Int32
	compute := func(context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte("computed"), nil
	}
	for range 3 {
		v, err := c.Fetch(ctx, "k", time.Minute, compute)
		if err != nil || string(v) != "computed" {
			t.Fatalf("Fetch = %q, %v", v, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("compute ran %d times, want 1 (later calls hit the cache)", calls.Load())
	}
	if v, ok := mustGet(t, c, "k"); !ok || string(v) != "computed" {
		t.Fatal("Fetch must store its result")
	}
}

func fetchCollapses(t *testing.T, newCache NewCache) {
	c := newCache(t, cache.Options{})
	const callers = 24
	var calls atomic.Int32
	release := make(chan struct{})
	var started sync.WaitGroup
	started.Add(callers)
	var wg sync.WaitGroup
	results := make([]string, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started.Done()
			v, err := c.Fetch(ctx, "hot", time.Minute, func(context.Context) ([]byte, error) {
				calls.Add(1)
				<-release
				return []byte("value"), nil
			})
			if err != nil {
				t.Errorf("Fetch: %v", err)
				return
			}
			v[0] = 'V' // every caller owns its copy
			results[i] = string(v)
		}()
	}
	started.Wait()
	time.Sleep(150 * time.Millisecond) // let the callers reach the flight
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("compute ran %d times for %d concurrent misses, want 1", calls.Load(), callers)
	}
	for i, r := range results {
		if r != "Value" {
			t.Fatalf("caller %d got %q", i, r)
		}
	}
	if v, _ := mustGet(t, c, "hot"); string(v) != "value" {
		t.Fatalf("cache holds %q after callers mutated their copies", v)
	}
}

func fetchDoesNotCacheErrors(t *testing.T, newCache NewCache) {
	c := newCache(t, cache.Options{})
	boom := errors.New("origin down")
	if _, err := c.Fetch(ctx, "k", time.Minute, func(context.Context) ([]byte, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("Fetch = %v, want the compute error", err)
	}
	if _, ok := mustGet(t, c, "k"); ok {
		t.Fatal("a failed computation was cached")
	}
	v, err := c.Fetch(ctx, "k", time.Minute, func(context.Context) ([]byte, error) { return []byte("ok"), nil })
	if err != nil || string(v) != "ok" {
		t.Fatalf("Fetch after a failure = %q, %v; want a fresh computation", v, err)
	}
	if _, err := c.Fetch(ctx, "k2", time.Minute, nil); err == nil {
		t.Fatal("nil compute accepted")
	}
	if _, err := c.Fetch(ctx, "", time.Minute, func(context.Context) ([]byte, error) { return nil, nil }); !errors.Is(err, cache.ErrInvalidKey) {
		t.Fatalf("Fetch with an invalid key = %v", err)
	}
}

func fetchWaiterSurvivesCancelledLeader(t *testing.T, newCache NewCache) {
	c := newCache(t, cache.Options{})
	leaderCtx, cancelLeader := context.WithCancel(ctx)
	leaderRunning := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		_, err := c.Fetch(leaderCtx, "k", time.Minute, func(ctx context.Context) ([]byte, error) {
			close(leaderRunning)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		leaderDone <- err
	}()
	<-leaderRunning
	waiterDone := make(chan string, 1)
	go func() {
		v, err := c.Fetch(ctx, "k", time.Minute, func(context.Context) ([]byte, error) { return []byte("waiter"), nil })
		if err != nil {
			waiterDone <- "error: " + err.Error()
			return
		}
		waiterDone <- string(v)
	}()
	time.Sleep(150 * time.Millisecond)
	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader = %v, want context.Canceled", err)
	}
	select {
	case got := <-waiterDone:
		if got != "waiter" {
			t.Fatalf("waiter got %q; it must compute for itself when the leader is cancelled", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never finished")
	}
}

func honorsCancelledContext(t *testing.T, newCache NewCache) {
	c := newCache(t, cache.Options{})
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := c.Get(cancelled, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get = %v, want context.Canceled", err)
	}
	if err := c.Set(cancelled, "k", []byte("v"), time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("Set = %v, want context.Canceled", err)
	}
}

func enforcesEntryBound(t *testing.T, newCache NewCache) {
	const bound, total = 5, 14
	c := newCache(t, cache.Options{MaxEntries: bound})
	for i := range total {
		mustSet(t, c, fmt.Sprintf("k%02d", i), []byte("v"), time.Hour)
		time.Sleep(3 * time.Millisecond) // distinct write times: eviction is oldest-write-first
	}
	if p, ok := c.(cache.Pruner); ok {
		for range total { // each call removes one bounded batch
			if removed, err := p.Prune(ctx); err != nil {
				t.Fatal(err)
			} else if removed == 0 {
				break
			}
		}
	}
	present := 0
	for i := range total {
		if _, ok := mustGet(t, c, fmt.Sprintf("k%02d", i)); ok {
			present++
		}
	}
	if present != bound {
		t.Fatalf("%d entries survive with MaxEntries=%d", present, bound)
	}
	for i := total - bound; i < total; i++ {
		if _, ok := mustGet(t, c, fmt.Sprintf("k%02d", i)); !ok {
			t.Errorf("recent entry k%02d was evicted", i)
		}
	}
	if _, ok := mustGet(t, c, "k00"); ok {
		t.Error("the oldest entry must be evicted first")
	}
}

func prunesExpired(t *testing.T, newCache NewCache) {
	c := newCache(t, cache.Options{})
	p, ok := c.(cache.Pruner)
	if !ok {
		t.Skip("cache keeps no persistent entries")
	}
	for i := range 3 {
		mustSet(t, c, fmt.Sprintf("short-%d", i), []byte("v"), shortTTL)
	}
	mustSet(t, c, "long", []byte("v"), time.Hour)
	time.Sleep(shortTTL + 150*time.Millisecond)
	removed, err := p.Prune(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Fatalf("Prune removed %d entries, want the 3 expired ones", removed)
	}
	if _, ok := mustGet(t, c, "long"); !ok {
		t.Fatal("Prune removed a live entry")
	}
	if removed, err := p.Prune(ctx); err != nil || removed != 0 {
		t.Fatalf("second Prune = %d, %v; want 0", removed, err)
	}
}
