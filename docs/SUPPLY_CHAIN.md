# Supply chain: fuzzing, SBOM, signed images and binaries

How Forge-Go checks its parsers continuously, describes what it ships, and
signs what it publishes (container images and the `forge` and `forge-proxy`
binaries), for the framework repository and for the
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
| Push of a `v*` tag, only | `release.yml` | Builds, pushes and signs `ghcr.io/rocketinsidecorp/forge-proxy` and `ghcr.io/rocketinsidecorp/forge-example`; builds the `forge` and `forge-proxy` archives for 6 platforms, with SBOMs, `checksums.txt` signed with cosign and one build provenance attestation covering every file; drafts the GitHub release. |
| Weekly | Dependabot | Go modules, GitHub Actions (SHA pins included) and Docker base images. |

Nothing publishes outside a tag push, and even then the GitHub release is a
draft a maintainer verifies and publishes ([RELEASING.md](RELEASING.md)): no pull request, branch, schedule or
manual run can reach `release.yml`, and it refuses a tag that is not a
version (`v1.2.3`, `v1.2.3-rc.1`), whose major version does not match the
module path of `go.mod` (from v2 on the path must end in `/vN`, or `go get`
refuses the tag), or that points at a commit outside the default branch.

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
  image, pinned by digest, with `--network none`. A release carries them as
  `forge_<version>_linux_<arch>.cdx.json` and
  `forge-proxy_<version>_linux_<arch>.cdx.json`, listed in `checksums.txt`
  and attested like the archives.
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

# binaries, from CI, or from a release (verify it first, below)
gh run download <run-id> --name sbom-binaries --dir dist/sbom
gh release download v0.1.0 --repo rocketinsidecorp/forge-go --pattern '*.cdx.json'

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

## Signed release binaries

`release.yml` also builds the command-line programs. The `binaries` job runs
`scripts/release-build.sh` (plain `go build`, no release tool) for `forge` and
`forge-proxy` on `linux`, `darwin` and `windows`, each on `amd64` and `arm64`:
twelve archives named `<command>_<version>_<os>_<arch>.tar.gz` (`.zip` on
Windows, the version is the tag, `v0.1.0`), each holding a directory of the
same name with the binary, `LICENSE` and `README.md`.

- **Static, no cgo.** `CGO_ENABLED=0`: SQLite is the pure-Go
  `modernc.org/sqlite`, so one binary runs on any libc (or none) of its
  platform and no C toolchain takes part in the build.
- **Reproducible.** `-trimpath`, the tag, the commit and the commit's time
  stamped through `internal/version` (and the toolchain's own VCS stamp,
  `-buildvcs=true`, read by `go version -m`), sorted tar entries, the commit
  time as every modification time, no owner, gzip without name or time. The
  same commit with the same Go toolchain (`go.mod`) gives the same binaries,
  and on one machine the same archives, byte for byte
  (`TestReleaseBuildProducesReproducibleArchivesForTheHost` builds twice and
  compares); the deflate stream is the system `gzip`'s, so another machine's
  archive may differ while the binaries inside do not.
- **Checksums.** `scripts/release-checksums.sh` writes `checksums.txt`, the
  SHA-256 of every archive and of the four binary SBOMs, in `sha256sum`
  format.
- **Signature.** The job signs `checksums.txt` keyless with cosign
  (`cosign sign-blob --bundle checksums.txt.sigstore.json`): the certificate
  in the bundle names the workflow file and the tag, the signature is logged
  in Rekor, and no key exists. The job verifies its own signature.
- **Provenance.** `actions/attest-build-provenance` makes one attestation
  whose subjects are every file `checksums.txt` lists (SLSA build provenance:
  repository, workflow, tag, commit),
  stored by GitHub and kept in the release as `provenance.jsonl` for offline
  checks.
