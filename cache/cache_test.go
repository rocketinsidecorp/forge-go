package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rocketinsidecorp/forge-go/web"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newClock() *clock { return &clock{now: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)} }

var bg = context.Background()

func TestMemoryEvictsLeastRecentlyUsed(t *testing.T) {
	m, err := NewMemory(MemoryOptions{Options: Options{MaxEntries: 3}})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if err := m.Set(bg, k, []byte(k), time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, _ := m.Get(bg, "a"); !ok { // a becomes the most recently used
		t.Fatal("a missing")
	}
	if err := m.Set(bg, "d", []byte("d"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := m.Get(bg, "b"); ok {
		t.Fatal("b was the least recently used and must be evicted")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok, _ := m.Get(bg, k); !ok {
			t.Fatalf("%s was evicted", k)
		}
	}
	if m.Len() != 3 {
		t.Fatalf("Len = %d", m.Len())
	}
}

func TestMemoryIsBoundedByBytes(t *testing.T) {
	m, err := NewMemory(MemoryOptions{Options: Options{MaxValueBytes: 1000, MaxEntries: 1_000_000}, MaxBytes: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 500 {
		if err := m.Set(bg, fmt.Sprintf("key-%d", i), make([]byte, 400), time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	m.mu.Lock()
	bytes, n := m.bytes, len(m.entries)
	m.mu.Unlock()
	if bytes > 10_000 || n == 0 || n > 25 {
		t.Fatalf("held %d bytes in %d entries, want at most 10000 bytes", bytes, n)
	}
	if _, ok, _ := m.Get(bg, "key-499"); !ok {
		t.Fatal("the newest entry must be kept")
	}
}

func TestMemoryDropsExpiredEntriesWithoutReads(t *testing.T) {
	c := newClock()
	m, _ := newMemory(MemoryOptions{}, c.Now)
	for i := range 50 {
		_ = m.Set(bg, fmt.Sprintf("old-%d", i), []byte("v"), time.Second)
	}
	c.Advance(2 * time.Second)
	for i := range 50 { // each write drops a few expired cold entries
		_ = m.Set(bg, fmt.Sprintf("new-%d", i), []byte("v"), time.Hour)
	}
	if got := m.Len(); got > 60 {
		t.Fatalf("Len = %d, want expired entries dropped by writes", got)
	}
}

func TestMemoryTTLWithInjectedClock(t *testing.T) {
	c := newClock()
	m, _ := newMemory(MemoryOptions{Options: Options{DefaultTTL: 10 * time.Second}}, c.Now)
	_ = m.Set(bg, "k", []byte("v"), 0)
	c.Advance(9 * time.Second)
	if _, ok, _ := m.Get(bg, "k"); !ok {
		t.Fatal("expired early")
	}
	c.Advance(time.Second)
	if _, ok, _ := m.Get(bg, "k"); ok {
		t.Fatal("must expire exactly at its ttl")
	}
	if m.Len() != 0 {
		t.Fatal("expired entry must be dropped on read")
	}
}

func TestMemoryConcurrentUseUnderRace(t *testing.T) {
	m, _ := NewMemory(MemoryOptions{Options: Options{MaxEntries: 50}})
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 400 {
				key := fmt.Sprintf("k%d", (g+i)%80)
				switch i % 4 {
				case 0:
					_ = m.Set(bg, key, []byte("v"), time.Second)
				case 1:
					_, _, _ = m.Get(bg, key)
				case 2:
					_ = m.Delete(bg, key)
				default:
					_, _ = m.Fetch(bg, key, time.Second, func(context.Context) ([]byte, error) { return []byte("f"), nil })
				}
			}
		}()
	}
	wg.Wait()
	if m.Len() > 50 {
		t.Fatalf("Len = %d, exceeds MaxEntries", m.Len())
	}
}

func TestNewMemoryValidation(t *testing.T) {
	for name, opts := range map[string]MemoryOptions{
		"negative entries":      {Options: Options{MaxEntries: -1}},
		"value above limit":     {Options: Options{MaxValueBytes: MaxValueBytesLimit + 1}},
		"tiny ttl":              {Options: Options{DefaultTTL: time.Nanosecond}},
		"bytes below one value": {Options: Options{MaxValueBytes: 1 << 20}, MaxBytes: 1024},
	} {
		if _, err := NewMemory(opts); err == nil {
			t.Errorf("%s: NewMemory succeeded", name)
		}
	}
}

func TestStoreOptionsNormalize(t *testing.T) {
	o, err := StoreOptions{}.Normalize()
	if err != nil || o.MaxEntries != 10_000 || o.MaxValueBytes != 1<<20 || o.DefaultTTL != time.Hour || o.PruneInterval != time.Minute || o.PruneBatch != 500 {
		t.Fatalf("defaults = %+v, %v", o, err)
	}
	if _, err := (StoreOptions{PruneBatch: -1}).Normalize(); err == nil {
		t.Fatal("negative batch accepted")
	}
	if o, _ := (StoreOptions{PruneInterval: -1}).Normalize(); o.PruneInterval != -1 {
		t.Fatal("negative interval must stay (disables pruning)")
	}
}

func TestKeyIsUnambiguous(t *testing.T) {
	if Key("ns", "a", "bc") == Key("ns", "ab", "c") {
		t.Fatal("part boundaries must be preserved")
	}
	if Key("ns", "x:1:y") == Key("ns", "x", "y") {
		t.Fatal("delimiters inside a part must not forge another key")
	}
	if err := CheckKey(Key("users", "42", "profile")); err != nil {
		t.Fatalf("Key produced an invalid key: %v", err)
	}
}

func TestKeyNamespaceIsLengthPrefixedToo(t *testing.T) {
	// A dynamic namespace ("tenant:"+id) must never equal a shorter namespace
	// plus parts, or one tenant could read another's entries.
	pairs := [][2]string{
		{Key("a:1:b"), Key("a", "b")},
		{Key("tenant:7"), Key("tenant", "7")},
		{Key("a:1:b", "c"), Key("a", "b", "c")},
		{Key(""), Key("", "")},
		{Key("1:a"), Key("1", "a")},
	}
	for i, p := range pairs {
		if p[0] == p[1] {
			t.Errorf("pair %d: distinct (namespace, parts) produced the same key %q", i, p[0])
		}
	}
	seen := map[string][3]string{}
	for _, ns := range []string{"", "a", "a:1", "a:1:b", "1:a", "tenant:7"} {
		for _, parts := range [][]string{nil, {""}, {"a"}, {"b"}, {"a", "b"}, {"1:a"}, {"", ""}} {
			k := Key(ns, parts...)
			id := [3]string{ns, strings.Join(parts, "\x00"), string(rune('0' + len(parts)))}
			if prev, dup := seen[k]; dup && prev != id {
				t.Errorf("collision: %v and %v both give %q", prev, id, k)
			}
			seen[k] = id
		}
	}
}

func TestEffectiveTTL(t *testing.T) {
	o, _ := Options{DefaultTTL: time.Minute}.Normalize()
	for in, want := range map[time.Duration]time.Duration{0: time.Minute, time.Hour: time.Hour, time.Microsecond: time.Millisecond} {
		if got, err := o.EffectiveTTL(in); err != nil || got != want {
			t.Errorf("EffectiveTTL(%s) = %s, %v; want %s", in, got, err, want)
		}
	}
	if _, err := o.EffectiveTTL(-1); !errors.Is(err, ErrInvalidTTL) {
		t.Error("negative ttl accepted")
	}
	if _, err := o.EffectiveTTL(maxTTL + 1); !errors.Is(err, ErrInvalidTTL) {
		t.Error("excessive ttl accepted")
	}
}

// --- Fetch behavior with a failing store ---

type brokenStore struct {
	getErr, setErr error
	sets           atomic.Int32
}

func (b *brokenStore) Get(context.Context, string) ([]byte, bool, error) { return nil, false, b.getErr }
func (b *brokenStore) Set(context.Context, string, []byte, time.Duration) error {
	b.sets.Add(1)
	return b.setErr
}

func TestFetchThroughDegradesWhenTheStoreFailsAndNeverLogsKeysOrValues(t *testing.T) {
	var logs bytes.Buffer
	ctx := web.WithLogger(bg, slog.New(slog.NewTextHandler(&logs, nil)))
	store := &brokenStore{getErr: errors.New("db down"), setErr: errors.New("db down")}
	var flight Flight
	v, err := FetchThrough(ctx, store, &flight, "session:alice-token-123", time.Minute, func(context.Context) ([]byte, error) {
		return []byte("secret-value-456"), nil
	})
	if err != nil || string(v) != "secret-value-456" {
		t.Fatalf("FetchThrough = %q, %v; want the computed value despite the failing store", v, err)
	}
	out := logs.String()
	if !strings.Contains(out, "cache read failed") || !strings.Contains(out, "cache write failed") {
		t.Fatalf("store failures were not logged: %q", out)
	}
	if strings.Contains(out, "alice-token-123") || strings.Contains(out, "secret-value-456") {
		t.Fatalf("log leaked a key or value: %q", out)
	}
}

func TestFetchThroughReportsCallerBugsInsteadOfSwallowingThem(t *testing.T) {
	store := &brokenStore{setErr: ErrValueTooLarge}
	var flight Flight
	if _, err := FetchThrough(bg, store, &flight, "k", time.Minute, func(context.Context) ([]byte, error) { return []byte("v"), nil }); !errors.Is(err, ErrValueTooLarge) {
		t.Fatalf("err = %v, want ErrValueTooLarge", err)
	}
}

// A computation that panics releases the callers waiting on it. The test
// runs in a synctest bubble so "the waiter has joined the flight" is
// observable: once the leader is asleep inside its computation and every
// other goroutine is blocked, the waiter is parked on the in-flight call.
func TestFlightPanicReleasesWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var f Flight
		started := make(chan struct{})
		waiter := make(chan error, 1)
		leaderDone := make(chan struct{})
		go func() {
			defer close(leaderDone)
			defer func() { _ = recover() }()
			_, _ = f.Do(bg, "k", func(context.Context) ([]byte, error) {
				close(started)
				time.Sleep(100 * time.Millisecond)
				panic("boom")
			})
		}()
		<-started
		go func() {
			_, err := f.Do(bg, "k", func(context.Context) ([]byte, error) { return []byte("x"), nil })
			waiter <- err
		}()
		synctest.Wait() // the waiter is blocked on the leader's call
		select {
		case err := <-waiter:
			if err == nil || !strings.Contains(err.Error(), "panicked") {
				t.Fatalf("waiter err = %v, want the leader's panic reported", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a panicking computation left its waiters blocked")
		}
		<-leaderDone
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.calls) != 0 {
			t.Fatal("flight leaked a call")
		}
	})
}

func TestFlightWaiterStopsWhenItsContextEnds(t *testing.T) {
	var f Flight
	release := make(chan struct{})
	running := make(chan struct{})
	go func() {
		_, _ = f.Do(bg, "k", func(context.Context) ([]byte, error) {
			close(running)
			<-release
			return nil, nil
		})
	}()
	<-running
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	if _, err := f.Do(ctx, "k", func(context.Context) ([]byte, error) { return nil, nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter = %v, want its own deadline", err)
	}
	close(release)
}

// --- JSON helpers ---

type profile struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func TestJSONHelpers(t *testing.T) {
	m, _ := NewMemory(MemoryOptions{})
	if _, ok, err := GetJSON[profile](bg, m, "p"); ok || err != nil {
		t.Fatalf("empty GetJSON = %v, %v", ok, err)
	}
	if err := SetJSON(bg, m, "p", profile{"Ana", 30}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := GetJSON[profile](bg, m, "p"); !ok || err != nil || v != (profile{"Ana", 30}) {
		t.Fatalf("GetJSON = %+v, %v, %v", v, ok, err)
	}
	var calls int
	for range 3 {
		v, err := FetchJSON(bg, m, "q", time.Minute, func(context.Context) (profile, error) {
			calls++
			return profile{"Bo", 7}, nil
		})
		if err != nil || v != (profile{"Bo", 7}) {
			t.Fatalf("FetchJSON = %+v, %v", v, err)
		}
	}
	if calls != 1 {
		t.Fatalf("compute ran %d times, want 1", calls)
	}
}

func TestJSONHelpersTreatUndecodableEntriesAsMisses(t *testing.T) {
	m, _ := NewMemory(MemoryOptions{})
	_ = m.Set(bg, "p", []byte(`{"name": 12}`), time.Minute) // stale schema
	if _, ok, err := GetJSON[profile](bg, m, "p"); ok || err != nil {
		t.Fatalf("GetJSON on a stale entry = %v, %v; want a miss", ok, err)
	}
	v, err := FetchJSON(bg, m, "p", time.Minute, func(context.Context) (profile, error) { return profile{"New", 1}, nil })
	if err != nil || v != (profile{"New", 1}) {
		t.Fatalf("FetchJSON = %+v, %v; want a recomputed value", v, err)
	}
	raw, _, _ := m.Get(bg, "p")
	var stored profile
	if err := json.Unmarshal(raw, &stored); err != nil || stored.Name != "New" {
		t.Fatalf("the stale entry was not replaced: %s", raw)
	}
}

// A value that does not decode is counted every time and logged once per
// key, at warn level, with a hash of the key and never the key or the value.
func TestUndecodableEntriesAreCountedAndLoggedOncePerKey(t *testing.T) {
	m, _ := NewMemory(MemoryOptions{})
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	ctx := context.Background()
	key := "profiles:" + t.Name()
	_ = m.Set(ctx, key, []byte(`{"name": "secret-value-12345", "age": "old"}`), time.Minute)
	before := DecodeFailures()
	for range 3 {
		if _, ok, err := GetJSON[profile](ctx, m, key); ok || err != nil {
			t.Fatalf("GetJSON on a stale entry = %v, %v; want a miss", ok, err)
		}
	}
	// FetchJSON reads the entry twice but counts it once, then replaces it.
	if _, err := FetchJSON(ctx, m, key, time.Minute, func(context.Context) (profile, error) { return profile{"New", 1}, nil }); err != nil {
		t.Fatal(err)
	}
	if got := DecodeFailures() - before; got != 4 {
		t.Fatalf("DecodeFailures grew by %d, want 4", got)
	}
	output := logs.String()
	if n := strings.Count(output, "cached value does not decode"); n != 1 {
		t.Fatalf("logged %d times, want once per key:\n%s", n, output)
	}
	if !strings.Contains(output, "level=WARN") || !strings.Contains(output, "key_hmac=") {
		t.Errorf("the log line lacks the level or the keyed key hash:\n%s", output)
	}
	if strings.Contains(output, key) || strings.Contains(output, "secret-value-12345") {
		t.Errorf("the log line leaks the key or the value:\n%s", output)
	}
	// Not an unkeyed hash of the key, which a guess could be checked against.
	plain := sha256.Sum256([]byte(key))
	if strings.Contains(output, hex.EncodeToString(plain[:8])) {
		t.Errorf("the log line holds an unkeyed SHA-256 of the key:\n%s", output)
	}
	// The replaced entry decodes again: no new failure.
	if _, ok, _ := GetJSON[profile](ctx, m, key); !ok {
		t.Fatal("the replaced entry does not decode")
	}
	if got := DecodeFailures() - before; got != 4 {
		t.Fatalf("DecodeFailures grew by %d after a good read, want 4", got)
	}
}

func TestFetchJSONPropagatesComputeErrors(t *testing.T) {
	m, _ := NewMemory(MemoryOptions{})
	boom := errors.New("boom")
	if _, err := FetchJSON(bg, m, "k", time.Minute, func(context.Context) (profile, error) { return profile{}, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}
