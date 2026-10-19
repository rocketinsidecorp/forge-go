# ADR 0032: Application cache

- **Status:** Accepted
- **Date:** 2026-10-19

## Context

Rails 8 ships Solid Cache: the application cache lives in the database the
application already operates, trading a little latency for capacity, durability
across restarts and no extra moving part. Forge applications need the same
(fragment and query results, remote API responses, expensive computations), on
all four database configurations, plus an in-process option for single
instances and tests.

Caches are also a place where security bugs hide: values written under keys an
attacker influences (poisoning), unbounded growth, secrets in keys that end up
in logs or tables, and thundering herds on a cold key.

## Decision

### Package layout

| Package | Role |
|---------|------|
| `cache` (core) | `Cache` interface, `Options`, bounded `Memory` (LRU + TTL), `Flight` (stampede protection), `FetchThrough`, JSON helpers, `Key` |
| `cache/cachetest` (core) | conformance suite |
| `cache/postgres`, `/mysql`, `/sqlite` | Solid Cache equivalent: shared table `forge_cache_entries`, embedded migrations |

### The interface

```go
type Cache interface {
    Get(ctx, key) ([]byte, bool, error)
    Set(ctx, key, value []byte, ttl) error
    Delete(ctx, key) error
    Fetch(ctx, key, ttl, compute func(ctx) ([]byte, error)) ([]byte, error)
}
```

Values are `[]byte`; `GetJSON`, `SetJSON` and `FetchJSON` give typed access. A
value that no longer decodes (the type changed in a deploy) is a **miss**, so a
stale entry is recomputed, never served or turned into a 500. A miss is not an
error. Stores copy on the way in and out. `ttl == 0` means the store's
`DefaultTTL` (default 1 hour); negative or more than one year is an error;
there is no "never expires": everything is bounded in time as well as in size.

### Stampede protection

`Fetch` runs `compute` once per key per process (`Flight`, a keyed
single-flight). It owns no goroutines: the computation runs on the first
caller's goroutine and waiters wait on a channel or their own context. If the
first caller is cancelled (not the waiters), the waiters do **not** inherit its
cancellation: one of them takes over and computes. Every caller receives its
own copy of the bytes, a panicking computation releases its waiters, and
`compute` errors are returned but never cached. Replicas do not coordinate: with
`N` replicas a cold key can be computed up to `N` times. A distributed lock
would add failure modes (lock expiry, owner death) for a benefit that rarely
matters; applications that need it can use an advisory lock around `Fetch`.

A failing cache never fails `Fetch`: a read error degrades to a miss and a
write error is logged (without key or value) and the computed value is returned.
Errors that are the caller's bug (`ErrValueTooLarge`, `ErrInvalidTTL`) are
returned. Direct `Get`/`Set`/`Delete` calls report store errors.

### Bounds

| Bound | Memory | Database stores |
|-------|--------|-----------------|
| Entries (`MaxEntries`, default 10,000) | enforced on every write, LRU | enforced when pruning: the **oldest writes** go first (FIFO by write time, like Solid Cache, which does not track reads) |
| Bytes | `MaxBytes` (default 64 MiB) including a per-entry overhead | `MaxEntries x MaxValueBytes` |
| Value size (`MaxValueBytes`, default 1 MiB, at most 8 MiB) | yes | yes, and a `CHECK` in the schema |
| Key size | 1 KiB, no NUL | same |
| Age | per-entry TTL | per-entry TTL on the database clock |

`Memory` starts no goroutines: expired entries are dropped when read and a few
cold entries are checked on every write. The database stores prune with
`Prune(ctx)`: up to `MaxPruneRounds` (20) batches of `PruneBatch` (default 500)
expired entries, then, if more than `MaxEntries` remain, up to as many batches
of the oldest. They also prune inline after a write, **after every `PruneBatch`
writes or every `PruneInterval` (default one minute), whichever comes first**,
after the write has succeeded and ignoring errors. A time-only trigger was
rejected: eviction capped at one batch per interval let a stream of new keys
grow the table to thousands of rows over `MaxEntries`. With the write counter the
bound is *soft but tight*: the table stays within about `PruneBatch` entries per
writing process (plus writes in flight) of `MaxEntries`. Each prune counts the
table, a scan; with millions of rows, raise `PruneBatch`.

### Keys are hashed; keys and values are never logged

The database stores address rows by `SHA-256(key)` (32 bytes). Keys often name
users, sessions or e-mail addresses; they are never stored, indexed or logged,
and errors never contain them. The cost is that keys cannot be listed or
deleted by pattern; use a namespace in the key and let TTLs and eviction
clean up (the Solid Cache trade-off).

`cache.Key(namespace, parts...)` builds keys unambiguously (the namespace and
every part are length-prefixed), so neither a user-controlled part nor a
dynamic namespace can reach into another namespace.

### Time

PostgreSQL and MySQL/MariaDB compute expiry on the database clock, so replica
clock skew cannot extend or shorten a TTL. SQLite uses the application clock
(single host).

## Consequences

- Applications get a bounded, shared, restart-surviving cache with no new
  infrastructure, and an in-memory cache for one instance.
- Nothing in the cache is authoritative: callers must treat a miss, a stale
  decode and a store outage the same way.
- Large objects belong in `storage`, not the cache (8 MiB ceiling).
- A cache is only as trustworthy as its writers: whoever can write the table
  can poison every replica. The table is application-privileged like any other
  (see the threat model).
- No tag/pattern invalidation and no hit statistics; both would need read
  tracking or key storage, which this design deliberately avoids.