- **Draft release.** The `draft` job downloads what `binaries` built and runs
  `scripts/release-draft.sh`: `gh release create --draft --verify-tag
  --generate-notes` with the archives, `checksums.txt`,
  `checksums.txt.sigstore.json`, the SBOMs and `provenance.jsonl`. It never
  publishes; a maintainer publishes the draft after verifying it
  ([RELEASING.md](RELEASING.md)).

Least privilege, per job (`TestReleaseJobsHoldOnlyTheirPermissions`, a table
that fails on any difference, new job included): top-level `permissions: {}`;
`binaries` holds `contents: read`, `id-token: write` (keyless signing) and
`attestations: write`, and nothing that can touch a release; `draft` holds only
`contents: write` and signs nothing; `images` keeps `packages: write` and
`id-token: write`; `verify` and `sbom` are read-only. No Forge installer
script exists (`TestNoDocumentTellsToPipeADownloadIntoAShell` refuses a
download piped into a shell in any Markdown document, and an `install`,
`install.sh` or `get.sh` at the top level or in `scripts/`): you download an
archive and verify it.

## Verify a release

Needs [cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
v3 or later and, for the attestation, the GitHub CLI (`gh`) 2.68 or later (for `--signer-workflow` and `--source-ref`).
Start from the release page or:

```sh
VERSION=v0.1.0 OS=linux ARCH=amd64        # OS: linux, darwin, windows; ARCH: amd64, arm64
gh release download "$VERSION" --repo rocketinsidecorp/forge-go \
  --pattern "forge_${VERSION}_${OS}_${ARCH}.*" --pattern 'checksums.txt*' --pattern provenance.jsonl
# (or curl -sSLO https://github.com/rocketinsidecorp/forge-go/releases/download/$VERSION/<file>)

# 1. The signature: checksums.txt comes from this repository's release workflow at a v* tag
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/rocketinsidecorp/forge-go/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

# 2. The archive is the one the signed checksums list
sha256sum -c --ignore-missing checksums.txt
#    macOS has shasum, not --ignore-missing:
grep " forge_${VERSION}_${OS}_${ARCH}" checksums.txt | shasum -a 256 -c

# 3. GitHub's provenance: --repo alone checks only that the attestation names this repository;
#    --signer-workflow and --source-ref also pin the workflow file and the tag it ran for
gh attestation verify "forge_${VERSION}_${OS}_${ARCH}.tar.gz" --repo rocketinsidecorp/forge-go \
  --signer-workflow rocketinsidecorp/forge-go/.github/workflows/release.yml --source-ref "refs/tags/$VERSION"
#    offline, from the bundle kept in the release:
gh attestation verify "forge_${VERSION}_${OS}_${ARCH}.tar.gz" --repo rocketinsidecorp/forge-go \
  --signer-workflow rocketinsidecorp/forge-go/.github/workflows/release.yml --source-ref "refs/tags/$VERSION" --bundle provenance.jsonl

# 4. The binary says what it is, and the commit is the tag's
tar -xzf "forge_${VERSION}_${OS}_${ARCH}.tar.gz"
"./forge_${VERSION}_${OS}_${ARCH}/forge" version         # forge v0.1.0 (3f2a9c1d07be, 2026-10-27T12:00:00Z, go1.27.3 linux/amd64)
git ls-remote https://github.com/rocketinsidecorp/forge-go "refs/tags/$VERSION" "refs/tags/$VERSION^{}"   # starts with the commit above
go version -m "./forge_${VERSION}_${OS}_${ARCH}/forge"   # modules linked, vcs.revision, -trimpath, CGO_ENABLED=0
```

Step 1 accepts any `v*` tag built by that workflow. To accept exactly one
release, replace `--certificate-identity-regexp` with
`--certificate-identity https://github.com/rocketinsidecorp/forge-go/.github/workflows/release.yml@refs/tags/v0.1.0`;
the attestation command above already pins the tag with `--source-ref`; drop
that flag to accept any release of this repository.
A checksum file or a bundle from any other repository, workflow, branch or
pull request fails step 1. `forge-proxy_<version>_<os>_<arch>` and the SBOMs
(`*.cdx.json`, in `checksums.txt`) verify the same way; the container images
have their own signature (above).

To rebuild and compare (the Go version of the `go` line of `go.mod`, which
`forge version` also shows; compare the binaries, which do not depend on the
`gzip` that compressed them):

```sh
git clone --branch "$VERSION" https://github.com/rocketinsidecorp/forge-go && cd forge-go
TARGETS="${OS}/${ARCH}" scripts/release-build.sh "$VERSION" /tmp/rebuilt
mkdir /tmp/rebuilt/x && tar -xzf "/tmp/rebuilt/forge_${VERSION}_${OS}_${ARCH}.tar.gz" -C /tmp/rebuilt/x
sha256sum "/tmp/rebuilt/x/forge_${VERSION}_${OS}_${ARCH}/forge" "./forge_${VERSION}_${OS}_${ARCH}/forge"   # the two lines agree
```

## Pinning policy

- Every third-party action is pinned to a full commit SHA with the version in
  a comment (`uses: actions/checkout@3d3c42e5… # v7.0.1`), in the
  framework's workflows and in the generated ones. Each SHA was checked
  against the tag in the action's repository (`git ls-remote`, dereferencing
  annotated tags).
