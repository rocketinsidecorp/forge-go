#!/usr/bin/env bash
# Creates the DRAFT GitHub release of a tag from the release directory: every
# archive (the names scripts/release-build.sh would write), checksums.txt, its
# Sigstore bundle, the SBOMs and the provenance bundle, with notes generated
# from the tag's commits. It never publishes: a maintainer verifies the assets
# (docs/RELEASING.md) and then publishes the draft by hand.
#
#   GH_TOKEN=... GH_REPO=rocketinsidecorp/forge-go scripts/release-draft.sh v0.1.0 dist/release
#
# Runs on a runner, where gh is installed; GH_TOKEN and GITHUB_REPOSITORY come
# from the workflow. A tag with a hyphen (v0.1.0-rc.1) is marked a pre-release.
set -euo pipefail
export LC_ALL=C

tag="${1:?usage: scripts/release-draft.sh TAG RELEASE_DIR}"
release="${2:?usage: scripts/release-draft.sh TAG RELEASE_DIR}"
repository="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}"
root="$(cd "$(dirname "$0")/.." && pwd)"

missing=0
assets=()
while IFS= read -r name; do
	assets+=("$release/$name")
done < <(DRY_RUN=1 "$root/scripts/release-build.sh" "$tag" /dev/null; echo checksums.txt; echo checksums.txt.sigstore.json)
for asset in "${assets[@]}"; do
	if [ ! -f "$asset" ]; then
		echo "release-draft: $asset is missing" >&2
		missing=1
	fi
done
if [ "$missing" -ne 0 ]; then
	exit 1
fi
# What the draft carries is what the signed checksums list.
(cd "$release" && if command -v sha256sum >/dev/null 2>&1; then sha256sum -c checksums.txt; else shasum -a 256 -c checksums.txt; fi)
# The SBOMs and the provenance bundle ride along when present.
for asset in "$release"/*.cdx.json "$release"/provenance.jsonl; do
	[ -f "$asset" ] && assets+=("$asset")
done

flags=(--draft --verify-tag --generate-notes --title "$tag")
if [[ "$tag" == *-* ]]; then
	flags+=(--prerelease)
fi
notes="Verify before you install: checksums.txt is signed with Sigstore cosign and every archive carries a GitHub build provenance attestation. The commands are in ${GITHUB_SERVER_URL:-https://github.com}/$repository/blob/$tag/docs/SUPPLY_CHAIN.md#verify-a-release"
gh release create "$tag" "${assets[@]}" "${flags[@]}" --notes "$notes"
echo "drafted the release of $tag with ${#assets[@]} assets; it is not published"
