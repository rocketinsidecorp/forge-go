# ADR 0006: Zero-dependency core

- **Status:** Accepted; amended by [ADR 0007](0007-postgresql-adapter-with-pgx.md);
  [ADR 0026](0026-dependency-free-telemetry.md) implements OpenTelemetry
  protocols in the core without a dependency
- **Date:** 2026-09-26

## Context

Every third-party module in a framework becomes a dependency of every
application built with it. Each adds supply-chain risk, version conflicts,
upgrade work, and audit cost. Go's standard library now covers routing
(`net/http` 1.22 patterns), structured logging (`log/slog`), cryptographic
randomness, JSON, and `database/sql` interfaces.

## Decision

The Forge core (`go.mod` module `github.com/rocketinsidecorp/forge-go`) has
no third-party Go module dependencies. It requires Go 1.27.1.

- Needed functionality is implemented on the standard library, as done for
  UUIDv7 ([ADR 0001](0001-uuidv7-default-identifier.md)) and HTTP
  ([ADR 0002](0002-stdlib-net-http-core.md)).
- Features that require external code (for example a PostgreSQL driver in
  Phase 2 or OpenTelemetry in Phase 5) will be introduced so that
  applications that do not use them do not inherit them, for example as
  separate modules. The exact mechanism is decided per feature in its own
  ADR.
- Development tools (`golangci-lint`, `govulncheck`) are installed
  separately and are not listed in `go.mod`.

## Consequences

- Core packages import only the standard library. Since ADR 0007 the module
  itself has `go.sum` entries for the `postgres` adapter's dependencies
  (pgx and its transitive set); `govulncheck` covers those as well as the
  standard library and toolchain. Applications that do not import
  `postgres` do not link them.
- Forge maintains more code itself (UUID generation, router hardening) and
  must test it thoroughly.
- Some features will ship later than they would with a library.
- Adding any third-party dependency to the core requires a new ADR.
