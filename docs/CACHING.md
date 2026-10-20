# Caching

Forge's `cache` package is the equivalent of Rails' Solid Cache: a key/value
cache with expiry that lives in the database you already run, plus a bounded
in-memory cache. The design is in [ADR 0032](adr/0032-cache.md).

## The interface

```go
type Cache interface {
    Get(ctx, key string) (value []byte, ok bool, err error)
    Set(ctx, key string, value []byte, ttl time.Duration) error
    Delete(ctx, key string) error
    Fetch(ctx, key string, ttl time.Duration, compute func(ctx) ([]byte, error)) ([]byte, error)
}
```

```go
price, err := cache.FetchJSON(ctx, c, cache.Key("price", sku), 10*time.Minute,
    func(ctx context.Context) (Price, error) { return catalog.Price(ctx, sku) })
```

- `Fetch` is read-through: on a miss it runs `compute` **once per key per
  process** however many requests wait for it (stampede protection), stores the
  result and returns it. Errors are returned and never cached. If the first
  caller's request is cancelled, a waiting caller takes over instead of
  inheriting the cancellation.
- A cache failure never fails `Fetch`: an unreadable cache is a miss, a failed
  write is logged (without key or value) and the computed value is returned.
  Direct `Get`/`Set`/`Delete` report store errors.
- A cached value that no longer decodes (`GetJSON`/`FetchJSON`; for example the
  struct changed in a deploy) is a miss and is recomputed.
- `ttl == 0` means the store's `DefaultTTL`; a negative or over-a-year ttl is an
  error. Nothing lives forever.
- Values and keys are copied; `[]byte` values may contain anything. Keys are
  1 to 1024 bytes without NUL; values at most `MaxValueBytes` (default 1 MiB,
  hard limit 8 MiB). Put large objects in [storage](STORAGE.md).
- Across replicas the stampede protection does not coordinate: `N` replicas can
  compute a cold key up to `N` times.

## Stores

| Store | Package | Shared across replicas | Survives restart |
| ----- | ------- | ---------------------- | ---------------- |
| Memory (LRU + TTL) | `cache` | no | no |
| PostgreSQL | `cache/postgres` | yes | yes |
| MySQL, MariaDB | `cache/mysql` | yes | yes |
| SQLite | `cache/sqlite` | processes of one host | yes |

```go
// bootstrap, PostgreSQL shown (mysql.CommandsFromSets / sqlite.CommandsFromSets otherwise)
forge.Main(bootstrap.Configure,
    postgres.CommandsFromSets(db.Migrations(), cachepostgres.Migrations())...)

c, err := cachepostgres.NewStore(db, cache.StoreOptions{Options: cache.OptionsFromConfig(cfg.Cache)})
// or in memory:
c, err := cache.NewMemory(cache.MemoryOptionsFromConfig(cfg.Cache))
```

`app rollback` never reverts this framework migration, and removing the set
from `CommandsFromSets` later keeps `migrate` working
([framework migrations](DATABASE.md#framework-migrations)).

Database rows are addressed by `SHA-256(key)`: keys are never stored, indexed or
logged (they often name users or sessions), and cannot be listed or deleted by
pattern. Namespace keys (`cache.Key("users", id, "profile")`) and let TTLs and
eviction clean up.

## Configuration

| Variable | Default | Meaning |
| -------- | ------- | ------- |
| `FORGE_CACHE_MAX_ENTRIES` | `10000` | Entry bound of every cache |
| `FORGE_CACHE_MAX_BYTES` | `67108864` | Memory bound of the in-memory cache (keys, values and per-entry overhead) |
| `FORGE_CACHE_MAX_VALUE_BYTES` | `1048576` | Largest cached value, at most `8388608`; the byte bound must exceed it by 4096 |
| `FORGE_CACHE_DEFAULT_TTL` | `1h` | ttl used when a write passes `0`; `1ms` to `8760h` |

Invalid values stop the process at startup without echoing them.

## Bounds and pruning

Memory enforces `MaxEntries` and `MaxBytes` on every write by evicting the
least recently used entry; it drops expired entries when read and checks a few
cold ones on each write, and starts no goroutines.

The database stores are pruned in bounded batches: expired entries first, then
the **oldest writes** while more than `MaxEntries` remain (FIFO by write time,
like Solid Cache; reads are not tracked). One `store.Prune(ctx)` call runs at
most `cache.MaxPruneRounds` (20) batches of `StoreOptions.PruneBatch` (default
500) of each kind. Writes call it inline **after every `PruneBatch` writes or
every `PruneInterval` (default one minute), whichever comes first** (`-1`
disables inline pruning), so sustained writes of new keys cannot outrun
eviction. You can also run it on a schedule:

```go
sched.Register("cache.prune", jobs.ScheduleSpec{Every: time.Minute}, func(ctx context.Context) error {
    _, err := store.Prune(ctx)
    return err
})
```

The entry bound on the database stores is **soft but tight**: each process
prunes after every `PruneBatch` writes, and a prune evicts the whole excess (up
to 20 batches), so the table stays within about `PruneBatch` entries per writing
process, plus writes in flight, of `MaxEntries`. A burst of more than
`20 x PruneBatch` new keys between two prunes by one process is cleared over the
next prunes. Each prune counts the table (a scan): with millions of rows, raise
`PruneBatch` so it runs less often, and leave headroom in `MaxEntries`.

## Security notes

- **Poisoning.** Whoever can write a key controls what every reader of that key
  sees. Never build a key from untrusted input without a namespace and
  `cache.Key` (the namespace and every part are length-prefixed, so neither a part nor a dynamic namespace such as `"tenant:"+id` can reach into another namespace), and
  never cache per-user data under a key a different user can compute or choose.
  Cache entries are untrusted bytes: decode them strictly (`GetJSON` does) and
  do not cache authorization decisions longer than the permission can change.
- **Secrets.** Do not put secrets or tokens in values you would not keep in the
  database in clear text; cache rows are plain application data. Keys are
  hashed at rest, and Forge never logs keys or values.
- **Tenancy.** The cache is not tenant-aware. Put the tenant ID in the key
  (`cache.Key("invoices", tenantID, id)`); a key without it is shared by all
  tenants.
- **Access.** The table is as writable as the application's database role. Do
  not share the cache table with a less trusted application.
- **Size.** Values are bounded (1 MiB default) and entries are bounded, so a
  client that causes many distinct keys cannot grow the cache without limit;
  it can evict useful entries, which only costs recomputation.

## Testing

`cachetest.Run(t, func(t testing.TB, o cache.Options) cache.Cache { ... })` is
the conformance suite every store passes.
