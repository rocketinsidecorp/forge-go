# Testing

How Forge-Go's own test suite runs and what CI requires of it. Generated
applications have their own tests; see [GETTING_STARTED.md](GETTING_STARTED.md).

## Running the tests

```sh
go test ./...                                   # unit tests; database tests skip
FORGE_TEST_POSTGRES_URL=postgres://forge:forge@127.0.0.1:5432/postgres?sslmode=disable \
FORGE_TEST_MYSQL_URL=mysql://root:forge@127.0.0.1:3306/mysql \
FORGE_TEST_REQUIRE_DATABASE=true go test -race ./...   # with the database tests
FORGE_TEST_REQUIRE_SSHD=true go test ./internal/deploy/  # deploy over a real sshd
npm install --global --ignore-scripts typescript@6.0.3   # the TypeScript checker (optional locally)
FORGE_TEST_REQUIRE_TSC=true go test -run TypeScript ./internal/cli   # type-check a generated application with tsc
```

`FORGE_TEST_REQUIRE_DATABASE=true`, `FORGE_TEST_REQUIRE_SSHD=true` and
`FORGE_TEST_REQUIRE_TSC=true` turn a missing server, `sshd` or `tsc` into a
failure instead of a skip; CI sets them, so these tests can never silently stop
running there (the generated application's TypeScript is type-checked in the
`generated` job, [ADR 0049](adr/0049-typescript-by-default.md)).

