# ADR 0007: PostgreSQL adapter with pgx, in the main module

- **Status:** Accepted (amends [ADR 0006](0006-zero-dependency-core.md));
  amended by [ADR 0009](0009-multiple-databases.md), which adds MySQL,
  MariaDB and SQLite adapters following the same pattern
- **Date:** 2026-09-26

## Context

PostgreSQL is Forge's primary database and the default for generated
applications (see [ADR 0009](0009-multiple-databases.md) for the other
choices). A driver is required; writing one is
out of the question. ADR 0006 left open how a third-party dependency enters
the framework without reaching applications that do not need it.

Options considered:

1. `database/sql` with a registered driver. Portable, but hides
   PostgreSQL-specific capabilities (COPY, LISTEN/NOTIFY, native types,
   pipelining) behind a lowest-common-denominator API and adds reflection.
2. `github.com/jackc/pgx/v5` natively, in a separate Go module
   (`forge-go/postgres`). Maximum isolation, but every change spans two
   modules, releases need coordinated tags, and generated applications need
   two requirements kept in step.
3. pgx natively, in the `postgres` package of the main module, with the core
   packages kept free of it by an automated check.

## Decision

Option 3.

- `postgres` uses pgx v5 and `pgxpool` directly. Its `DBTX` interface
  matches the one sqlc generates for pgx/v5, so hand-written and generated
  repositories are interchangeable.
- The core packages (`forge`, `config`, `fault`, `health`, `httpserver`,
  `pagination`, `router`, `uuid`, `web`, and since ADR 0009 `sqldb`,
  `migrate` and `dbtest`) import only the standard library and each other. `architecture_test.go` enforces this with `go list -deps` and
  fails CI on any violation.
- Go's module graph pruning means an application that never imports
  `postgres` neither builds nor links pgx.
- The core declares the extension point (`forge.Command`, `App.OnShutdown`,
  `App.Readiness`); the adapter depends on the core, never the reverse.

## Consequences

- One module, one version, one tag. `go.mod` now lists pgx and its small
  transitive set (pgpassfile, pgservicefile, puddle, x/sync, x/text), which
  `govulncheck` covers.
- Adding another adapter dependency (Redis, OpenTelemetry) follows the same
  pattern and needs its own ADR, as the MySQL and SQLite drivers did
  ([ADR 0009](0009-multiple-databases.md)).
- If the dependency set grows enough to burden applications, adapters can
  still move to separate modules without changing their import-facing API.

## Addendum (2026-10-27): connections a cancellation fired on

pgx v5.11.0 can return a poisoned connection to the pool. When a write takes
longer than 15 ms, pgconn starts a background reader, which may read the
whole result and wait for more before the write returns. If the query's
context is then cancelled, pgx's context watcher sets a socket deadline, the
pending background read fails with `i/o timeout`, and the error is stored for
the next read. The query itself succeeds, the deadline is cleared on unwatch,
and the next query on the connection fails with `i/o timeout` (on the extended
protocol; the simple protocol's first read happens to discard it). pgx's
comment in `exitPotentialWriteReadDeadlock` describes the hazard. On a
CPU-saturated host, 32 goroutines whose contexts were cancelled at random
poisoned connections 27 times in 509,000 cancellable queries over five
minutes; with the guard below, 0 times in 265,000.

`postgres.Open` therefore wraps the pool's `BuildContextWatcherHandler` so that
a firing cancellation marks the connection (an atomic flag in pgconn's
`CustomData`), and adds a `PrepareConn` that destroys a marked connection
before it is handed out; Acquire then takes or opens another. Any hook the
configuration already had keeps running. Every socket deadline on a pooled
connection comes from its context watcher, so an unmarked connection cannot
hold the stored error, and a query whose context is not cancelled while it
runs marks nothing: the guard costs no reconnections under normal load. A
cancellation that fails its query already closes the connection.

Considered and rejected: `AfterRelease` runs in a goroutine on every release,
during which a concurrent Acquire can open an extra connection;
`CancelRequestContextWatcherHandler` still sets the fallback deadline (and
waits 100 ms on every cancellation); a handler without a deadline cannot
interrupt a query on a dead network.

What the guard does not cover:
- A statement on the same acquired connection after the cancelled one, as
  inside a transaction. When the request itself was cancelled the
  transaction ends anyway; but when only one statement had its own derived
  timeout, the next statement on that connection (`COMMIT` included) can
  fail with `i/o timeout` although the server ran it. A commit that fails
  this way is ambiguous: treat it like any commit whose outcome is unknown.
  This predates the guard.
- The connect phase: pgx uses its own deadline handler while connecting, so
  a connection whose connect timeout fires just after the server's
  ReadyForQuery could carry the stored error unmarked. The window is a few
  microseconds wide.
- A marked connection idle for over a second is pinged by the pool before
  PrepareConn destroys it: one wasted round trip. In a storm of
  cancellations, Acquire can meet more marked connections in a row than the
  pool retries and return its "too many failed attempts" error, as it
  already does for failing pings.
- The guard owns PrepareConn; pgxpool ignores BeforeAcquire when
  PrepareConn is set, and Open sets neither, so none is lost today.
`TestCancelledQueryPoisonsUnguardedPool` reproduces the interleaving
deterministically and fails when a pgx upgrade stops storing the error, at
which point the guard can be revisited.
