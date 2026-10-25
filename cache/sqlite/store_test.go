package sqlite_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/cache"
	"github.com/rocketinsidecorp/forge-go/cache/cachetest"
	cachesqlite "github.com/rocketinsidecorp/forge-go/cache/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
)

func newStore(t testing.TB, opts cache.StoreOptions) *cachesqlite.Store {
	t.Helper()
	store, err := cachesqlite.NewStore(sqlitetest.NewMigrated(t, cachesqlite.Migrations()), opts)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestStoreConformance(t *testing.T) {
	cachetest.Run(t, func(t testing.TB, o cache.Options) cache.Cache {
		return newStore(t, cache.StoreOptions{Options: o, PruneInterval: -1})
	})
}

func TestKeysAreNotStored(t *testing.T) {
	db := sqlitetest.NewMigrated(t, cachesqlite.Migrations())
	store, err := cachesqlite.NewStore(db, cache.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), "session:alice@example.com", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM forge_cache_entries WHERE instr(key_hash, CAST('alice' AS BLOB)) > 0`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows containing the key: %d, %v; want the key stored only as a hash", n, err)
	}
}

func TestSetPrunesOpportunistically(t *testing.T) {
	store := newStore(t, cache.StoreOptions{Options: cache.Options{MaxEntries: 3}, PruneInterval: time.Millisecond, PruneBatch: 100})
	ctx := context.Background()
	for _, key := range []string{"a", "b", "c", "d", "e", "f"} {
		if err := store.Set(ctx, key, []byte("v"), time.Hour); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Writes pruned inline: after one more explicit prune the bound holds.
	if _, err := store.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	kept := 0
	for _, key := range []string{"a", "b", "c", "d", "e", "f"} {
		if _, ok, _ := store.Get(ctx, key); ok {
			kept++
		}
	}
	if kept != 3 {
		t.Fatalf("%d entries kept, want MaxEntries=3", kept)
	}
}

func TestPruneIsBounded(t *testing.T) {
	store := newStore(t, cache.StoreOptions{Options: cache.Options{MaxEntries: 1}, PruneInterval: -1, PruneBatch: 2})
	ctx := context.Background()
	for i := range 60 {
		if err := store.Set(ctx, fmt.Sprintf("k%02d", i), []byte("v"), time.Hour); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	// One call is bounded work: MaxPruneRounds batches of 2.
	for i, want := range []int64{cache.MaxPruneRounds * 2, 19, 0} {
		if removed, err := store.Prune(ctx); err != nil || removed != want {
			t.Fatalf("Prune #%d = %d, %v; want %d", i+1, removed, err, want)
		}
	}
}

func TestSustainedWritesOfNewKeysStayNearTheBound(t *testing.T) {
	// The default interval is a minute: only the write counter can keep up.
	db := sqlitetest.NewMigrated(t, cachesqlite.Migrations())
	store, err := cachesqlite.NewStore(db, cache.StoreOptions{Options: cache.Options{MaxEntries: 100}, PruneBatch: 50})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := range 2000 {
		if err := store.Set(ctx, fmt.Sprintf("k%d", i), []byte("v"), time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM forge_cache_entries`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows > 100+2*50 {
		t.Fatalf("%d rows after 2000 writes of new keys with MaxEntries=100, want at most %d", rows, 100+2*50)
	}
}

func TestNewStoreValidation(t *testing.T) {
	if _, err := cachesqlite.NewStore(nil, cache.StoreOptions{}); err == nil {
		t.Fatal("nil database accepted")
	}
	db := sqlitetest.NewMigrated(t, cachesqlite.Migrations())
	if _, err := cachesqlite.NewStore(db, cache.StoreOptions{Options: cache.Options{MaxEntries: -1}}); err == nil {
		t.Fatal("negative MaxEntries accepted")
	}
}

func TestStoreFailsWhenItsTableIsUnavailable(t *testing.T) {
	cachetest.RunUnavailable(t, func(tb testing.TB) (cache.Cache, func(testing.TB) func()) {
		db := sqlitetest.NewMigrated(tb, cachesqlite.Migrations())
		store, err := cachesqlite.NewStore(db, cache.StoreOptions{PruneInterval: -1})
		if err != nil {
			tb.Fatal(err)
		}
		rename := func(tb testing.TB, from, to string) {
			tb.Helper()
			if _, err := db.ExecContext(context.Background(), "ALTER TABLE "+from+" RENAME TO "+to); err != nil {
				tb.Fatal(err)
			}
		}
		return store, func(tb testing.TB) func() {
			rename(tb, "forge_cache_entries", "forge_cache_entries_gone")
			return func() { rename(tb, "forge_cache_entries_gone", "forge_cache_entries") }
		}
	})
}
