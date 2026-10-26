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
```

`FORGE_TEST_REQUIRE_DATABASE=true` and `FORGE_TEST_REQUIRE_SSHD=true` turn a
missing server or `sshd` into a failure instead of a skip; CI sets both, so
these tests can never silently stop running there.

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

## Coverage

CI's `coverage` job measures statement coverage of the module and fails when
it drops below the committed floor in `.coverage-floor`:

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
- **The floor only rises.** It is 95.0%, set on 2026-10-25 against a gated
  total of 96.0% (94.9% gross, kits included), measured on PostgreSQL 16 and
  18 and MariaDB; it started at 84.0% and is raised, never lowered, as tests
  are added, toward the project's target of 95%. A change that lowers
  coverage below the floor fails CI; raise the floor in the same pull request
  when coverage grows.
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
`docs/test-strategy.md`.
