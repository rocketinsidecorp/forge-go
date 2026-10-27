#!/usr/bin/env bash
# Refuses a release of a commit the CI workflow has not passed on. It reads the
# check runs GitHub Actions recorded for the commit and requires every job in
# `required` below to have finished with the conclusion "success": a job that
# is missing, still running, failed, was cancelled or skipped stops the release
# before anything is built or signed. It fails closed: a check it cannot read
# is a refusal.
#
#   GH_TOKEN=... GH_REPO=rocketinsidecorp/forge-go scripts/release-require-ci.sh "$GITHUB_SHA"
#
# Runs on a runner, where gh and jq are installed: GH_TOKEN is the workflow's
# token (`checks: read` is enough) and GH_REPO (or, failing that, the runner's
# GITHUB_REPOSITORY) names the repository. Only check runs created by GitHub
# Actions count, and the latest run of each name (a re-run replaces the failure
# it re-ran). A test (supply_chain_test.go) keeps `required` equal to the jobs
# of .github/workflows/ci.yml that gate a release.
set -euo pipefail
export LC_ALL=C

sha="${1:?usage: scripts/release-require-ci.sh SHA}"
repository="${GH_REPO:-${GITHUB_REPOSITORY:-}}"
if [ -z "$repository" ]; then
	echo "release-require-ci: set GH_REPO (owner/name) or GITHUB_REPOSITORY" >&2
	exit 1
fi
if ! [[ "$sha" =~ ^[0-9a-f]{40}$ ]]; then
	echo "release-require-ci: $sha is not a full commit SHA" >&2
	exit 1
fi
for tool in gh jq; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "release-require-ci: $tool is not installed" >&2
		exit 1
	fi
done

# The check run names are the job names of ci.yml: the job id, and for the
# docker matrix "docker (<database>)".
required=(
	"quality"
	"coverage"
	"generated"
	"mariadb"
	"docker (postgresql)"
	"docker (mysql)"
	"docker (mariadb)"
	"docker (sqlite)"
)

# One line per check run: name, status and conclusion, separated by tabs.
# `|| status=$?` keeps a failing gh or jq from being swallowed by the pipe.
status=0
runs="$(gh api --paginate "repos/$repository/commits/$sha/check-runs?filter=latest&per_page=100" |
	jq -r '.check_runs[] | select(.app.slug == "github-actions") | [.name, .status, (.conclusion // "")] | @tsv')" || status=$?
if [ "$status" -ne 0 ]; then
	echo "release-require-ci: could not read the check runs of $sha in $repository" >&2
	exit 1
fi

failed=0
for name in "${required[@]}"; do
	found=0
	while IFS=$'\t' read -r run_name run_status conclusion; do
		[ "$run_name" = "$name" ] || continue
		found=1
		if [ "$run_status" != "completed" ]; then
			echo "release-require-ci: CI job \"$name\" has not finished ($run_status)" >&2
			failed=1
		elif [ "$conclusion" != "success" ]; then
			echo "release-require-ci: CI job \"$name\" concluded \"$conclusion\", not \"success\"" >&2
			failed=1
		fi
	done <<RUNS
$runs
RUNS
	if [ "$found" -eq 0 ]; then
		echo "release-require-ci: CI job \"$name\" has no check run on $sha" >&2
		failed=1
	fi
done
if [ "$failed" -ne 0 ]; then
	echo "release-require-ci: refusing to release $sha: CI has not passed on it. Wait for CI on that commit to finish green (a failed job can be re-run), then re-run this workflow." >&2
	exit 1
fi
echo "CI passed on $sha: ${required[*]}"
