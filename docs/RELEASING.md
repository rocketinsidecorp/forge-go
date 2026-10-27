# Releasing

How a maintainer cuts a Forge-Go release. A `v*` tag builds everything and
leaves a **draft** GitHub release; nothing is published until you verify the
draft and publish it by hand (owner decision D24: signed binaries, checksums,
cosign and attestations, no pipe-to-shell installer). What the workflow does
and why it can be trusted: [SUPPLY_CHAIN.md](SUPPLY_CHAIN.md#signed-release-binaries).

A release is the tag `vMAJOR.MINOR.PATCH` (or `vMAJOR.MINOR.PATCH-rc.1`, which
becomes a pre-release). It is also the module version `go install ...@vX.Y.Z`
resolves, so a tag is never moved or reused: a bad release is superseded by the
next patch.

## Before tagging

```sh
git switch main && git pull --ff-only
git status --short                                   # clean
gofmt -l . ; go vet ./... ; golangci-lint run
go test -race -count=1 ./...                         # CI's gates, green on this commit
go test -count=1 -run 'TestRelease|SupplyChain|Workflows' .
scripts/release-build.sh v0.1.0-rc.0 /tmp/release-dry   # optional: the archives, on your machine
```

- The commit is on `main` and its CI run has finished green (`release.yml`
  refuses a tag whose commit is not on the default branch, or whose `ci.yml`
  push run has not finished with the jobs `quality`, `coverage`, `generated`,
  `mariadb` and `docker (<database>)` all successful:
  `scripts/release-require-ci.sh`; jobs of other workflows do not count). Check it yourself
  first: `gh run list --repo rocketinsidecorp/forge-go --workflow ci.yml --commit "$(git rev-parse HEAD)"`.
- `README.md` and the docs never name a version that does not exist: they
  use `@latest` and `vX.Y.Z` placeholders
  (`TestDocsNeverInstallAVersionThatIsNotATag` enforces it), so tagging needs
  no documentation bump. The framework version `forge new` pins is the tag
  (`forge version` reports it; a binary installed with `go install ...@vX.Y.Z`
  and one from the archive agree).
- Repository settings: a ruleset protects `v*` tags (only maintainers create
  them, no deletion or update), and the repository can create build
  provenance attestations (a public repository, or GitHub Enterprise Cloud).
- Read the diff since the previous tag of `.github/workflows/`, `scripts/`,
  `go.mod` and `go.sum`: the release runs the scripts of the tagged commit.

## Tag and push

```sh
git tag -s v0.1.0 -m "Forge-Go v0.1.0"               # signed, if you have a signing key; -a otherwise
git push origin v0.1.0
gh run watch --repo rocketinsidecorp/forge-go "$(gh run list --repo rocketinsidecorp/forge-go --workflow release.yml --limit 1 --json databaseId --jq '.[0].databaseId')"
```

The workflow, in order: `verify` (tag shape, module path, on `main`, CI green on
the commit), then in
parallel `images` (signed container images) and `sbom` (binary SBOMs), then
`binaries` (archives, checksums, signature, attestations), then `draft` (the
draft release). A red job leaves no draft: fix the cause on `main`, delete the
tag only if **nothing** from it was published (no image, no release), and tag
the next patch otherwise.

If `verify` refuses because CI has not passed on the commit (no `ci.yml` run, a job is missing,
still running or failed, and the message names it), nothing was built or
signed: wait for CI to finish, re-run a failed CI job if it was a flake, and
then re-run the release workflow (`gh run rerun --failed`, or the run's page);
if CI cannot go green on that commit, fix `main` and tag the next patch.

## Verify the draft

Before publishing, check the draft the way a user checks the release. A draft's
assets need your own `gh` login:

```sh
VERSION=v0.1.0 OS=linux ARCH=amd64
mkdir /tmp/verify-$VERSION && cd /tmp/verify-$VERSION
gh release download "$VERSION" --repo rocketinsidecorp/forge-go           # every asset of the draft
ls                                                  # 12 archives, 4 *.cdx.json, checksums.txt, checksums.txt.sigstore.json, provenance.jsonl

cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/rocketinsidecorp/forge-go/.github/workflows/release.yml@refs/tags/$VERSION" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum -c checksums.txt                          # every file; all OK
for archive in "forge_${VERSION}_${OS}_${ARCH}.tar.gz" "forge-proxy_${VERSION}_${OS}_${ARCH}.tar.gz"; do
  gh attestation verify "$archive" --repo rocketinsidecorp/forge-go \
    --signer-workflow rocketinsidecorp/forge-go/.github/workflows/release.yml --source-ref "refs/tags/$VERSION"
done

tar -xzf "forge_${VERSION}_${OS}_${ARCH}.tar.gz"
"./forge_${VERSION}_${OS}_${ARCH}/forge" version    # forge v0.1.0 (<12 hex>, <commit time>, go1.27.3 linux/amd64)
git ls-remote https://github.com/rocketinsidecorp/forge-go "refs/tags/$VERSION"   # the same commit as above
"./forge_${VERSION}_${OS}_${ARCH}/forge" new /tmp/verify-app --skip-deps && echo scaffold ok
```

Also verify both container images (commands in
[SUPPLY_CHAIN.md](SUPPLY_CHAIN.md#signed-images)), and read the generated
release notes: they list the pull requests since the previous tag, and their
first paragraph points users at the verification commands.

Checklist:

- [ ] `checksums.txt` signature verifies against this tag's workflow identity.
- [ ] `sha256sum -c checksums.txt` is all OK, and lists 12 archives and 4 SBOMs.
- [ ] `gh attestation verify` passes for one archive of each command.
- [ ] `forge version` shows the tag, the tag's commit and the Go version of `go.mod`.
- [ ] Both images verify with `cosign verify` (and have an SBOM and provenance).
- [ ] The pull request that last touched the release machinery passed the Claude Code Review with no unresolved blocking findings, including the check for malicious or exfiltrating code (unexpected network calls, credential or wallet/crypto-key access, obfuscated code, install scripts, new dependencies), as the Definition of Done requires.

## Publish

Only after every box is ticked:

```sh
gh release edit "$VERSION" --repo rocketinsidecorp/forge-go --draft=false --latest   # a pre-release: --prerelease, without --latest
```

Then, from a clean directory and a clean `GOPATH`, confirm the public path:

```sh
go install "github.com/rocketinsidecorp/forge-go/cmd/forge@$VERSION" && forge version
curl -sSLO https://github.com/rocketinsidecorp/forge-go/releases/download/v0.1.0/checksums.txt   # anonymous download works
```

If the repository enabled immutable releases, publishing locks the assets, and
that is why the draft already holds every asset: nothing is uploaded later.

## When a release is bad

A published tag and its images are never deleted or re-pointed: Go's checksum
database and every registry already know them. Unpublish the release if it is
dangerous (`gh release edit "$VERSION" --draft=true`), say so in the next
release's notes, and publish a fixed patch version. For a security problem
follow [SECURITY.md](../SECURITY.md).

## Parity with Forge-Rust

Release binaries, checksums, signatures and provenance are a user-facing
capability that Forge-Rust must offer with the same names and verification
commands (owner decision of 2026-10-20). Forge-Rust takes it after Forge-Go
reaches 100% (decision D37); [RAILS_PARITY.md](RAILS_PARITY.md) tracks it.
