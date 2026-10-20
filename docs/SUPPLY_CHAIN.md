# Supply chain: fuzzing, SBOM, signed images

How Forge-Go checks its parsers continuously, describes what it ships, and
signs what it publishes, for the framework repository and for the
applications `forge new` generates
([ADR 0044](adr/0044-continuous-fuzzing-sbom-signed-images.md)).

## What runs where

| When | Workflow | What |
| --- | --- | --- |
| Pull request, push to `main` | `ci.yml` job `fuzz` | Every fuzz target, discovered automatically, 10 s each. |
| Pull request, push to `main` | `ci.yml` job `sbom` | CycloneDX SBOMs of the `forge` and `forge-proxy` binaries (artifact `sbom-binaries`). |
| Pull request, push to `main` | `ci.yml` job `docker` | Builds the example and generated images; checks the generated CI's Syft command on a real image. |
| Pull request, push to `main` | `ci.yml` job `quality` | `govulncheck` (pinned version), lint, race tests. |
| Nightly 03:17 UTC, or by hand | `fuzz.yml` | Each fuzz target on its own runner for 10 minutes (input `fuzztime`), with its corpus cached between nights. |
| Weekly, Monday 04:41 UTC, or by hand | `scorecard.yml` | OpenSSF Scorecard; results stay in the run (artifact `scorecard`, summary). |
| Push of a `v*` tag, only | `release.yml` | Builds, pushes and signs `ghcr.io/rocketinsidecorp/forge-proxy` and `ghcr.io/rocketinsidecorp/forge-example`; binary SBOMs. |
| Weekly | Dependabot | Go modules, GitHub Actions (SHA pins included) and Docker base images. |

Nothing publishes outside a tag push: no pull request, branch, schedule or
manual run can reach `release.yml`, and it refuses a tag that is not a
version (`v1.2.3`, `v1.2.3-rc.1`) or that points at a commit outside the
default branch.

## Fuzzing

`scripts/fuzz.sh` finds every `FuzzXxx` function with
`go test -list '^Fuzz'` and fuzzes each one in turn, so a new target is
fuzzed in CI without editing a list. It continues after a failure and exits
1 if any target failed.

```sh
scripts/fuzz.sh -l                                # list "package target"
scripts/fuzz.sh                                   # every target, 10s each (what CI runs)
scripts/fuzz.sh -t 2m ./i18n ./openapi            # these packages, 2 minutes per target
scripts/fuzz.sh -t 10m -r '^FuzzNewTarget$' ./internal/dbtasks
go test -run '^$' -fuzz '^FuzzDatabaseURL$' -fuzztime 1m ./config   # plain go test
```

A failing input is written to the package's `testdata/fuzz/<target>/`
directory. It is a real bug: fix it and commit the input with the fix, where
it keeps running as a regression test in every `go test ./...`. CI uploads
new failing inputs as the artifact `fuzz-failures` (pull requests) or
`fuzz-failures-<package>-<target>` (nightly); to reproduce one:

```sh
gh run download <run-id> --name fuzz-failures-config-FuzzDatabaseURL --dir /tmp/failure
cp -r /tmp/failure/config/testdata/fuzz/FuzzDatabaseURL config/testdata/fuzz/
go test -run 'FuzzDatabaseURL/' ./config          # runs the seeds, the new input among them
```

Targets and what each one asserts besides "never panics":

