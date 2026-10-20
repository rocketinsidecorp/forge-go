#!/usr/bin/env bash
# Runs every Go fuzz target of the module, one after another, for a fixed
# time each. Targets are discovered with `go test -list '^Fuzz'`, so a new
# FuzzXxx function is fuzzed in CI without editing a list.
#
#   scripts/fuzz.sh                        # every target, 10s each
#   scripts/fuzz.sh -t 2m ./i18n           # the i18n targets, 2 minutes each
#   scripts/fuzz.sh -t 5m -r '^FuzzParseToken$' ./auth
#   scripts/fuzz.sh -l                     # list "package target" pairs
#   scripts/fuzz.sh -j                     # the same, as JSON for a CI matrix
#   scripts/fuzz.sh -c DIR                 # copy new failing inputs into DIR
#
# A failing input is written by `go test` to the package's
# testdata/fuzz/<target>/ directory; commit it with the fix so it stays a
# regression test. The script fuzzes every target even after a failure and
# exits 1 if any failed.
set -euo pipefail

usage() {
	echo "usage: scripts/fuzz.sh [-t duration] [-r target-regexp] [-l | -j | -c dir] [package ...]" >&2
	exit 2
}

fuzztime="${FUZZTIME:-10s}"
filter='^Fuzz'
mode=run
while getopts 't:r:ljc:h' option; do
	case "$option" in
	t) fuzztime="$OPTARG" ;;
	r) filter="$OPTARG" ;;
	l) mode=list ;;
	j) mode=json ;;
	c)
		mode=collect
		collect_dir="$OPTARG"
		;;
	*) usage ;;
	esac
done
shift $((OPTIND - 1))

# collect copies the failing inputs go test wrote (new, untracked files under
# testdata/fuzz) into a directory, keeping their paths, for a CI artifact.
if [ "$mode" = collect ]; then
	mkdir -p "$collect_dir"
	count=0
	while IFS= read -r -d '' file; do
		mkdir -p "$collect_dir/$(dirname "$file")"
		cp "$file" "$collect_dir/$file"
		echo "failing input: $file"
		count=$((count + 1))
	done < <(git ls-files -z --others --exclude-standard -- '*testdata/fuzz/*')
	echo "collected $count failing inputs into $collect_dir"
	exit 0
fi
if [ "$#" -eq 0 ]; then
	set -- ./...
fi
# A duration (10s, 5m, 1h) or an iteration count (1000x), as go test takes.
if ! [[ "$fuzztime" =~ ^[0-9]+(ms|s|m|h|x)$ ]]; then
	echo "fuzz.sh: invalid duration $fuzztime" >&2
	exit 2
fi

# discover prints "import/path FuzzName" for every fuzz target. go test
# prints each package's output in one block, the names first and then the
# package's "ok" line, so names are buffered until that line names their
# package. A package that fails to build fails the script.
discover() {
	go test -run '^$' -list '^Fuzz' "$@" | awk -v filter="$filter" '
		/^Fuzz[A-Za-z0-9_]*$/ { names[count++] = $0; next }
		/^ok[ \t]/ {
			for (i = 0; i < count; i++) if (names[i] ~ filter) print $2, names[i]
			count = 0
			next
		}
		/^(FAIL|panic:)/ { failed = 1 }
		END { exit failed }
	'
}

targets="$(discover "$@")"
case "$mode" in
list)
	printf '%s\n' "$targets"
	exit 0
	;;
json)
	# id names a target uniquely in artifact and cache names: its package
	# relative to the module, with "-" for "/", then the target.
	printf '%s\n' "$targets" | awk -v module="$(go list -m)" '
		BEGIN { printf "[" }
		NF == 2 {
			id = $1
			if (id == module) id = ""
			else if (index(id, module "/") == 1) id = substr(id, length(module) + 2)
			gsub(/[^A-Za-z0-9_.]/, "-", id)
			id = (id == "" ? "" : id "-") $2
			printf "%s{\"package\":\"%s\",\"target\":\"%s\",\"id\":\"%s\"}", (n++ ? "," : ""), $1, $2, id
		}
		END { print "]" }
	'
	exit 0
	;;
esac

if [ -z "$targets" ]; then
	echo "fuzz.sh: no fuzz targets found" >&2
	exit 1
fi

failed=()
total=0
while read -r package target; do
	total=$((total + 1))
	echo "=== fuzz $package $target ($fuzztime)"
	if ! go test -run '^$' -fuzz "^${target}\$" -fuzztime "$fuzztime" "$package"; then
		failed+=("$package $target")
		if [ -n "${GITHUB_ACTIONS:-}" ]; then
			echo "::error title=Fuzzing failed::$package $target found a failing input; see testdata/fuzz/$target"
		fi
	fi
done <<<"$targets"

echo "fuzzed $total targets for $fuzztime each; ${#failed[@]} failed"
if [ "${#failed[@]}" -gt 0 ]; then
	printf 'FAIL %s\n' "${failed[@]}"
	exit 1
fi
