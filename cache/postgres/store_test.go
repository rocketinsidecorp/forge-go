package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/cache"
	"github.com/rocketinsidecorp/forge-go/cache/cachetest"
	cachepostgres "github.com/rocketinsidecorp/forge-go/cache/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

func newStore(t testing.TB, opts cache.StoreOptions) *cachepostgres.Store {
	t.Helper()
	store, err := cachepostgres.NewStore(postgrestest.NewMigrated(t, cachepostgres.Migrations()), opts)
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
	db := postgrestest.NewMigrated(t, cachepostgres.Migrations())
	store, err := cachepostgres.NewStore(db, cache.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), "session:alice@example.com", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM forge_cache_entries WHERE position('alice'::bytea in key_hash) > 0`).Scan(&n); err != nil || n != 0 {
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
	for _, key := range []string{"a", "b", "c", "d", "e", "f"} {
		if err := store.Set(ctx, key, []byte("v"), time.Hour); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Millisecond)
	}
	for i, want := range []int64{2, 2, 1, 0} { // 6 entries -> bound 1, two per batch
		if removed, err := store.Prune(ctx); err != nil || removed != want {
			t.Fatalf("Prune #%d = %d, %v; want %d", i+1, removed, err, want)
		}
	}
}

func TestNewStoreValidation(t *testing.T) {
	if _, err := cachepostgres.NewStore(nil, cache.StoreOptions{}); err == nil {
		t.Fatal("nil database accepted")
	}
	db := postgrestest.NewMigrated(t, cachepostgres.Migrations())
	if _, err := cachepostgres.NewStore(db, cache.StoreOptions{Options: cache.Options{MaxEntries: -1}}); err == nil {
		t.Fatal("negative MaxEntries accepted")
	}
}