| Package | Target | Property |
| --- | --- | --- |
| `auth` | `FuzzParseToken`, `FuzzParsePasswordHashNeverPanics`, `FuzzParseRecoveryTokenNeverPanics`, `FuzzParseMFAChallengeTokenNeverPanics`, `FuzzParseMFABackupCodeNeverPanics` | The parsers never panic; a token parses only in its canonical form. |
| `config` | `FuzzDatabaseURL` | Errors never echo the URL; an accepted URL has no fragment, no `@` after the host, no whitespace or control character and no repeated parameter, one PostgreSQL host, and a verified TLS mode in production. |
| `config` | `FuzzMailURL` | Errors never echo the URL; an accepted SMTP URL has one host, a port in 1-65535, no path and no parameters. |
| `i18n` | `FuzzParseAcceptLanguage` | At most 32 well-formed ranges, sorted by quality; negotiation always returns an available locale. |
| `i18n` | `FuzzCanonicalLocale` | Canonicalization only changes case, is stable, never yields a path or file-name metacharacter, and names its own file. |
| `i18n` | `FuzzLoadCatalog` | A fuzzed translation file either fails to load or every key translates, and interpolated values are HTML-escaped. |
| `internal/dbtasks` | `FuzzNewTarget` | Errors and descriptions never show a URL or password; pgx, the `PG*` variables of `pg_dump`/`psql` and `Local()` agree on host, port and database; the MySQL option file reads back (as `mysys` parses it) to exactly the URL's host, port, user, password and database. |
| `internal/scaffold` | `FuzzNormalizeNeverAcceptsPathEscapes` | An accepted application name or module path holds no path separator, `..`, quote or whitespace. |
| `mail` | `FuzzAddressAndHeaderValidation` | An accepted message re-parses with exactly its one recipient and no injected header. |
| `openapi` | `FuzzValidateResponse` | The contract validator classifies every response as valid, `ErrContract` or `ErrUndocumented`, deterministically. |
| `pagination` | `FuzzDecodeCursor` | Only canonical UUIDv7 cursors decode. |
| `telemetry` | `FuzzParseTraceparent`, `FuzzNormalizeTracestate` | W3C trace context round-trips; normalization is idempotent and bounded. |
| `uuid` | `FuzzParse` | Only canonical UUID text parses. |
| `web` | `FuzzTrustedProxiesResolve` | The resolved client address is the peer or a canonical `X-Forwarded-For` entry. |

## SBOMs

Two formats, from the tools that know each artifact best, neither of them a
Go module dependency:

- **Binaries** (`forge`, `forge-proxy`): CycloneDX JSON from Syft, read from
  the module information the Go toolchain embeds in each binary, so it lists
  exactly the linked modules and the Go version. `scripts/sbom.sh` builds
  the binaries like a release (static, `-trimpath`) and runs Syft from its
  image, pinned by digest, with `--network none`.
- **Published images**: an SPDX SBOM attestation that BuildKit's Syft
  scanner (pinned by digest) attaches to the image index, with SLSA
  provenance (`mode=max`), during the release build.
- **Generated applications**: their CI builds the image and keeps a
  CycloneDX SBOM of it (distroless packages and Go modules) as the `sbom`
  artifact of every run.

```sh
# binaries, locally (Docker, or a local Syft with SYFT=/path/to/syft)
scripts/sbom.sh dist/sbom
jq -r '.components[] | "\(.name) \(.version)"' dist/sbom/forge-proxy.cdx.json

# binaries, from CI or a release
gh run download <run-id> --name sbom-binaries --dir dist/sbom

# a published image: SBOM and provenance of the linux/amd64 variant
docker buildx imagetools inspect ghcr.io/rocketinsidecorp/forge-proxy:v0.1.0 \
  --format '{{ json (index .SBOM "linux/amd64").SPDX }}' | jq -r '.packages[] | "\(.name) \(.versionInfo)"'
docker buildx imagetools inspect ghcr.io/rocketinsidecorp/forge-proxy:v0.1.0 \
  --format '{{ json (index .Provenance "linux/amd64").SLSA }}'

# without Go installed: the module list a binary was built from
go version -m ./forge
```

## Signed images

`release.yml` runs only when a `v*` tag is pushed. Each image is built for
`linux/amd64` and `linux/arm64` (the Dockerfiles cross-compile, no emulator),
pushed to ghcr.io with provenance and SBOM, and its multi-platform index is
signed by digest with Sigstore cosign. Signing is keyless: the job's GitHub
OIDC token gets a short-lived Fulcio certificate that names this repository,
the workflow file and the tag, and the signature is logged in Rekor. There
is no signing key or secret in the repository. The job verifies its own
signature before it ends.

```sh
git tag v0.1.0 && git push origin v0.1.0   # a maintainer cuts a release
```

Verify before you deploy (cosign v3 or later):

