#!/usr/bin/env bash
# Builds the release archives of the forge CLI and forge-proxy: one per
# command and platform, named <command>_<version>_<os>_<arch>.tar.gz (.zip for
# windows), each holding a directory of the same name with the binary, LICENSE
# and README.md. Plain `go build`, no release tool: the binaries are static
# (CGO_ENABLED=0; SQLite is the pure-Go modernc.org/sqlite), -trimpath, with
# the version, commit and commit time stamped through internal/version (shown
# by `forge version`) and the toolchain's own VCS stamp (-buildvcs=true,
# `go version -m`). Archives are reproducible: the same commit and Go
# toolchain give the same bytes (sorted entries, the commit's time as every
# modification time, no owner, gzip without a name or time).
#
#   scripts/release-build.sh v0.1.0 dist/release
#   TARGETS="linux/amd64 darwin/arm64" scripts/release-build.sh v0.1.0 dist/release
#   DRY_RUN=1 scripts/release-build.sh v0.1.0 x      # list the archive names, build nothing
#
# TARGETS is a space-separated list of GOOS/GOARCH (default: linux, darwin
# and windows on amd64 and arm64). Needs git, go, GNU tar (gtar on macOS),
# gzip and, for windows targets, zip. A tree with changes is refused when the
# version is a tag of the checked-out commit (or in a tag run of the workflow);
# a local build of an untagged version is stamped with a -dirty commit. The release workflow runs it, then
# scripts/release-checksums.sh; docs/RELEASING.md has the whole procedure.
set -euo pipefail
export LC_ALL=C

version="${1:?usage: scripts/release-build.sh VERSION OUTPUT_DIR}"
out="${2:?usage: scripts/release-build.sh VERSION OUTPUT_DIR}"
targets="${TARGETS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64}"
commands="forge forge-proxy"

if ! [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
	echo "release-build: $version is not a version such as v1.2.3 or v1.2.3-rc.1" >&2
	exit 1
fi

# archive_name prints the archive's base name (without .tar.gz or .zip).
archive_name() { echo "${1}_${version}_${2%/*}_${2#*/}"; }
archive_file() {
	case "${2%/*}" in
	windows) echo "$(archive_name "$1" "$2").zip" ;;
	*) echo "$(archive_name "$1" "$2").tar.gz" ;;
	esac
}

if [ "${DRY_RUN:-}" = 1 ]; then
	for target in $targets; do
		for command in $commands; do
			archive_file "$command" "$target"
		done
	done
	exit 0
fi

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
mkdir -p "$out"
out="$(cd "$out" && pwd)"

tar_command=tar
if ! tar --version 2>/dev/null | grep -q 'GNU tar'; then
	if command -v gtar >/dev/null 2>&1; then
		tar_command=gtar
	else
		echo "release-build: GNU tar is required (brew install gnu-tar, then gtar)" >&2
		exit 1
	fi
fi

module="$(awk '$1 == "module" { print $2; exit }' go.mod)"
commit="$(git rev-parse HEAD)"
if [ -n "$(git status --porcelain)" ]; then
	# A release is built from exactly the tagged commit: refuse a tree with
	# changes when the tag names this commit, or in a tag run of the workflow.
	# A local build of a version nobody tagged here is stamped -dirty instead.
	tagged="$(git rev-parse -q --verify "refs/tags/$version^{commit}" || true)"
	if [ "$tagged" = "$commit" ] || [ "${GITHUB_REF_TYPE:-}" = tag ]; then
		echo "release-build: the working tree has changes; a release is built from the clean tag $version" >&2
		git status --short >&2
		exit 1
	fi
	commit="$commit-dirty"
fi
epoch="$(git log -1 --format=%ct)"
date="$(TZ=UTC git log -1 --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ)"
stamp="$(TZ=UTC git log -1 --format=%cd --date=format-local:%Y%m%d%H%M.%S)"
ldflags="-s -w -X $module/internal/version.Version=$version -X $module/internal/version.Commit=$commit -X $module/internal/version.Date=$date"

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

for target in $targets; do
	goos="${target%/*}"
	goarch="${target#*/}"
	extension=""
	if [ "$goos" = windows ]; then
		extension=".exe"
	fi
	for command in $commands; do
		base="$(archive_name "$command" "$target")"
		directory="$stage/$base"
		mkdir -p "$directory"
		CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
			go build -trimpath -buildvcs=true -ldflags "$ldflags" -o "$directory/$command$extension" "./cmd/$command"
		cp LICENSE README.md "$directory/"
		chmod 0755 "$directory" "$directory/$command$extension"
		chmod 0644 "$directory/LICENSE" "$directory/README.md"
		TZ=UTC touch -t "$stamp" "$directory" "$directory/$command$extension" "$directory/LICENSE" "$directory/README.md"
		file="$out/$(archive_file "$command" "$target")"
		rm -f "$file"
		if [ "$goos" = windows ]; then
			(cd "$stage" && TZ=UTC zip -X -q -9 "$file" "$base/$command$extension" "$base/LICENSE" "$base/README.md")
		else
			(cd "$stage" && "$tar_command" --format=gnu --sort=name --mtime="@$epoch" --owner=0 --group=0 --numeric-owner \
				-cf - "$base" | gzip -n -9 >"$file")
		fi
		echo "wrote $file"
	done
done
