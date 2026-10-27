# ADR 0053: A core that needs nothing (module split, step 1)

- **Status:** Accepted
- **Date:** 2026-10-27 (decision D38, the module split before v0.1.0)
- **Amends:** [ADR 0006](0006-zero-dependency-core.md) and
  [ADR 0007](0007-postgresql-adapter-with-pgx.md) (the core rule of
  `architecture_test.go` now covers tests and is computed, not listed),
  [ADR 0048](0048-argon2id-password-hashing.md) (Argon2id leaves the core
  through a registration seam), [ADR 0052](0052-import-boundaries.md)
  (`queue/internal/jobscmd` merged into `queuecmd`, rule A-5 restated)
- **Parity with:** Forge-Rust's framework crates do not depend on `argon2`
  either: the generated application owns its `Argon2idPasswordHasher`. The
  Cargo-feature equivalent of the module split is tracked with the
  `modular-install` parity row in a later step.

## Context

The owner decided (D38) to split forge-go into Go modules before v0.1.0: a
core module with no `require` line, one module per database driver, and a
tools module for the `forge` CLI and forge-proxy. An application that uses
only HTTP then downloads and links no third-party code, and one that uses
PostgreSQL links pgx and nothing for MySQL or SQLite.

A Go module's requirements include what its tests import. Before this
change the packages that will form the core reached third-party code in
four ways:

1. `auth` imported `golang.org/x/crypto/argon2` for Argon2id (ADR 0048), so
   every application linked `x/crypto` and `x/sys`.
2. `internal/terminal`, which `forge setup` uses to read a password without
   echo, used `golang.org/x/sys/unix` for its termios ioctls.
3. Tests of core packages opened real databases: `sqldb`, `migrate`,
   `encryption`, the root package's log tests and `auth/internal/sqlidentity`
   used SQLite (or MySQL), `auth/recoveryqueue` used PostgreSQL.
4. Library packages that will live in different modules shared internal
   packages (`auth/internal/setupcmd`, `auth/internal/sqlidentity`,
   `queue/internal/jobscmd`, `internal/pgurl`). Go lets any package under a
   module path import that path's `internal/` packages, but once the
   importer is in another module the two can be resolved at different
   versions, so a library may not import another module's internal package.

This ADR is the first of four steps and changes no module boundary yet:
afterwards the core needs nothing, which the later steps rely on.

## Decision