```sh
cosign verify ghcr.io/rocketinsidecorp/forge-proxy:v0.1.0 \
  --certificate-identity-regexp '^https://github\.com/rocketinsidecorp/forge-go/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# pin exactly what you verified, by digest, in config/deploy.yml (proxy.image)
docker buildx imagetools inspect ghcr.io/rocketinsidecorp/forge-proxy:v0.1.0 --format '{{ .Manifest.Digest }}'
```

To accept exactly one release, replace the regular expression with
`--certificate-identity https://github.com/rocketinsidecorp/forge-go/.github/workflows/release.yml@refs/tags/v0.1.0`.
A signature made by any other repository, workflow, branch or pull request
does not match either form.

Generated applications get the same workflow as
`.github/workflows/release.yml`, opt-in: it does nothing until the
repository variable `FORGE_RELEASE_IMAGES` is `true`, and then only for `v*`
tags on the default branch. Its README shows the commands.

## Pinning policy

- Every third-party action is pinned to a full commit SHA with the version in
  a comment (`uses: actions/checkout@3d3c42e5… # v7.0.1`), in the
  framework's workflows and in the generated ones. Each SHA was checked
  against the tag in the action's repository (`git ls-remote`, dereferencing
  annotated tags).
- Every container a workflow runs (Syft, BuildKit's scanner, CI database
  services) and the base images of the published Dockerfiles are pinned by
  the digest of their multi-architecture index.
- New pins use releases at least a week old, so a compromised release has
  time to be noticed and withdrawn before Forge runs it.
- `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0` names a version, which
  the Go checksum database pins; the vulnerability database itself is read
  live.
- `supply_chain_test.go` fails `go test ./...` when an action is not pinned
  by SHA, a workflow container or service image is not pinned by digest,
  copies of the Syft pins differ, a checkout keeps its credentials, a
  workflow lacks a top-level `permissions:`, or a release workflow gains a
  trigger other than `v*` tags. It reads `.yml` and `.yaml` workflows alike
  and refuses:
  - a `uses:` that is not `owner/repo@<40-hex commit> # vX.Y.Z`, in any
    spelling (`- uses:`, `"uses":`, `uses :`, quoted value);
  - a `container: <image>` shorthand or service `image:` without a digest
    (a bare `container:` mapping opener and a `- image:` matrix entry are
    not images);
  - every `<scope>: write` permission, except `id-token` and `packages` in
    `release.yml`, and `write-all` everywhere;
  - `secrets[`, `secrets)`, `secrets }`, `secrets}` and `secrets: inherit`,
    which hand secrets on wholesale or by computed name.
- Permissions are read-only by default (`contents: read`, or `{}` in
  `release.yml` and `scorecard.yml`); only the release image job holds
  `packages: write` and `id-token: write`.

To update a pin by hand:

```sh
git ls-remote https://github.com/actions/checkout 'refs/tags/v7.0.1' 'refs/tags/v7.0.1^{}'   # the ^{} line, if any, is the commit
docker buildx imagetools inspect ghcr.io/anchore/syft:v1.54.1 --format '{{ .Manifest.Digest }}'
```

Dependabot proposes new SHAs for actions and digests for Dockerfile base
images. It does not update the digests inside workflow `run:` steps (Syft),
`sbom: generator=` (BuildKit's scanner) or CI service images; refresh those
by hand, all copies at once (the test requires them to agree).

## Residual risks

- `ossf/scorecard-action` runs its image by tag (its `action.yaml` names
  `ghcr.io/ossf/scorecard-action:v2.5.0`); the job is read-only and
  publishes nothing.
- The `sigstore/cosign-installer` pin chooses the cosign version (v3.0.6) and
  verifies its checksum; Syft inside BuildKit's scanner image is whatever that
  digest holds.
- The generated application's Dockerfile and compose database images are
  referenced by tag, not digest; their provenance attestation records the
  digests that were used.
- A maintainer who can push `v*` tags can publish a signed image from any
  commit: the default-branch guard runs from the tagged commit's own
  `release.yml`, so a tag on a commit that removed it skips it. Protect `v*`
  tags with a repository ruleset, and when verifying an image also check that
  the certificate's source commit is on `main`.
- Signatures prove which workflow built an image from which commit, not that
  the commit is benign: review still guards `main`.
- Binary SBOMs are workflow artifacts (90 days for releases); they are not
  attached to a GitHub release.