- Every container a workflow runs (Syft, BuildKit's scanner, CI database
  services) and the base images of the published Dockerfiles are pinned by
  the digest of their multi-architecture index.
- New pins use releases at least two weeks old (`actions/attest-build-provenance`
  v4.2.2 of 2026-08-06, `actions/download-artifact` v8.0.2 of 2026-10-07), so a compromised release has
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
    spelling (`- uses:`, `"uses":`, `uses :`, quoted value, flow style
    `- { uses: ... }`, an anchor or alias), and a `docker://` action without
    a digest; a local `./` action's `action.yml` is checked like a workflow;
  - an action outside the allowlist (`allowedActions` in
    `supply_chain_rules_test.go`, compared without case:
    `actions/attest-build-provenance`, `actions/checkout`, `actions/cache`,
    `actions/download-artifact`, `actions/setup-go`, `actions/upload-artifact`,
    `docker/setup-buildx-action`, `docker/login-action`,
    `docker/build-push-action`, `golangci/golangci-lint-action`,
    `ossf/scorecard-action`, `sigstore/cosign-installer`, plus local `./`
    actions), however it is pinned, and every `docker://` action. To add
    one, review its code and what it downloads, pin it by SHA with its
    version comment, and add it to `allowedActions` and to the pins below
    in the same reviewed change; an allowlisted action that is no longer
    used fails the test too; a sub-path cannot leave the repository (no
    empty, `.` or `..` segment);
  - anything outside the simple YAML subset gated files are written in
    (workflows, local actions, generated-app templates): a carriage return
    not followed by a line feed, U+0085, U+2028 or U+2029 (line breaks a
    parser sees and a line reader does not), a byte-order mark past the
    start; outside block scalars (`run: |`) and plain scalars continued on
    deeper lines, an anchor (`&`), alias (`*`), tag (`!`, `!!`), explicit
    key (`?`) or merge key (`<<`) where any key or value starts
    (`${{ !cancelled() }}`, quoted strings and `run: |` text stay allowed);
    a flow collection or quoted scalar that does not close on its line; a
    backslash in any double-quoted scalar, key or value (an escape spells
    text the rules would not read: `"u\x73es"` is `uses`, `"g\x6f env -w"`
    is `go env -w`; plain and single-quoted scalars and block text have no
    escapes, so the text the rules read is the text YAML yields). Write a multi-line expression as a
    `>-` block scalar. Every gated file is also parsed as YAML
    (`go.yaml.in/yaml/v3`): the parser and the line scanner must read the
    same `uses` keys, the same values on the same lines (block-scalar text
    is not scanned, so a decoy there cannot balance a hidden key); the
    checkout (`with: persist-credentials: false` in the step itself) and
    `permissions` rules also read the parsed tree, not the text; no mapping
    may repeat a key, compared without case (`persist-credentials` and
    `Persist-Credentials`, a second `uses`), since readers that keep the
    first and the last value would disagree;
  - a variable that redirects where the go command fetches modules,
    checksums or toolchains, or what it reads and runs (`GOFLAGS`,
    `GOPROXY`, `GONOSUMDB`, `GONOSUMCHECK`, `GOSUMDB`, `GOINSECURE`,
    `GOPRIVATE`, `GONOPROXY`, `GOENV`, `GOCACHEPROG`,
    `GOMODCACHE`, `GOPATH`, `XDG_CONFIG_HOME` (it moves the go env file),
    `ACTIONS_ALLOW_UNSECURE_COMMANDS`, and `GOTOOLCHAIN` unless set to
    `local` or one exact `goX.Y.Z`), in any case, in an `env:` block at any
    level or in `run` text (a second check under the closed allowlists of
    the Threat model section); `HOME` and `GOROOT` where they are set (an
    `env:` key or a `HOME=`/`HOME+=` assignment: HOME moves
    `~/.config/go/env`), reading `$HOME` or `go env GOROOT` stays allowed; any `$GITHUB_ENV` write, whatever it sets (a variable for later
    steps goes in an `env:` block, where the gate reads its name); and the
    deprecated `::set-env` and `::add-path` workflow commands. No go env
    file (`go.env`, or `env` in a directory named `go`) exists anywhere in
    the repository or in a generated application
    (`TestNoGoEnvFileExistsInTheRepository`);
  - in a gated template (`internal/scaffold/templates/ci.yml.tmpl`), any
    directive but `{{.Field}}`, `{{index . N}}` and `if`/`else`/`range`/`end`
    over fields (`and`, `or`, `not`): no string literals, variables,
    `print*`, `with`, `template` or other functions, so a directive cannot
    render a step the gate never reads. The files `forge new` writes under
    `.github` are also generated for every database (and none), two names
    and every locale with the real template data; every file found there is
    gated as written (the workflow rules on `.github/workflows/*`), and the
    set of files must equal the expected list, so a new generated workflow
    cannot ship ungated; their database service images are pinned by digest to the
    framework CI's pins (`internal/scaffold/database.go`). The file
    templates (`internal/scaffold/templates/*.tmpl`) share one template
    set, where a later file's `{{define "ci.yml.tmpl"}}` would replace the
    gated template, so `Generate` refuses `{{define}}` and `{{block}}` in
    them and a test checks that each name in the set holds the tree its file
    parses to on its own;
  - a checkout step, in any spelling of `actions/checkout`, that does not
    set `persist-credentials: false` itself (checked per step, not counted);
  - a `container: <image>` shorthand, a job's `container.image` or a
    service `image:` without a digest, in block or flow style (an `image:`
    action input under `with:` and a `- image:` matrix entry are not
    containers);
  - every `<scope>: write` permission, in block or flow style
    (`permissions: { contents: write }`), except `id-token` and `packages`
    in a release workflow and, in the framework's own `release.yml` only,
    `attestations` and `contents` (and there each job holds exactly its
    listed permissions, `TestReleaseJobsHoldOnlyTheirPermissions`), and
    `write-all` everywhere;
  - `pull_request_target`, which runs fork code with the repository's
    secrets;
  - `secrets[`, `toJSON(secrets)`, `join(secrets, ...)`, `secrets }` and
    `secrets: inherit` in any case or spacing, which hand secrets on
    wholesale or by computed name;
  - any `secrets.NAME` read other than `secrets.GITHUB_TOKEN` in
    `release.yml`;
  - a `run:` script that contains `${{ }}` (in any workflow or generated
    file): GitHub pastes the expression's value into the shell text, so a
    tag, branch name or commit message would run as code. Scripts read values
    through `env:` (`env: NAME: ${{ github.ref_name }}`, then `"$NAME"`), and
    the list of safe contexts is empty
    (`TestRunScriptsRefuseInterpolatedExpressions`);
  - a document that installs a literal version
    (`go install .../cmd/forge@v0.1.0`): documents use `@latest` or a
    `vX.Y.Z` placeholder, since no tag may be assumed to exist
    (`TestDocsNeverInstallAVersionThatIsNotATag`);
  - a release workflow without its signing and verification steps
    (`releaseViolations`; for the framework's, `binaryReleaseViolations`, which
    reads code, never comments: `cache: false` on every setup-go step, the
    build, checksum and draft scripts, `cosign sign-blob` and `verify-blob`,
    the provenance attestation of `checksums.txt`, and a draft script that
    creates a draft, verifies the tag, and never edits, uploads to or
    un-drafts a release).