The PostgreSQL tests that need `uuidv7()` (any application table with
`DEFAULT uuidv7()`; Forge's identity migrations no longer use it) require
PostgreSQL 18. On an older
server they skip with `requires PostgreSQL 18 (uuidv7()): the test server is
PostgreSQL 16.x`, or fail with that reason when `FORGE_TEST_REQUIRE_DATABASE=true`
is set. A test of your own that calls `uuidv7()` starts with
`postgrestest.RequireUUIDv7(t, db)`; `postgrestest.NewMigrated`, `NewMigratedSets`
and `NewRLS` check the migrations they apply themselves.

## Deterministic tests

The suite runs in parallel on loaded CI runners, so a test may not depend on
how fast the machine is:

- **Executable fixtures.** A fake `ssh`, database client or hook script that
  a test runs as a process is created with `internal/exectest.Write`, never
  `os.WriteFile` followed by an exec. Writing in-process lets a parallel
  test's `fork` inherit the write descriptor, and the exec then fails with
  `text file busy` (ETXTBSY, golang/go#22315); `Write` creates the file in a
  child process, so this process never holds it open for writing.
- **No sleep as synchronization.** Wait for the event itself: a channel, a
  marker file or a condition polled against a deadline. Code driven by timers
  and goroutines runs inside `testing/synctest`, where the clock is fake and
  `synctest.Wait` says when every goroutine is blocked. Real sockets and
  processes cannot run in a bubble, so those tests keep real time and give
  every timeout a wide margin over the pause it must outlast.
- **Sleeps that remain** only wait for a time-based expiry to pass (a lower
  bound that load can only lengthen) or bound a negative check ("nothing
  happened within N ms").
- **Polling has one home.** A condition another goroutine, process or server
  makes true is awaited with `internal/waittest` (`waittest.Until(t, timeout,
  what, cond)`, or `waittest.Poll` when the caller words the failure): it
  returns the moment the condition holds and fails, naming what it waited for,
  only after the timeout. Do not write a `for ... time.Sleep` loop.
- **Every `time.Sleep` in a test is accounted for.** `test_sleeps_test.go`
  parses the test files and fails on a sleep without a comment on its line (or
  the line above) that starts with `real-clock:` and gives a reason (a database
  stamps the expiry, a real HTTP server's timeout must pass, a helper process
  waits to be killed, a negative check), or `fake-clock:` for a sleep inside a
  `synctest` bubble, where it costs no real time (the marker is refused
  anywhere else, and `real-clock:` is refused inside a bubble). The number of
  real-clock sleeps has a budget in that file, `maxRealClockSleeps`, that
  only goes down (no directory is exempt: the `jobs/` scheduler and tick
  tests run in `synctest` bubbles, and the only real-clock sleeps left in `cache/` space the writes of
  the database store tests so the stores' own clocks order them).
- **Timeouts are explicit.** CI and `scripts/coverage.sh` run `go test
  -timeout 30m`: the default of ten minutes is shorter than the serial
  generated-application workflows of `internal/cli` on a slow runner, and a
  timeout is then a failure unrelated to the change. `TestGeneratedApplication`
  runs in its own CI job (`generated`); the `quality` job skips it with
  `-skip`.

## Coverage

CI's `coverage` job measures statement coverage of the module and fails when
the gated total drops below the committed floor in `.coverage-floor`, when the
gross total drops below its floor, or when a security-relevant package drops
below its own floor (three gates, all in `scripts/coverage.sh`):

```sh
scripts/coverage.sh                                # measure, print the table, gate
COVERAGE_PROFILE=cover.out scripts/coverage.sh     # report and gate an existing profile
```

- **How it is measured.** `go test -count=1 -coverpkg=./... ./...` with
  PostgreSQL 18 (`FORGE_TEST_POSTGRES_URL`), PostgreSQL 16
  (`FORGE_TEST_DATABASE_URL`), MariaDB (`FORGE_TEST_MYSQL_URL`) and a real
  `sshd`. With `-coverpkg` every package's tests count for every package; the
  script merges the profile so a block is covered when any test binary ran
  it, then reads the total with `go tool cover -func`. The job writes the
  total and a per-package table to the run's summary and uploads
  `dist/coverage` (the raw profile, the gross merged profile
  `coverage-gross.out` and the gated one `coverage.out`).
- **Two totals.** The script prints both. The *gross* total counts every
  package except the four exclusions below. The *gated* total, the one the
  floor checks, also leaves out the contract and test kits listed under
  "Contract and test kits".
- **The floor only rises.** It is 96.0%, raised from 95.0% on 2026-10-26
  (QG12) against a gated total of 96.26% (95.05% gross, kits included),
  measured on PostgreSQL 16 and 18 and MariaDB; it started at 84.0% and is
  raised, never lowered, as tests are added. The gates compare covered and
  total statements exactly, not rounded percentages (the script prints two
  decimals; `go tool cover` would show 95.96 as 96.0). A change that lowers
  coverage below the floor fails CI; raise the floor in the same pull request
  when coverage grows.
- **The gross floor.** The gross total, which counts the kits, must also be at
  least 95.0% (`gross_floor` in `scripts/coverage.sh`). The gated total leaves
  the kits out by an explicit list; the gross gate keeps that list from
  becoming a way to hide code: a package added to `kits` without tests still
  has to be covered enough for the gross total to hold.
- **Per-package floors.** Each security-relevant package must reach 95% of its
  own statements (`package_floor` and `guarded` in `scripts/coverage.sh`), so
  a well covered module cannot average away an untested one: `auth`,
  `auth/postgres` (the identity store), `credentials`, `csrf`, `encryption`,
  `internal/pgurl` (the PostgreSQL URL isolation) and `postgres`. The table in
  the job summary lists them with their numbers; a listed package that is not
  measured fails too. The comparison is exact, not the table's one-decimal
  rounding. A package joins the list in the pull request that brings it to
  95%, and the list only grows.
- **What counts as covered there.** Tests assert behaviour: refusals, error
  paths and edge inputs, each with a failure that would show if the code were
  wrong (a state left unchanged, a secret absent from the error). Excluding
  code from the measurement to pass a floor is not allowed (see Exclusions).
  A few branches in these packages cannot be reached by any test and stay
  uncovered, each of one kind: the error return after a call that cannot fail
  (`crypto/rand`, which crashes instead of returning an error; marshalling
  data that is always valid; `uuid.New` over `crypto/rand`), a guard against a
  race between two system calls (the symbolic-link check in `credentials`), and
  a consistency check made after a row lock that the same transaction's
  earlier statement already guarantees (`auth/postgres`). Tests that need a
  file system enforcing permissions against the user (an unreadable file, a
  read-only directory) skip for root and run on CI's non-root runner, so the
  per-package numbers of a root run, such as a container's, are the lower
  bound.
- **Exclusions.** Only these, each for the reason given; nothing else may be
  excluded, and a new exclusion needs a reason here:

  | Excluded | Why |
  | --- | --- |
  | `cmd/*/main.go` | The `main` shims of `forge` and `forge-proxy`: one call each into `internal/cli.Run` and `internal/proxy.Main`, which are measured. A `main` function cannot run inside a test binary. |
  | `examples/**` | Example applications that show the framework in use; they are not framework code (their packages still build and test in CI). |
  | `internal/version` | Three variables the linker sets with `-ldflags -X`; there are no statements to run. |
  | `internal/deploy/internal/fakedocker` | The docker stand-in the deploy tests execute as a separate process over SSH; coverage of another process is not recorded in the test binary's profile. It is test code, not shipped behaviour. |

### Contract and test kits (decision D7)

Coverage measures production code. These packages are harnesses other
packages' tests call; their only code that does not run is the failure branch
of an assertion (`t.Fatalf` when a backend breaks the contract), which cannot
run while every backend passes. They stay verified by being run against every
real backend (PostgreSQL 18 and 16, MariaDB/MySQL, SQLite) in CI, where a
broken kit or a broken backend fails the suite. They are excluded from the
gated total only, and still appear in the gross one:

`cache/cachetest`, `queue/storetest`, `storage/storagetest`,
`migrate/migratetest`, `ratelimit/limitertest`, `mail/mailtest`,
`telemetry/telemetrytest`, `mysql/mysqltest`, `postgres/postgrestest`,
`sqlite/sqlitetest`, `dbtest`.

The list is explicit in `scripts/coverage.sh` (`kits`), never a pattern, so
production code cannot be hidden by a naming accident. The script also fails
when a listed kit does not exist, is not named `*test`, or is imported by a
file that is neither a `_test.go` file nor another kit: such a package is
production code and has to be covered.

The Forge-Rust twin (`scripts/coverage.sh`, `.coverage-floor`, the same job
name) measures line coverage with cargo-llvm-cov; see its
`docs/test-strategy.md`. It has one floor, for the gated total, and neither the
gross floor nor per-crate floors yet; bringing them to Rust is tracked as QR8.
