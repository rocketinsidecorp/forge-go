## What and why

<!-- What does this change do, and which problem does it solve? Link issues: "Fixes #123". -->

## How it was tested

<!-- Commands you ran and their result. Database-backed changes: which databases (PostgreSQL, MySQL, MariaDB, SQLite)? -->

## Checklist

- [ ] Tests cover the change, and a bug fix includes a test that fails without it.
- [ ] `gofmt -l .`, `go vet ./...`, `golangci-lint run ./...` and `go test -race ./...` pass locally.
- [ ] Core packages still import only the standard library (`architecture_test.go`).
- [ ] New identifiers default to UUIDv7; no other ID strategy is introduced.
- [ ] Docs updated; a significant design decision has an ADR in `docs/adr/`.
- [ ] No secrets, credentials or personal data in code, tests, logs or fixtures.
- [ ] Breaking changes are called out above.