- Permissions are read-only by default (`contents: read`, or `{}` in
  `release.yml` and `scorecard.yml`); in the release workflow only the image
  job holds `packages: write` and `id-token: write`, only the binary job
  `id-token: write` and `attestations: write`, and only the draft job
  `contents: write`.

To update a pin by hand:

```sh
git ls-remote https://github.com/actions/checkout 'refs/tags/v7.0.1' 'refs/tags/v7.0.1^{}'   # the ^{} line, if any, is the commit
docker buildx imagetools inspect ghcr.io/anchore/syft:v1.54.1 --format '{{ .Manifest.Digest }}'
```

Dependabot proposes new SHAs for actions and digests for Dockerfile base
images. It does not update the digests inside workflow `run:` steps (Syft),
`sbom: generator=` (BuildKit's scanner) or CI service images; refresh those
by hand, all copies at once (the test requires them to agree).
The service images of the generated application's CI are pinned to the same
digests as its compose file and the framework's CI
(`internal/scaffold/database.go`), and Dependabot does not update CI service
images: bump them together, by hand, in one change.

## TypeScript: esbuild and tsc

Generated applications use TypeScript by default
([ADR 0049](adr/0049-typescript-by-default.md)). Two third-party programs are
involved, both pinned to one exact version and both optional for running an
application:

| | esbuild | tsc (TypeScript) |
| --- | --- | --- |
| Role | strips types from `app/javascript/**/*.ts`; linked into the `forge` command (`github.com/evanw/esbuild/pkg/api`, `Transform` only) | checks types (`tsc --noEmit`); never part of any binary |
| Version | `v0.28.2` in `go.mod` and `go.sum` (`frontend.EsbuildVersion`; a test compares them), released 2026-08-08 | `6.0.3` (`frontend.TypeScriptVersion`), released 2026-04-16 |
| Licence | MIT | Apache-2.0 |
| Reaches | the `forge` binary only; no application, no `forge-proxy`, no core package (`TestEsbuildIsLinkedOnlyByTheForgeCommandLine`) | the developer's machine and the two CI jobs that run `tsc --noEmit` |
| Dependencies | none of its own: one `go.mod` edge to `golang.org/x/sys`, which already resolves to Forge's `v0.49.0`; pure Go, no cgo, no `os/exec`, no install or build scripts run by `go` | none: the one pure-JavaScript package, no install scripts. 6.0.x rather than 7.x on purpose: 7.0 is a native compiler that installs a platform binary package besides `typescript` |
| Installed by | `go install` / `go build` (checksummed in `go.sum`, `go mod verify`) | `npm install --global --ignore-scripts typescript@6.0.3`, the one npm package any workflow installs (`TestTypeScriptCheckerIsTheOnlyNpmPackageAndIsPinned`) |

Updating either is a reviewed change, never an automatic one:

```sh
# esbuild: pick a release at least two weeks old, read its changelog, then
go get github.com/evanw/esbuild@v0.28.3 && go mod tidy && go mod verify
go list -deps ./cmd/forge | grep evanw              # still only pkg/api and its internal packages
go list -deps -f '{{join .Imports "\n"}}' github.com/evanw/esbuild/pkg/api | sort -u   # no new os/exec, plugin or network import
# set frontend.EsbuildVersion to the new version, regenerate dist of the templates' tests, update the ADR
go test ./internal/frontend ./internal/scaffold .

# tsc: change frontend.TypeScriptVersion and the same version in .github/workflows/ci.yml and
# internal/scaffold/templates/ci.yml.tmpl together (the test requires both), then
go test -count=1 -run 'TypeScript' . ./internal/cli
```

Dependabot proposes esbuild through the Go module updates it already opens;
treat that pull request like any other new-code review (diff the module, look
for new imports of `os/exec`, `net`, `plugin` or `unsafe` in the packages
`pkg/api` links). It cannot update the `tsc` version, which sits in workflow
`run:` text. The generated application's own `go.sum` lists esbuild because it
sits in Forge's module graph, but its `go.mod` and its binary do not.

## Threat model and residual risk

Engineering decisions D6 and D8 (2026-10-25). The supply-chain gate is
defense in depth against unreviewed drift and casual injection in the
workflows, local actions and generated-app files. It is not a shell sandbox:
review of every change is the primary control. Since D8 the gate's core is a
set of closed allowlists in `supply_chain_allowlist_test.go`, so it converges
instead of chasing new redirects; growing a list is a reviewed change to that
file.

- Variables (`allowedVariables`): a gated file sets only listed names (the
  release workflow's `GH_TOKEN`, `GH_REPO` and `BUNDLE` take one exact
  expression each), in an
  `env:` key at any level (workflow, job, step, container, service; plain,
  quoted or flow) or in `run` text (`NAME=`, `NAME+=`, `NAME[i]=`, `export`,
  `env`, `sudo`, `declare`, `typeset`, `readonly`, `local`, `read`,
  `mapfile`, `getopts`, `let`, `printf -v`, `for`/`select`/`coproc NAME`,
  `${NAME:=...}`, `{NAME}>`, arithmetic). `run` text is read in every
  reading bash could give it (`shellLines`): physical lines, lines joined
  as bash joins a backslash-newline continuation, joined with a space, each
  with and without comments, so a target split from its `=` by a
  continuation is still read. Names a tool reads
  (`GOARCH`, `SYFT_CHECK_FOR_APP_UPDATE`, `FORGE_TEST_REQUIRE_*`) take one
  exact value. `HOME`, `PATH`, `BASH_ENV`, `LD_PRELOAD`, `CC`, `CXX`,
  `CGO_*`, `GOWORK`, `GOFLAGS`, `GOROOT` and every other unlisted name are
  refused as assignment targets; reading `$HOME` or `go env GOROOT` stays
  allowed. An `env:` or `with:` that is a whole expression is refused, and
  `shell:` is bash only.
- Runner files (`runnerFileLines`): `$GITHUB_ENV`, `$GITHUB_PATH` and
  `$GITHUB_OUTPUT` are written only on the listed lines (the PostgreSQL
  client tools on `PATH`, the fuzz matrix and corpus outputs, the release
  image names), each a complete logical line: a listed line continued from
  or into another is refused, and so is every other line that names a
  runner file, in any reading, a heredoc or a `$( )` included; `::set-env`, `::add-path` and
  `ACTIONS_ALLOW_UNSECURE_COMMANDS` are refused.
- Expressions (`expressionLines`): `format`, `fromJSON`, `join` and `toJSON`
  appear only on the listed line (the fuzz matrix).
- The go command: no `go env -w`, no `-toolexec`, `-exec`, `-modfile` or
  `-overlay`, and no `-gcflags`/`-ldflags`/`-asmflags` value that passes an
  option outside `allowedGoFlags` (empty today).
- Action inputs: allowlisted actions get exact versions (`go-version`
  X.Y.Z or `go-version-file: go.mod`, which every workflow and the generated
  CI now use; golangci-lint `version` vX.Y.Z; `cosign-release` and
  setup-buildx `version` vX.Y.Z when set), no `check-latest`, `driver-opts`
  images by digest, and no `latest` anywhere.
- Job containers and services (`containerKeys`,
  `allowedContainerOptions`): only `image`, `env` (under
  `allowedVariables`), numeric `ports` and `options`; no `volumes`,
  `credentials`, `entrypoint` or `command`. `options:` is exactly one of the
  listed database health checks (framework and generated CI), and `-e`,
  `--env`, `--env-file`, `-v`, `--volume`, `--mount`, `--entrypoint`,
  `--privileged`, `--network` and `--cap-add` are refused in it.
- Tool configuration files (`toolConfigurationFiles`): none anywhere in the
  repository (templates included) or in any generated application, for
  every database, name and locale: `go.work`, `go.work.sum`, `go.env`,
  `go/env`, `.cargo/config`, `.cargo/config.toml`, a `rust-toolchain` file
  with a `path` (Forge-Rust lists its rust-toolchain files' exact content
  instead; a Go project has none, so this backstop stays), `.bashrc`, `.profile`, `.bash_profile`, `.envrc`.
- The YAML-level rules still apply underneath: the simple subset (no
  escapes, anchors, tags, hidden line breaks), the parser cross-check,
  duplicate keys, template directives, the rendered gate, and the denylist
  of redirecting variables as a second, independent check.

Accepted residual risk, left to review: names the shell builds at run time
(`eval`, a `printf`-built or `$(...)`-built name), the content of the scripts
the workflows call (`scripts/*.sh`, the release scripts included, which are
only checked for the redirecting variables and pins above), and what an allowlisted action does
internally with its pinned inputs. Reviews of the gate judge it against this
model: a bypass inside the lists' scope is a defect to fix in the gate.

## Residual risks

- `ossf/scorecard-action` runs its image by tag (its `action.yaml` names
  `ghcr.io/ossf/scorecard-action:v2.5.0`); the job is read-only and
  publishes nothing.
- The `sigstore/cosign-installer` pin chooses the cosign version (v3.0.6) and
  verifies its checksum; Syft inside BuildKit's scanner image is whatever that
  digest holds.
- The generated application's Dockerfile base images and compose database
  images are pinned by digest, the same digests as Forge's own Dockerfiles
  and CI (`TestDockerfilesPinBaseImagesByDigest`,
  `TestGeneratedWorkflowsPassTheGate`); they age until the application's
  Dependabot (`docker`, `docker-compose`) or a maintainer moves them, and
  the image's provenance attestation records them.
- A maintainer who can push `v*` tags can publish a signed image from any
  commit: the default-branch guard runs from the tagged commit's own
  `release.yml`, so a tag on a commit that removed it skips it. Protect `v*`
  tags with a repository ruleset, and when verifying an image also check that
  the certificate's source commit is on `main`.
- `npm install` of `tsc` fetches from the npm registry by exact version and
  integrity-checks the tarball, but there is no lockfile and the registry is
  trusted for that release; `--ignore-scripts` means no package code runs at
  install. It affects type checking only, never what is built or shipped.
- Signatures prove which workflow built an image from which commit, not that
  the commit is benign: review still guards `main`.
- The draft job runs `scripts/release-draft.sh` from the tagged commit with
  `contents: write`, and a maintainer who can push `v*` tags chooses that
  commit: protect `v*` tags with a ruleset, and read the diff of
  `.github/workflows/release.yml` and `scripts/release-*.sh` before tagging.
- Build provenance attestations need a public repository (or GitHub
  Enterprise Cloud for a private one); on a plan without them the `binaries`
  job fails at the attestation step, before anything is drafted. The cosign
  signature on `checksums.txt` is logged in the public Rekor transparency log
  either way, as the image signatures already are.
- `checksums.txt` and the provenance prove which workflow built which bytes,
  not that the source is benign, and the release archives are not signed one
  by one: the signed checksums vouch for them. Windows and macOS binaries are
  not code-signed or notarized by an OS vendor: Gatekeeper and SmartScreen may
  warn, and the verification above is how a user gains trust instead.
- `scripts/sbom.sh` builds the SBOM's binaries separately, without the version
  stamp, so an SBOM describes the modules a release links (they are the same)
  but is not a hash of the archive's binary.
