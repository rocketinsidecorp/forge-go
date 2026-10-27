#!/usr/bin/env bash
# Completes the release directory that scripts/release-build.sh filled:
# optionally copies the binary SBOMs into it under release names, then writes
# checksums.txt, the SHA-256 of every file there (sha256sum format, sorted),
# and checks it. The release workflow signs checksums.txt with cosign and
# attests every file it lists.
#
#   scripts/release-checksums.sh v0.1.0 dist/release
#   scripts/release-checksums.sh v0.1.0 dist/release dist/sbom   # SBOM_DIR holds sbom-<tag>-linux-<arch>/<command>.cdx.json
#
# An SBOM <command>.cdx.json of sbom-v0.1.0-linux-amd64 becomes
# <command>_v0.1.0_linux_amd64.cdx.json, next to the archive it describes.
set -euo pipefail
export LC_ALL=C

version="${1:?usage: scripts/release-checksums.sh VERSION RELEASE_DIR [SBOM_DIR]}"
release="${2:?usage: scripts/release-checksums.sh VERSION RELEASE_DIR [SBOM_DIR]}"
sbom="${3:-}"

cd "$release"
if [ -n "$sbom" ]; then
	copied=0
	for directory in "$sbom"/sbom-*-linux-*; do
		[ -d "$directory" ] || continue
		arch="${directory##*-}"
		for file in "$directory"/*.cdx.json; do
			[ -f "$file" ] || continue
			name="$(basename "$file" .cdx.json)"
			cp "$file" "${name}_${version}_linux_${arch}.cdx.json"
			copied=$((copied + 1))
		done
	done
	if [ "$copied" -eq 0 ]; then
		echo "release-checksums: no SBOM found under $sbom" >&2
		exit 1
	fi
fi

if command -v sha256sum >/dev/null 2>&1; then
	hash=(sha256sum)
else
	hash=(shasum -a 256)
fi
files=()
for file in *; do
	case "$file" in checksums.txt*) continue ;; esac
	[ -f "$file" ] && files+=("$file")
done
if [ "${#files[@]}" -eq 0 ]; then
	echo "release-checksums: $release holds no files" >&2
	exit 1
fi
"${hash[@]}" "${files[@]}" >checksums.txt
"${hash[@]}" -c checksums.txt
echo "wrote $release/checksums.txt (${#files[@]} files)"
