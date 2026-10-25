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
# The gross total is that measurement. The GATED total, the one the floor
# checks, additionally leaves out the contract and test kits listed in `kits`
# below (decision D7): harnesses whose only uncovered code is the failure
# branch of an assertion, verified instead by running against every real
# backend in CI. The list is explicit and checked: a kit that no longer
# exists, is not named *test, or has a consumer outside _test.go files and
# other kits is production code and fails the script.
# Writes dist/coverage/{raw,coverage-gross,coverage}.out (coverage.out is the
# gated profile), prints both totals and a per-package table of the gated
# packages (also to $GITHUB_STEP_SUMMARY when set), and exits 1 when the gated
# total is below the floor.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
out="$root/dist/coverage"
mkdir -p "$out"
floor="$(tr -d '[:space:]' < .coverage-floor)"
module="$(awk '$1 == "module" { print $2; exit }' go.mod)"

# Contract and test kits excluded from the gated total (docs/TESTING.md,
# "Coverage"). Add a package here only if it is a harness for other packages'
# tests; the check below refuses anything shipped code depends on.
kits=(
	cache/cachetest
	queue/storetest
	storage/storagetest
	migrate/migratetest
	ratelimit/limitertest
	mail/mailtest
	telemetry/telemetrytest
	mysql/mysqltest
	postgres/postgrestest
	sqlite/sqlitetest
	dbtest
)

in_kit() { # in_kit <path relative to the module root>
	local kit
	for kit in "${kits[@]}"; do
		case "$1" in "$kit"/*) return 0 ;; esac
	done
	return 1
}

for kit in "${kits[@]}"; do
	case "${kit##*/}" in
	*test) ;;
	*)
		echo "coverage kit $kit is not named *test; it cannot be excluded" >&2
		exit 1
		;;
	esac
	if [ ! -d "$kit" ]; then
		echo "coverage kit $kit does not exist: update kits in scripts/coverage.sh and docs/TESTING.md" >&2
		exit 1
	fi
	while IFS= read -r consumer; do
		consumer="${consumer#./}"
		if ! in_kit "$consumer"; then
			echo "coverage kit $kit is imported by $consumer, which is not a test or another kit: it is production code and cannot be excluded" >&2
			exit 1
		fi
	done < <(grep -rl --include='*.go' --exclude='*_test.go' --exclude-dir=.git --exclude-dir=dist -e "\"$module/$kit\"" . || true)
done

raw="${COVERAGE_PROFILE:-}"
if [ -z "$raw" ]; then
	raw="$out/raw.out"
	go test -count=1 -coverpkg=./... -coverprofile="$raw" ./...
fi

# One line per block: dropped when excluded, merged across test binaries
# (covered when any binary covered it). The result is a `mode: set` profile.
merged="$out/coverage-gross.out"
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

gross="$(go tool cover -func="$merged" | awk '$1 == "total:" { sub(/%/, "", $NF); print $NF }')"

# The gated profile: the gross one without the kits.
gated="$out/coverage.out"
awk -v module="$module/" -v kits="${kits[*]}" '
	BEGIN { count = split(kits, list, " ") }
	NR == 1 { print; next }
	{
		file = substr($1, 1, index($1, ":") - 1)
		path = substr(file, length(module) + 1)
		for (i = 1; i <= count; i++) {
			if (index(path, list[i] "/") == 1) {
				next
			}
		}
		print
	}
' "$merged" > "$gated"
total="$(go tool cover -func="$gated" | awk '$1 == "total:" { sub(/%/, "", $NF); print $NF }')"

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
' "$gated" | sort)"

table="$(printf '### Statement coverage: %s%% gated (floor %s%%), %s%% gross\n\nGated: production code, without the contract kits. Gross: also the kits (%s).\n\n| Package | Covered statements | Statements | Coverage |\n| --- | ---: | ---: | ---: |\n%s\n' "$total" "$floor" "$gross" "${kits[*]}" "$report")"
printf '%s\n' "$table"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	printf '%s\n' "$table" >> "$GITHUB_STEP_SUMMARY"
fi

if awk -v total="$total" -v floor="$floor" 'BEGIN { exit !(total + 0 < floor + 0) }'; then
	echo "coverage ${total}% (gross ${gross}%) is below the floor ${floor}% (.coverage-floor)" >&2
	exit 1
fi
echo "coverage ${total}% (gross ${gross}%) >= floor ${floor}%"
