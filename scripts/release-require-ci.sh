#!/usr/bin/env bash
# Refuses a release of a commit the CI workflow has not passed on. It finds the
# run of .github/workflows/ci.yml for the commit (the newest push run of that
# workflow file, so a job of the same name in another workflow counts for
# nothing), requires that run to have finished, and requires every job in
# `required` below to have concluded "success" in it: a job that is missing,
# still running, failed, was cancelled or skipped stops the release before
# anything is built or signed. It fails closed: a run or job it cannot read is
# a refusal.
#
#   GH_TOKEN=... GH_REPO=rocketinsidecorp/forge-go scripts/release-require-ci.sh "$GITHUB_SHA"
#
# Runs on a runner, where gh and jq are installed: GH_TOKEN is the workflow's
# token (`actions: read` is enough) and GH_REPO (or, failing that, the runner's
# GITHUB_REPOSITORY) names the repository. The jobs listed are the latest
# attempt of the run (a re-run replaces the failure it re-ran). A test
# (release_ci_gate_test.go) keeps `required` equal to the jobs of ci.yml that
# gate a release.
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

# The names are the job names of ci.yml: the job id, and for the docker matrix
# "docker (<database>)".
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
workflow=".github/workflows/ci.yml"

# `|| status=$?` keeps a failing gh or jq from being swallowed by the pipe.
unreadable() {
	echo "release-require-ci: could not read $1 of $sha in $repository" >&2
	exit 1
}

# The newest push run of ci.yml for this commit, as "id<TAB>status". A run's
# path may carry "@<ref>" after the file. --paginate prints one document per
# page, so jq slurps them.
status=0
run="$(gh api --paginate "repos/$repository/actions/runs?head_sha=$sha&event=push&per_page=100" |
	jq -rs --arg sha "$sha" --arg workflow "$workflow" '[.[].workflow_runs[] | select(.head_sha == $sha and ((.path // "") | split("@")[0]) == $workflow)] | sort_by(.id) | last // empty | [.id, .status] | @tsv')" || status=$?
[ "$status" -eq 0 ] || unreadable "the workflow runs"
if [ -z "$run" ]; then
	echo "release-require-ci: there is no push run of $workflow on $sha: CI has not run on this commit" >&2
	echo "release-require-ci: refusing to release $sha: CI has not passed on it." >&2
	exit 1
fi
IFS=$'\t' read -r run_id run_status <<RUN
$run
RUN
if ! [[ "$run_id" =~ ^[0-9]+$ ]]; then
	unreadable "the id of the CI run"
fi
if [ "$run_status" != "completed" ]; then
	echo "release-require-ci: the CI run $run_id on $sha has not finished ($run_status)" >&2
	echo "release-require-ci: refusing to release $sha: CI has not passed on it. Wait for CI on that commit to finish green, then re-run this workflow." >&2
	exit 1
fi

# One line per job of that run: name, status and conclusion, separated by tabs.
status=0
jobs="$(gh api --paginate "repos/$repository/actions/runs/$run_id/jobs?filter=latest&per_page=100" |
	jq -r '.jobs[] | [.name, .status, (.conclusion // "")] | @tsv')" || status=$?
[ "$status" -eq 0 ] || unreadable "the jobs of CI run $run_id"

failed=0
for name in "${required[@]}"; do
	found=0
	while IFS=$'\t' read -r job_name job_status conclusion; do
		[ "$job_name" = "$name" ] || continue
		found=1
		if [ "$job_status" != "completed" ]; then
			echo "release-require-ci: CI job \"$name\" has not finished ($job_status)" >&2
			failed=1
		elif [ "$conclusion" != "success" ]; then
			echo "release-require-ci: CI job \"$name\" concluded \"$conclusion\", not \"success\"" >&2
			failed=1
		fi
	done <<JOBS
$jobs
JOBS
	if [ "$found" -eq 0 ]; then
		echo "release-require-ci: CI job \"$name\" is not in CI run $run_id" >&2
		failed=1
	fi
done
if [ "$failed" -ne 0 ]; then
	echo "release-require-ci: refusing to release $sha: CI has not passed on it. Wait for CI on that commit to finish green (a failed job can be re-run), then re-run this workflow." >&2
	exit 1
fi
echo "CI run $run_id passed on $sha: ${required[*]}"
