package forge_test

// Tests of the gate that keeps a tag from being released unless CI passed on
// its commit: scripts/release-require-ci.sh (against a fake gh) and the way
// release.yml runs it, before every other job.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const ciGateScript = "release-require-ci.sh"

// ciCheckNames are the names GitHub gives the check runs of the jobs of
// ci.yml that gate a release: the job id, and "id (value)" for each value of
// a one-key matrix. Jobs outside the gate are listed in notGating.
func ciCheckNames(t *testing.T) map[string]bool {
	t.Helper()
	documents, err := yamlDocuments(readText(t, filepath.Join(".github", "workflows", "ci.yml")), false)
	if err != nil {
		t.Fatal(err)
	}
	jobs := yamlGet(documents[0].Content[0], "jobs")
	names := map[string]bool{}
	for index := 0; index+1 < len(jobs.Content); index += 2 {
		id, job := jobs.Content[index].Value, jobs.Content[index+1]
		if yamlGet(job, "name") != nil {
			t.Fatalf("ci.yml job %q sets a name: the gate's check names are job ids", id)
		}
		matrix := yamlGet(yamlGet(job, "strategy"), "matrix")
		if matrix == nil || len(matrix.Content) != 2 || matrix.Content[1].Kind != yaml.SequenceNode {
			names[id] = true
			continue
		}
		for _, value := range matrix.Content[1].Content {
			names[fmt.Sprintf("%s (%s)", id, value.Value)] = true
		}
	}
	return names
}

// notGating are the jobs of ci.yml a release does not wait for: smoke tests
// and artifacts that do not decide whether the code is releasable, and the
// macOS job (a portability check, unproven until it has run on GitHub).
var notGating = []string{"benchmarks", "fuzz", "sbom", "macos"}

