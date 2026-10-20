#!/usr/bin/env bash
# Builds the forge CLI and forge-proxy the way a release does (static,
# -trimpath) and writes a CycloneDX SBOM of each binary, read by Syft from
# the module information the Go toolchain embeds in it.
#
#   scripts/sbom.sh dist/sbom                        # linux/amd64
#   GOARCH=arm64 scripts/sbom.sh dist/sbom
#   SYFT=/usr/local/bin/syft scripts/sbom.sh dist/sbom   # a local Syft, no Docker
#
# Syft runs from its image, pinned by digest, without network access. The
# output directory gets forge.cdx.json and forge-proxy.cdx.json.
set -euo pipefail

# ghcr.io/anchore/syft v1.54.1 (2026-10-06), the multi-architecture index.
syft_image="ghcr.io/anchore/syft:v1.54.1@sha256:3eb5379ba7b409c3f4069b686110527af0c47df993fa5c10d13e7cf34f49b1aa"

out="${1:?usage: scripts/sbom.sh OUTPUT_DIR}"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
bin="$(mktemp -d)"
trap 'rm -rf "$bin"' EXIT

export CGO_ENABLED=0 GOOS="${GOOS:-linux}" GOARCH="${GOARCH:-amd64}"
version="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
for name in forge forge-proxy; do
	go build -trimpath -ldflags="-s -w" -o "$bin/$name" "./cmd/$name"
	if [ -n "${SYFT:-}" ]; then
		SYFT_CHECK_FOR_APP_UPDATE=false "$SYFT" scan "file:$bin/$name" \
			--source-name "$name" --source-version "$version" \
			-o "cyclonedx-json=$out/$name.cdx.json"
	else
		docker run --rm --network none -e SYFT_CHECK_FOR_APP_UPDATE=false \
			-v "$bin:/in:ro" -v "$out:/out" "$syft_image" \
			scan "file:/in/$name" --source-name "$name" --source-version "$version" \
			-o "cyclonedx-json=/out/$name.cdx.json"
	fi
	echo "wrote $out/$name.cdx.json ($GOOS/$GOARCH)"
done
