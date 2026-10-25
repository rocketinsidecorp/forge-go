# Contributing to Forge

Thanks for helping. Forge is MIT-licensed and accepts pull requests from
everyone. This guide explains what a change needs before it can merge.

## Ground rules

- Be respectful; see [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
- Security issues go through private reporting, never public issues; see
  [SECURITY.md](SECURITY.md).
- By submitting a contribution you agree that it is licensed under the
  project's [MIT license](LICENSE). Sign your commits off (`git commit -s`)
  to certify the [Developer Certificate of Origin](https://developercertificate.org/).

## Before you start

For anything larger than a small fix, open an issue first so the design can be
agreed before you write code. Significant decisions are recorded as ADRs in
[docs/adr](docs/adr); read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the
module boundaries.

Non-negotiable conventions:

- Core packages import only the standard library and other core packages
  (enforced by `architecture_test.go`); third-party code lives in adapters.
- Identifiers are UUIDv7 everywhere, including in generators.
- Secure defaults: fail closed, never log or echo secrets, validate input.
- No TODOs presented as features, no mocks in production paths.

## Development setup

Requires Go 1.27.3+ and Docker.

```bash
docker run -d --name forge-test-pg -e POSTGRES_USER=forge -e POSTGRES_PASSWORD=forge \
  -p 127.0.0.1:55432:5432 postgres:18-alpine
docker run -d --name forge-test-mysql -e MYSQL_ROOT_PASSWORD=forge -p 127.0.0.1:53306:3306 mysql:8.4
export FORGE_TEST_POSTGRES_URL='postgres://forge:forge@127.0.0.1:55432/postgres?sslmode=disable'
export FORGE_TEST_MYSQL_URL='mysql://root:forge@127.0.0.1:53306/mysql'
```

Database tests skip without these variables; CI sets
`FORGE_TEST_REQUIRE_DATABASE=true` so they fail instead.

## Before opening a pull request

```bash
gofmt -l .
go vet ./...
golangci-lint run ./...
gosec -quiet -tests=false ./...
go test -race -count=1 ./...
scripts/fuzz.sh            # every fuzz target, 10s each, as CI runs it
```

- Add tests for every change; a bug fix needs a test that fails without it.
- Code that parses untrusted input gets a `FuzzXxx` target with a property
  beyond "does not panic"; CI finds and runs it automatically. Commit any
  failing input it finds (`testdata/fuzz/`) with the fix.
- Pin a new GitHub Action by commit SHA with its version in a comment, and a
  container by digest (`supply_chain_test.go` checks;
  [docs/SUPPLY_CHAIN.md](docs/SUPPLY_CHAIN.md)).
- Update the docs your change affects.
- Keep pull requests focused; unrelated refactors belong in separate ones.
- Use [Conventional Commits](https://www.conventionalcommits.org/) messages
  (`feat:`, `fix:`, `docs:`, `test:`, `chore:`).

## Review and merge

Every pull request into `main` needs a maintainer's approval, passing CI and
resolved conversations; approvals are dismissed when new commits are pushed.
CI on pull requests from forks runs after a maintainer approves the workflow
run. Maintainers merge with squash or merge commits.