// gateRequired reads the `required=( ... )` list of the script.
func gateRequired(t *testing.T) []string {
	t.Helper()
	script := readText(t, filepath.Join("scripts", ciGateScript))
	_, rest, ok := strings.Cut(script, "required=(\n")
	if !ok {
		t.Fatal("scripts/release-require-ci.sh has no required=( list")
	}
	list, _, _ := strings.Cut(rest, "\n)")
	var names []string
	for _, line := range strings.Split(list, "\n") {
		if name := strings.Trim(strings.TrimSpace(line), `"`); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// TestReleaseGateRequiresEveryGatingCIJob keeps the script's list equal to the
// jobs of ci.yml: a new CI job is either required by a release or named in
// notGating, and a renamed job cannot leave the gate waiting for a name that
// never appears. (The script reads the jobs of the ci.yml run, so another
// workflow's job of the same name cannot satisfy it.)
func TestReleaseGateRequiresEveryGatingCIJob(t *testing.T) {
	t.Parallel()
	required := gateRequired(t)
	var want []string
	for name := range ciCheckNames(t) {
		if !slices.ContainsFunc(notGating, func(skipped string) bool { return name == skipped || strings.HasPrefix(name, skipped+" (") }) {
			want = append(want, name)
		}
	}
	slices.Sort(want)
	got := slices.Clone(required)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("release-require-ci.sh requires %q, ci.yml's gating jobs are %q", got, want)
	}
	for _, name := range []string{"quality", "coverage", "generated", "mariadb"} {
		if !slices.Contains(required, name) {
			t.Errorf("%q is not required", name)
		}
	}
}

// TestReleaseChecksCIBeforeAnyOtherJob: the gate is a step of the verify job,
// after the tag checks, and every other job needs verify, directly or through
// another job, so nothing is built, pushed or signed before it passed.
func TestReleaseChecksCIBeforeAnyOtherJob(t *testing.T) {
	t.Parallel()
	documents, err := yamlDocuments(readText(t, frameworkReleasePath), false)
	if err != nil {
		t.Fatal(err)
	}
	jobs := yamlGet(documents[0].Content[0], "jobs")
	needs := map[string][]string{}
	for index := 0; index+1 < len(jobs.Content); index += 2 {
		id, job := jobs.Content[index].Value, jobs.Content[index+1]
		if node := yamlGet(job, "needs"); node != nil {
			if node.Kind == yaml.ScalarNode {
				needs[id] = []string{node.Value}
			}
			for _, need := range node.Content {
				needs[id] = append(needs[id], need.Value)
			}
		}
		if id != "verify" {
			continue
		}
		gate := slices.IndexFunc(yamlGet(job, "steps").Content, func(step *yaml.Node) bool {
			run := yamlGet(step, "run")
			return run != nil && strings.TrimSpace(run.Value) == `scripts/release-require-ci.sh "$GITHUB_SHA"`
		})
		if gate < 0 {
			t.Fatal("the verify job does not run scripts/release-require-ci.sh \"$GITHUB_SHA\"")
		}
		if token := yamlGet(yamlGet(yamlGet(job, "steps").Content[gate], "env"), "GH_TOKEN"); token == nil || token.Value != "${{ secrets.GITHUB_TOKEN }}" {
			t.Error("the gate step reads GH_TOKEN from the workflow token")
		}
	}
	var reachesVerify func(job string, seen map[string]bool) bool
	reachesVerify = func(job string, seen map[string]bool) bool {
		if seen[job] {
			return false
		}
		seen[job] = true
		for _, need := range needs[job] {
			if need == "verify" || reachesVerify(need, seen) {
				return true
			}
		}
		return false
	}
	for index := 0; index+1 < len(jobs.Content); index += 2 {
		if id := jobs.Content[index].Value; id != "verify" && !reachesVerify(id, map[string]bool{}) {
			t.Errorf("job %q does not need verify, so it can run before CI was checked", id)
		}
	}
}

// ciRun is a workflow run of the fake GitHub API, with the jobs of its latest
// attempt.
type ciRun struct {
	id             int
	path, sha      string
	status         string
	jobs           []ciJob
	jobsUnreadable bool
}

type ciJob struct{ name, status, conclusion string }

// fakeGH writes a gh that records its arguments and answers `gh api` for the
// workflow runs of a commit and for the jobs of a run, as GitHub's JSON, or
// fails.
func fakeGH(t *testing.T, runs []ciRun, fail bool) (bin, calls string) {
	t.Helper()
	bin = t.TempDir()
	calls = filepath.Join(bin, "calls")
	quote := func(value string) string { return `"` + value + `"` }
	var listed []string
	script := "#!/bin/sh\necho \"$@\" >> " + calls + "\n"
	if fail {
		script += "echo 'gh: HTTP 403' >&2\nexit 1\n"
	}
	for _, run := range runs {
		listed = append(listed, fmt.Sprintf(`{"id":%d,"path":%s,"head_sha":%s,"event":"push","status":%s}`, run.id, quote(run.path), quote(run.sha), quote(run.status)))
		var jobs []string
		for _, job := range run.jobs {
			conclusion := "null"
			if job.conclusion != "" {
				conclusion = quote(job.conclusion)
			}
			jobs = append(jobs, fmt.Sprintf(`{"name":%s,"status":%s,"conclusion":%s}`, quote(job.name), quote(job.status), conclusion))
		}
		if run.jobsUnreadable {
			script += fmt.Sprintf("case \"$*\" in *actions/runs/%d/jobs*) echo 'gh: HTTP 500' >&2; exit 1;; esac\n", run.id)
			continue
		}
		script += fmt.Sprintf("case \"$*\" in *actions/runs/%d/jobs*) cat <<'JSON'\n{\"total_count\":%d,\"jobs\":[%s]}\nJSON\nexit 0;; esac\n", run.id, len(jobs), strings.Join(jobs, ","))
	}
	script += "case \"$*\" in *actions/runs?*) cat <<'JSON'\n{\"total_count\":" + fmt.Sprint(len(listed)) + ",\"workflow_runs\":[" + strings.Join(listed, ",") + "]}\nJSON\nexit 0;; esac\necho \"unexpected gh call: $*\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

const (
	someSHA  = "0123456789abcdef0123456789abcdef01234567"
	ciPath   = ".github/workflows/ci.yml"
	fuzzPath = ".github/workflows/fuzz.yml"
)

// passingJobs are the required jobs, all successful, and two the release does
// not wait for, in states that must not matter.
func passingJobs(t *testing.T) []ciJob {
	t.Helper()
	var jobs []ciJob
	for _, name := range gateRequired(t) {
		jobs = append(jobs, ciJob{name, "completed", "success"})
	}
	return append(jobs, ciJob{"benchmarks", "completed", "failure"}, ciJob{"macos", "in_progress", ""})
}

// TestReleaseRequiresCIToHavePassedOnTheCommit runs release-require-ci.sh with
// a fake gh: the release goes ahead only when the ci.yml run of the commit has
// finished and every required job of it concluded "success", and every other
// state, a job of another workflow, or a failure to read stops it.
func TestReleaseRequiresCIToHavePassedOnTheCommit(t *testing.T) {
	t.Parallel()
	requireReleaseTools(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	with := func(name string, job ciJob) []ciJob {
		var jobs []ciJob
		for _, existing := range passingJobs(t) {
			if existing.name == name {
				existing = job
			}
			jobs = append(jobs, existing)
		}
		return jobs
	}
	without := func(name string) []ciJob {
		return slices.DeleteFunc(passingJobs(t), func(job ciJob) bool { return job.name == name })
	}
	ci := func(id int, jobs []ciJob) ciRun {
		return ciRun{id: id, path: ciPath, sha: someSHA, status: "completed", jobs: jobs}
	}
	cases := []struct {
		name    string
		runs    []ciRun
		fail    bool
		sha     string
		wantErr string // empty: the release may go ahead
	}{
		{name: "every required job succeeded", runs: []ciRun{ci(10, passingJobs(t))}},
		{name: "a run whose path names the ref", runs: []ciRun{{id: 10, path: ciPath + "@refs/heads/main", sha: someSHA, status: "completed", jobs: passingJobs(t)}}},
		{name: "a failed job", runs: []ciRun{ci(10, with("coverage", ciJob{"coverage", "completed", "failure"}))}, wantErr: `"coverage" concluded "failure"`},
		{name: "a cancelled job", runs: []ciRun{ci(10, with("generated", ciJob{"generated", "completed", "cancelled"}))}, wantErr: `"generated" concluded "cancelled"`},
		{name: "a skipped job", runs: []ciRun{ci(10, with("quality", ciJob{"quality", "completed", "skipped"}))}, wantErr: `"quality" concluded "skipped"`},
		{name: "a job still running", runs: []ciRun{ci(10, with("mariadb", ciJob{"mariadb", "in_progress", ""}))}, wantErr: `"mariadb" has not finished (in_progress)`},
		{name: "a queued matrix job", runs: []ciRun{ci(10, with("docker (sqlite)", ciJob{"docker (sqlite)", "queued", ""}))}, wantErr: `"docker (sqlite)" has not finished (queued)`},
		{name: "a missing job", runs: []ciRun{ci(10, without("docker (mysql)"))}, wantErr: `"docker (mysql)" is not in CI run 10`},
		{name: "a run without jobs", runs: []ciRun{ci(10, nil)}, wantErr: `"quality" is not in CI run 10`},
		{name: "no run at all", wantErr: "there is no push run of .github/workflows/ci.yml"},
		{name: "a run still in progress", runs: []ciRun{{id: 10, path: ciPath, sha: someSHA, status: "in_progress", jobs: passingJobs(t)}}, wantErr: "has not finished (in_progress)"},
		// A job named like a required one in another workflow proves nothing.
		{name: "only another workflow ran, with every job green", runs: []ciRun{{id: 11, path: fuzzPath, sha: someSHA, status: "completed", jobs: passingJobs(t)}}, wantErr: "there is no push run of .github/workflows/ci.yml"},
		{name: "a green quality job in another workflow beside a failed one in ci.yml", runs: []ciRun{
			ci(10, with("quality", ciJob{"quality", "completed", "failure"})),
			{id: 11, path: fuzzPath, sha: someSHA, status: "completed", jobs: passingJobs(t)},
		}, wantErr: `"quality" concluded "failure"`},
		{name: "a ci.yml run of another commit", runs: []ciRun{{id: 10, path: ciPath, sha: strings.Repeat("a", 40), status: "completed", jobs: passingJobs(t)}}, wantErr: "there is no push run of .github/workflows/ci.yml"},
		// Two runs of ci.yml on the commit: the newest decides.
		{name: "a newer run passed after an older one failed", runs: []ciRun{ci(10, with("quality", ciJob{"quality", "completed", "failure"})), ci(12, passingJobs(t))}},
		{name: "a newer run failed after an older one passed", runs: []ciRun{ci(12, with("quality", ciJob{"quality", "completed", "failure"})), ci(10, passingJobs(t))}, wantErr: `"quality" concluded "failure"`},
		{name: "a newer run still running after an older one passed", runs: []ciRun{ci(10, passingJobs(t)), {id: 12, path: ciPath, sha: someSHA, status: "queued", jobs: passingJobs(t)}}, wantErr: "CI run 12 on " + someSHA + " has not finished (queued)"},
		{name: "jobs that cannot be read", runs: []ciRun{{id: 10, path: ciPath, sha: someSHA, status: "completed", jobsUnreadable: true}}, wantErr: "could not read the jobs of CI run 10"},
		{name: "gh failing", runs: []ciRun{ci(10, passingJobs(t))}, fail: true, wantErr: "could not read the workflow runs"},
		{name: "a branch name instead of a commit", runs: []ciRun{ci(10, passingJobs(t))}, sha: "main", wantErr: "not a full commit SHA"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			bin, calls := fakeGH(t, test.runs, test.fail)
			sha := test.sha
			if sha == "" {
				sha = someSHA
			}
			env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "GH_REPO=example/forge", "GITHUB_REPOSITORY="}
			output, err := releaseScript(t, env, ciGateScript, sha)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("the release was refused: %v\n%s", err, output)
				}
				called, _ := os.ReadFile(calls)
				for _, want := range []string{"repos/example/forge/actions/runs?head_sha=" + someSHA + "&event=push", "/jobs?filter=latest"} {
					if !strings.Contains(string(called), want) {
						t.Errorf("gh was called with %q, want it to contain %q", called, want)
					}
				}
				return
			}
			if err == nil {
				t.Fatalf("the release went ahead:\n%s", output)
			}
			if !strings.Contains(output, test.wantErr) {
				t.Errorf("output lacks %q:\n%s", test.wantErr, output)
			}
			if !strings.Contains(output, "refusing to release") && !strings.Contains(output, "not a full commit SHA") && !strings.Contains(output, "could not read") {
				t.Errorf("the refusal does not say so:\n%s", output)
			}
		})
	}
	t.Run("a refusal says what to do", func(t *testing.T) {
		t.Parallel()
		bin, _ := fakeGH(t, []ciRun{ci(10, with("quality", ciJob{"quality", "completed", "failure"}))}, false)
		output, err := releaseScript(t, []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "GH_REPO=example/forge"}, ciGateScript, someSHA)
		if err == nil || !regexp.MustCompile(`refusing to release `+someSHA+`.*re-run this workflow`).MatchString(output) {
			t.Errorf("err=%v output:\n%s", err, output)
		}
	})
	t.Run("no repository", func(t *testing.T) {
		t.Parallel()
		bin, _ := fakeGH(t, []ciRun{ci(10, passingJobs(t))}, false)
		output, err := releaseScript(t, []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "GH_REPO=", "GITHUB_REPOSITORY="}, ciGateScript, someSHA)
		if err == nil || !strings.Contains(output, "set GH_REPO") {
			t.Errorf("err=%v output:\n%s", err, output)
		}
	})
}
