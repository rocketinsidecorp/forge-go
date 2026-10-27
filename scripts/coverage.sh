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
#   - internal/deploy/internal/fakedocker, the docker stand-in the deploy
#     tests run as a separate process.
# The gross total is that measurement. The GATED total, the one the floor
# checks, additionally leaves out the contract and test kits listed in `kits`
# below (decision D7): harnesses whose only uncovered code is the failure
# branch of an assertion, verified instead by running against every real
# backend in CI. The list is explicit and checked: a kit that no longer
# exists, is not named *test, or has a consumer outside _test.go files and
# other kits is production code and fails the script.
# Three gates, all of which must hold (docs/TESTING.md, "Coverage"):
#   - the GATED total is at least the floor in .coverage-floor;
#   - the GROSS total is at least gross_floor, set below (kits included, so a kit
#     cannot be hidden from the measurement by being added to `kits`);
#   - each security-relevant package in `guarded` below is at least
#     package_floor on its own, so a well covered module cannot average away
#     an untested authentication, credentials, CSRF, encryption or database
#     package.
# Writes dist/coverage/{raw,coverage-gross,coverage}.out (coverage.out is the
# gated profile), prints both totals, a per-package table of the gated
# packages and the table of the guarded packages (also to $GITHUB_STEP_SUMMARY
# when set), and exits 1 when any gate fails, after reporting all of them.
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
	auth/authtest
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

# Gross total floor: the measurement of every package but the three exclusions,
# kits included. It only rises.
gross_floor=95.0

# Security-relevant packages held to package_floor percent of their own
# statements (docs/TESTING.md, "Coverage"). Paths are relative to the module
# root; each must exist and be measured. Package encryption is the
# AES-GCM field encryption (docs/ENCRYPTION.md); postgres/internal/pgurl guards the
# PostgreSQL URL parsing the connection security depends on.
package_floor=95.0
guarded=(
	auth
	auth/authhttp
	auth/internal/sqlidentity
	auth/mysql
	auth/postgres
	auth/sqlite
	credentials
	csrf
	encryption
	postgres/internal/pgurl
	postgres
)

for package in "${guarded[@]}"; do
	if ! ls "$package"/*.go > /dev/null 2>&1; then
		echo "guarded package $package has no Go files: update guarded in scripts/coverage.sh and docs/TESTING.md" >&2
		exit 1
	fi
done

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
	go test -count=1 -timeout 30m -coverpkg=./... -coverprofile="$raw" ./...
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
			path ~ /^internal\/deploy\/internal\/fakedocker\//) {
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

# Covered statements and statements of a `mode: set` profile, as "covered all".
# The totals are compared exactly from these, never through the one-decimal
# rounding of `go tool cover -func`, which shows 94.95 to 94.99 as "95.0".
statements() { awk 'NR > 1 { all += $2; if ($3 > 0) hit += $2 } END { printf "%d %d\n", hit, all }' "$1"; }
# percent <covered> <all>: for display, two decimals; the gates compare exactly.
percent() { awk -v hit="$1" -v all="$2" 'BEGIN { printf "%.2f", all ? 100 * hit / all : 100 }'; }
# below <covered> <all> <floor>: succeeds when covered/all is under the floor.
below() { awk -v hit="$1" -v all="$2" -v floor="$3" 'BEGIN { exit !(hit * 100 < floor * all) }'; }

read -r gross_hit gross_all < <(statements "$merged")
gross="$(percent "$gross_hit" "$gross_all")"

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
read -r total_hit total_all < <(statements "$gated")
total="$(percent "$total_hit" "$total_all")"

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

# The guarded packages: covered statements, statements and percent of each,
# compared exactly (not through the one-decimal rounding of the table). A
# package without statements in the profile is a failure, not a pass.
guarded_report="$(awk -v module="$module/" -v floor="$package_floor" -v list="${guarded[*]}" '
	BEGIN { count = split(list, names, " "); for (i = 1; i <= count; i++) wanted[names[i]] = 1 }
	NR == 1 { next }
	{
		file = substr($1, 1, index($1, ":") - 1)
		package = substr(file, length(module) + 1)
		sub(/\/[^\/]+$/, "", package)
		if (package in wanted) {
			all[package] += $2
			if ($3 > 0) {
				hit[package] += $2
			}
		}
	}
	END {
		for (i = 1; i <= count; i++) {
			package = names[i]
			if (!all[package]) {
				printf "| `%s` | 0 | 0 | not measured | %s%% | FAIL |\n", package, floor
				continue
			}
			status = (hit[package] * 100 >= floor * all[package]) ? "ok" : "FAIL"
			printf "| `%s` | %d | %d | %.2f%% | %s%% | %s |\n", package, hit[package], all[package], 100 * hit[package] / all[package], floor, status
		}
	}
' "$gated")"

table="$(printf '### Statement coverage: %s%% gated (floor %s%%), %s%% gross (floor %s%%)\n\nGated: production code, without the contract kits. Gross: also the kits (%s).\n\n| Package | Covered statements | Statements | Coverage |\n| --- | ---: | ---: | ---: |\n%s\n\n### Security-relevant packages (floor %s%% each)\n\n| Package | Covered statements | Statements | Coverage | Floor | |\n| --- | ---: | ---: | ---: | ---: | --- |\n%s\n' "$total" "$floor" "$gross" "$gross_floor" "${kits[*]}" "$report" "$package_floor" "$guarded_report")"
printf '%s\n' "$table"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	printf '%s\n' "$table" >> "$GITHUB_STEP_SUMMARY"
fi

status=0
if below "$total_hit" "$total_all" "$floor"; then
	echo "coverage ${total}% (${total_hit}/${total_all} statements) is below the floor ${floor}% (.coverage-floor)" >&2
	status=1
fi
if below "$gross_hit" "$gross_all" "$gross_floor"; then
	echo "gross coverage ${gross}% (${gross_hit}/${gross_all} statements, kits included) is below the gross floor ${gross_floor}% (gross_floor in scripts/coverage.sh)" >&2
	status=1
fi
below="$(awk -F'|' -v floor="$package_floor" '$7 ~ /FAIL/ { gsub(/[ `]/, "", $2); gsub(/^ +| +$/, "", $5); print "package " $2 " is " ($5 == "not measured" ? "not measured" : "at " $5) ", below its per-package floor " floor "% (guarded in scripts/coverage.sh)" }' <<< "$guarded_report")"
if [ -n "$below" ]; then
	printf '%s\n' "$below" >&2
	status=1
fi
if [ "$status" -ne 0 ]; then
	exit 1
fi
echo "coverage ${total}% (gross ${gross}%) >= floor ${floor}%; gross >= ${gross_floor}%; ${#guarded[@]} guarded packages >= ${package_floor}%"