- **The core is computed.** Every package is core except an explicit,
  commented list of prefixes in `architecture_test.go` (`nonCorePackages`:
  the database adapters and their test kits, `*/internal/integration`,
  `cmd/` and the CLI's `internal/` packages, `examples/`).
  `TestCorePackagesAndTheirTestsDependOnlyOnStandardLibraryAndCore` runs
  `go list -deps -test` on every core package, for Linux, macOS and Windows,
  and fails on any import of a
  package outside the standard library and the core, tests included. It
  replaces the hand-kept `corePackages` and `coreThirdPartyImports`. The
  root package's repository-wide tests that need the CLI's packages or yaml
  (Rails parity, supply chain, release gate) are the one exception, listed
  file by file (`rootToolTests`) until they move under `cmd/`; a listed file
  that no longer needs them fails the test.
- **Argon2id is registered, not linked.** `auth.RegisterArgon2id(kdf)` takes
  a function with the signature of `argon2.IDKey`. The first registration
  wins; later ones and a nil function are ignored; it is safe for concurrent
  use; hashers look the function up at each hash and verification, so the
  order of package initialization does not matter. The identity stores
  `auth/postgres`, `auth/mysql` and `auth/sqlite` import
  `golang.org/x/crypto/argon2` and register it in `init`, so generated
  `--auth` applications keep writing `$argon2id$` hashes with no change.
  An import rule lets only those three packages import `argon2`.
  `RegisterArgon2id` checks the function against known answers computed
  with `x/crypto/argon2.IDKey` and, independently, with the RustCrypto
  `argon2` 0.6.0 crate Forge-Rust uses. It returns whether this call's
  function is now the one in use (false for nil or a later registration).
  - At registration, two small answers (t=2, m=32 KiB, p=2, a 32-byte key;
    t=3, m=48 KiB, p=3, a 24-byte key: every argument differs); a miss
    panics. They catch a function that is not Argon2id v19 (`argon2.Key`,
    Argon2i, has the same signature) and one that ignores or clamps t, p,
    the key length, or a memory of 48 KiB or less. Each is needed: a
    wrapper fixed to either one's parameters passes the other.
  - At the first use (hash or verification) in the process, once, an answer
    at Forge's default cost (m=19456 KiB, t=2, p=1, a 32-byte key); it costs
    one hash, so it does not run at init. A miss is logged at error level
    and the function is treated as not registered: Argon2id hashes and
    verifications fail with `ErrPasswordAlgorithmUnavailable`, new passwords
    are PBKDF2, nothing panics in the request path. It catches any change at
    the default cost, such as a cap on memory.
  - Not detected: a function that is right at those three parameter sets
    and wrong at others. Forge writes only the default cost, so this
    concerns verifying stored hashes that name another cost (an older
    parameter set, or hashes from another system).
  - With nothing registered, new hashes are PBKDF2-HMAC-SHA256 with 600,000
    iterations (the format and parameters of ADR 0048's PBKDF2), and
    verifying a stored `$argon2id$` hash fails closed with
    `auth.ErrPasswordAlgorithmUnavailable`: before any hashing, never a
    mismatch, never a rehash at sign-in, audited as `auth.login.failed` (or
    the step-up's event) with the outcome `failed`. A password reset needs
    no verification and writes PBKDF2. A hasher set to Argon2id with nothing
    registered skips the rehash of a verified PBKDF2 hash (as when hashing
    is saturated), so a right password is not told from a wrong one by a
    `503`, and verifies unknown accounts against the PBKDF2 dummy, so they
    fail like a known PBKDF2 account with a wrong password. It is a `fault.Error` of category
    `unavailable`, so the HTTP adapters answer `503` through `web.Error`,
    which logs its cause naming the fix (link an identity store or call
    `auth.RegisterArgon2id(argon2.IDKey)`). A hasher set to Argon2id with
    `WithPasswordAlgorithm` refuses to hash the same way.
  - `DefaultPasswordAlgorithm` becomes a function (an API break, allowed
    before v0.1.0): Argon2id once registered, PBKDF2 before that or under
    `-tags forge_pbkdf2`, whose meaning is unchanged.
  - The legacy timing floor of ADR 0048 applies, by default, while the
    hasher writes Argon2id; a hasher that falls back to PBKDF2 already costs
    PBKDF2 on every check. Its calibration and padding are unchanged.
  - The core's tests register a fast fake with `IDKey`'s signature; the
    real Argon2id, Forge-Rust's fixture and the `password_verify` benchmark
    run in `auth/postgres`, and each identity store tests that linking it
    makes the default hasher write Forge-Rust's `$argon2id$` shape.
- **`internal/terminal` uses the standard library's `syscall`**: Linux
  `TCGETS`/`TCSETS`, macOS `TIOCGETA`/`TIOCSETA`, through
  `syscall.Syscall(SYS_IOCTL, …)`. The pseudo-terminal helper the tests
  shared (three copies) is `internal/ptytest`, on Linux (`TIOCSPTLCK`,
  `TIOCGPTN`) and macOS (`TIOCPTYGRANT`, `TIOCPTYUNLK`, `TIOCPTYGNAME`), so
  the echo-off test runs on both.
  - On macOS `syscall.Syscall` is a raw kernel trap (`SVC 0x80` on arm64,
    `SYSCALL` on amd64), not a call through libc, which is the only system
    call interface Apple supports; `golang.org/x/sys/unix` goes through
    libc. It is accepted because `ioctl` is BSD system call 54, unchanged in
    XNU since the first macOS, the Go toolchain still ships and tests
    `syscall.Syscall` on darwin, and a break fails closed:
    `IsTerminal` reports false and `ReadSecret` refuses to read, so `forge
    setup` asks for `--password-stdin` or `FORGE_SETUP_PASSWORD` and never
    reads a password with echo on. The macOS CI job runs the echo-off test,
    so a break shows there first. The alternative, if Apple ever breaks it,
    is reading the terminal through `stty -echo` (`os/exec`) on darwin, or
    letting the tools module own `x/sys` for the prompt.
- **Shared packages become public or move with their only users:**
  `auth/internal/setupcmd` is `auth/authsetup`, `auth/internal/sqlidentity`
  is `auth/authsql` (their exported names unchanged, minus the few only
  their own tests used), `queue/internal/jobscmd` is merged into
  `queue/queuecmd` (rule A-5 now reads "`queuecmd` imports no store; stores
  may delegate to `queuecmd`"), and `internal/pgurl` is
  `postgres/internal/pgurl`, which the `forge db` commands reach through the
  new `postgres.IsolatedConnConfig(raw, allowedKeys)` (refusals are
  `*postgres.URLRefusedError`).
- **Tests that need a driver move under the adapter that provides it**, as
  test-only packages: `sqlite/internal/integration` (the root package's log
  tests, `sqldb`'s query log and telemetry tests, `migrate`'s fault tests,
  `encryption`'s SQL rewriter tests, `authsql`'s fault tests),
  `mysql/internal/integration` (the rewriter on MySQL) and
  `postgres/internal/integration` (`auth/recoveryqueue`). Driver-free tests
  stay with their package; helpers the moved tests share are copied. The
  coverage gate measures every package through every test binary
  (`-coverpkg`), so the moved tests still count for the packages they test.

## Consequences

- An application that links no identity store and never calls
  `RegisterArgon2id` writes PBKDF2 hashes, and refuses (`503`, logged) to
  sign in an account whose stored hash is Argon2id. Generated applications
  always link an identity store with `--auth`; a custom store must register
  Argon2id once (docs/AUTHENTICATION.md shows the line).
- In such a misconfigured process the `503` of an account with an Argon2id
  hash differs from the `401` of an unknown account, so the response tells
  them apart until the deployment is fixed; the log names the fix.
- `auth.DefaultPasswordAlgorithm` is now called: `auth.DefaultPasswordAlgorithm()`.
- `internal/terminal` and `internal/ptytest` contain the core's only `unsafe`
  use (the pointer argument of the ioctl system call, in the form
  `unsafe.Pointer` rule 4 documents); the macOS CI job builds and tests it.
- **Residual:** on macOS the terminal ioctls are raw system calls, an ABI
  Apple does not promise (see above); a break disables the interactive
  password prompt, it never echoes a password.
- Until the root's repository-wide tests move under `cmd/`, the root
  package's test binary still links the CLI's packages; `rootToolTests`
  names them.
- `scripts/coverage.sh` guards `auth/authsql` and `postgres/internal/pgurl`
  under their new paths.
