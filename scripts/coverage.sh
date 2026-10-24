#!/usr/bin/env bash
# Statement coverage of the framework module, measured with
# `go test -coverpkg=./...` and gated by the committed floor in
# .coverage-floor (docs/TESTING.md, "Coverage"). Same name, floor file and
# output as Forge-Rust's scripts/coverage.sh.
#
#   scripts/coverage.sh                                  # measure, report, gate
#   COVERAGE_PROFILE=cover.out scripts/coverage.sh       # report and gate an existing profile
#
# The database tests run when their servers are named, as in CI:
# FORGE_TEST_POSTGRES_URL, FORGE_TEST_DATABASE_URL and FORGE_TEST_MYSQL_URL
# (FORGE_TEST_REQUIRE_DATABASE=true turns a missing server into a failure).
#
# Every package is measured, through every package's tests (-coverpkg): a
# block counts as covered when any test binary ran it. Excluded, each for the
# reason in docs/TESTING.md, and nothing else:
#   - cmd/*/main.go, the main shims of the two binaries;
#   - examples/**, the example applications;
#   - internal/version, the variables the linker sets;
#   - internal/deploy/internal/fakedocker, the docker stand-in the deploy
#     tests run as a separate process.
# Writes dist/coverage/{raw,coverage}.out, prints the total and a per-package
# table (also to $GITHUB_STEP_SUMMARY when set), and exits 1 below the floor.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
out="$root/dist/coverage"
mkdir -p "$out"
floor="$(tr -d '[:space:]' < .coverage-floor)"
module="$(awk '$1 == "module" { print $2; exit }' go.mod)"

raw="${COVERAGE_PROFILE:-}"
if [ -z "$raw" ]; then
	raw="$out/raw.out"
	go test -count=1 -coverpkg=./... -coverprofile="$raw" ./...
fi

# One line per block: dropped when excluded, merged across test binaries
# (covered when any binary covered it). The result is a `mode: set` profile.
merged="$out/coverage.out"
awk -v module="$module/" '
	NR == 1 { next }
	{
		file = substr($1, 1, index($1, ":") - 1)
		path = substr(file, length(module) + 1)
		if (path ~ /^cmd\/[^\/]+\/main\.go$/ || path ~ /^examples\// ||
			path ~ /^internal\/version\// || path ~ /^internal\/deploy\/internal\/fakedocker\//) {
			next
		}
		if (!($1 in statements)) {
			order[++blocks] = $1
		}
		statements[$1] = $2
		if ($3 > 0) {
			covered[$1] = 1
		}
	}
	END {
		print "mode: set"
		for (i = 1; i <= blocks; i++) {
			block = order[i]
			print block, statements[block], (block in covered) ? 1 : 0
		}
	}
' "$raw" > "$merged"

total="$(go tool cover -func="$merged" | awk '$1 == "total:" { sub(/%/, "", $NF); print $NF }')"

# Per package: covered statements, statements, percent.
report="$(awk -v module="$module/" '
	NR == 1 { next }
	{
		file = substr($1, 1, index($1, ":") - 1)
		package = substr(file, length(module) + 1)
		sub(/\/[^\/]+$/, "", package)
		if (package == "" || index(package, "/") == 0 && package ~ /\.go$/) {
			package = "."
		}
		all[package] += $2
		if ($3 > 0) {
			hit[package] += $2
		}
	}
	END {
		for (package in all) {
			printf "| `%s` | %d | %d | %.1f%% |\n", package, hit[package], all[package], all[package] ? 100 * hit[package] / all[package] : 100
		}
	}
' "$merged" | sort)"

table="$(printf '### Statement coverage: %s%% (floor %s%%)\n\n| Package | Covered statements | Statements | Coverage |\n| --- | ---: | ---: | ---: |\n%s\n' "$total" "$floor" "$report")"
printf '%s\n' "$table"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	printf '%s\n' "$table" >> "$GITHUB_STEP_SUMMARY"
fi

if awk -v total="$total" -v floor="$floor" 'BEGIN { exit !(total + 0 < floor + 0) }'; then
	echo "coverage ${total}% is below the floor ${floor}% (.coverage-floor)" >&2
	exit 1
fi
echo "coverage ${total}% >= floor ${floor}%"
