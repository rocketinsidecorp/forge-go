# ADR 0044: Continuous fuzzing, SBOMs and signed images

- **Status:** Accepted
- **Date:** 2026-10-20

The guide is [docs/SUPPLY_CHAIN.md](../SUPPLY_CHAIN.md). The decision is
language-neutral; the [Forge-Rust port](#forge-rust-port) section maps it.

## Context

Forge parses hostile or operator-supplied text in many places: tokens,
cursors, trace headers, `Accept-Language`, translation files, database and
mail URLs, response bodies checked against an OpenAPI document. Fuzz targets
existed for some of them, but CI ran two, by name, for 30 seconds; new
targets were never run unless someone edited the workflow. The i18n parsers,
the OpenAPI validator, the configuration URL rules (`rejectAmbiguous`) and
the `forge db` URL checks had none, although the last two decide which
server credentials are sent to.

Nothing described what the `forge` CLI, forge-proxy or a generated image
contain, and nothing proved where an image came from: the threat model
listed "SBOM and signed images" and "pin actions by commit SHA" as Planned,
operators had to build forge-proxy themselves, and workflow actions were
referenced by mutable tags. Constraints: no new Go module dependency
(ADR 0006), no long-lived signing key or other secret in the repository, and
nothing may publish except a deliberate release.

## Decision

1. **Every fuzz target runs, discovered, not listed.** `scripts/fuzz.sh`
   finds `FuzzXxx` functions with `go test -list '^Fuzz'` and fuzzes each
   for a fixed time (10 s per target on every pull request). The nightly
   `fuzz.yml` gives each target its own runner for 10 minutes, caches each
   target's corpus between nights, and uploads new failing inputs from
   `testdata/fuzz` as artifacts. A failing input is a bug: it is fixed and
   committed as a seed.
2. **New targets for the high-risk parsers, with properties, not just "no
   panic":** `FuzzParseAcceptLanguage`, `FuzzCanonicalLocale`,
   `FuzzLoadCatalog` (i18n), `FuzzValidateResponse` (openapi),
   `FuzzDatabaseURL`, `FuzzMailURL` (config: errors never echo the URL, an
   accepted URL is unambiguous) and `FuzzNewTarget` (internal/dbtasks: pgx,
   the libpq tools' `PG*` variables, `Local()` and the MySQL option file, read
   back as the MySQL client parses it, all agree on the server and
   database).
3. **SBOMs from the tools that know each artifact, outside the module.**
   Binaries: Syft reads the module information the Go toolchain embeds and
   writes CycloneDX (`scripts/sbom.sh`, CI job `sbom`). Published images:
   BuildKit attaches an SPDX SBOM and SLSA provenance (`mode=max`) to the
   image index. Generated applications: CI keeps a CycloneDX SBOM of the
   image as an artifact. Syft and BuildKit's scanner run from images pinned
   by digest, Syft with `--network none`.
4. **Signed images from tags only.** `release.yml` triggers on `v*` tag
   pushes and nothing else, refuses a tag that is not a version or not on the
   default branch, builds forge-proxy and the example for `linux/amd64` and
   `linux/arm64` (the Dockerfiles cross-compile), pushes them to ghcr.io and
   signs each index by digest with cosign, keyless, through the job's GitHub
   OIDC token. Users verify with `cosign verify` pinned to this repository's
   workflow identity and GitHub's issuer. Only that job holds
   `packages: write` and `id-token: write`; every workflow defaults to
   read-only. Generated applications get the same workflow, opt-in through
   the repository variable `FORGE_RELEASE_IMAGES`.
5. **Pin by SHA and digest, enforce with a test.** Actions are pinned to a
   verified commit SHA with the version in a comment, workflow containers
   and published base images by index digest, `govulncheck` by version;
   new pins use releases at least a week old. `supply_chain_test.go` fails
   the build when a pin, a permission rule, the tag-only trigger or
   `persist-credentials: false` is lost, and when copies of the Syft pins
   disagree. Dependabot updates actions and Dockerfile digests; OpenSSF
   Scorecard runs weekly without publishing.

## Consequences

- A new `FuzzXxx` function is fuzzed on the next pull request and nightly
  without workflow changes; every PR spends roughly four minutes fuzzing.
- `FuzzNewTarget` found, in its first long run, that `forge db` accepted any
  digits as a port: the `mysql` and `mysqldump` clients would read `99999`
  from the option file and dial it truncated to 16 bits (port 34463 on the
  same host), and `01` reached the tools and pgx spelled differently. Ports
  must now be 1-65535 without leading zeros; the input is kept as a seed and
  `TestNewTargetRefusesUnsafeOrMissingConfiguration` covers both URLs. The
  other new targets found nothing; their properties now guard against
  regressions in URL ambiguity, secret echoing and HTML escaping.
- Operators can pull a signed, multi-architecture forge-proxy instead of
  building it, and pin the digest they verified.
- Pins must be refreshed deliberately: Dependabot does not see the digests in
  `run:` steps, `sbom: generator=` or service images.
- The signature proves which workflow built an image from which commit, not
  that the commit is benign; whoever can push a `v*` tag on the default
  branch can publish. A tag ruleset is recommended.

## Follow-ups

- Attach binary SBOMs to a GitHub release (needs `contents: write` in a
  separate job) instead of keeping them as 90-day artifacts.
- Pin the generated application's Dockerfile and compose database images by
  digest, with Dependabot keeping them current.
- Publish signed `forge` CLI binaries with SLSA provenance.

## Forge-Rust port

Done in Forge-Rust (forge-rust#34, closed; Forge-Rust ADR 0043 and the
Rails parity matrix, row `fuzz-sbom-images`). Same contract, Rust tooling:

- **Fuzzing:** `cargo fuzz` targets (or `bolero`) for the same parsers, with
  the same properties, discovered by `cargo fuzz list`; the same PR budget,
  nightly workflow, corpus cache and failing-input artifacts.
- **SBOM:** CycloneDX for the `forge` and `forge-proxy` binaries
  (`cargo auditable` embeds the dependency list, Syft reads it, no new crate
  in the build graph), the same BuildKit SPDX SBOM and provenance for images,
  and the same `sbom` artifact in generated CI.
- **Signing:** the same `release.yml` contract (tag-only, default branch,
  `ghcr.io/<owner>/forge-proxy`, keyless cosign, least privilege), the same
  opt-in `FORGE_RELEASE_IMAGES` variable for generated applications, and the
  same verification command with forge-rust's workflow identity.
- **Pinning:** the same SHA and digest rules, enforced by a test.
