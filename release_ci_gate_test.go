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
// never appears.
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
	// Every other workflow's jobs must not reuse a required name: the script
	// matches check runs by name.
	other, err := filepath.Glob(filepath.Join(".github", "workflows", "*.y*ml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range other {
		if filepath.Base(path) == "ci.yml" {
			continue
		}
		documents, err := yamlDocuments(readText(t, path), false)
		if err != nil {
			t.Fatal(err)
		}
		jobs := yamlGet(documents[0].Content[0], "jobs")
		for index := 0; jobs != nil && index+1 < len(jobs.Content); index += 2 {
			if id := jobs.Content[index].Value; slices.Contains(required, id) {
				t.Errorf("%s: job %q shares its name with a CI job the release requires", path, id)
			}
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

// checkRun is one entry of the fake GitHub API's answer.
type checkRun struct{ name, app, status, conclusion string }

// fakeGH writes a gh that records its arguments and answers `gh api` with the
// check runs as GitHub's JSON, or fails.
func fakeGH(t *testing.T, runs []checkRun, fail bool) (bin, calls string) {
	t.Helper()
	bin = t.TempDir()
	calls = filepath.Join(bin, "calls")
	var entries []string
	for _, run := range runs {
		conclusion := "null"
		if run.conclusion != "" {
			conclusion = `"` + run.conclusion + `"`
		}
		entries = append(entries, fmt.Sprintf(`{"name":%q,"status":%q,"conclusion":%s,"app":{"slug":%q}}`, run.name, run.status, conclusion, run.app))
	}
	body := `{"total_count":` + fmt.Sprint(len(runs)) + `,"check_runs":[` + strings.Join(entries, ",") + `]}`
	script := "#!/bin/sh\necho \"$@\" >> " + calls + "\n"
	if fail {
		script += "echo 'gh: HTTP 403' >&2\nexit 1\n"
	} else {
		script += "cat <<'JSON'\n" + body + "\nJSON\n"
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

func passingRuns(t *testing.T) []checkRun {
	t.Helper()
	var runs []checkRun
	for _, name := range gateRequired(t) {
		runs = append(runs, checkRun{name, "github-actions", "completed", "success"})
	}
	// Jobs the release does not wait for may be in any state.
	return append(runs, checkRun{"benchmarks", "github-actions", "completed", "failure"}, checkRun{"macos", "github-actions", "in_progress", ""})
}

const someSHA = "0123456789abcdef0123456789abcdef01234567"

// TestReleaseRequiresCIToHavePassedOnTheCommit runs release-require-ci.sh with
// a fake gh: the release goes ahead only when every required job concluded
// "success", and every other state or failure to read stops it.
func TestReleaseRequiresCIToHavePassedOnTheCommit(t *testing.T) {
	t.Parallel()
	requireReleaseTools(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	replace := func(name string, with checkRun) []checkRun {
		var runs []checkRun
		for _, run := range passingRuns(t) {
			if run.name == name {
				run = with
			}
			runs = append(runs, run)
		}
		return runs
	}
	without := func(name string) []checkRun {
		return slices.DeleteFunc(passingRuns(t), func(run checkRun) bool { return run.name == name })
	}
	cases := []struct {
		name    string
		runs    []checkRun
		fail    bool
		sha     string
		wantErr string // empty: the release may go ahead
	}{
		{name: "every required job succeeded", runs: passingRuns(t)},
		{name: "a failed job", runs: replace("coverage", checkRun{"coverage", "github-actions", "completed", "failure"}), wantErr: `"coverage" concluded "failure"`},
		{name: "a cancelled job", runs: replace("generated", checkRun{"generated", "github-actions", "completed", "cancelled"}), wantErr: `"generated" concluded "cancelled"`},
		{name: "a skipped job", runs: replace("quality", checkRun{"quality", "github-actions", "completed", "skipped"}), wantErr: `"quality" concluded "skipped"`},
		{name: "a job still running", runs: replace("mariadb", checkRun{"mariadb", "github-actions", "in_progress", ""}), wantErr: `"mariadb" has not finished (in_progress)`},
		{name: "a queued matrix job", runs: replace("docker (sqlite)", checkRun{"docker (sqlite)", "github-actions", "queued", ""}), wantErr: `"docker (sqlite)" has not finished (queued)`},
		{name: "a missing job", runs: without("docker (mysql)"), wantErr: `"docker (mysql)" has no check run`},
		{name: "no check runs at all", runs: nil, wantErr: `"quality" has no check run`},
		{name: "a success reported by another app", runs: replace("quality", checkRun{"quality", "some-other-app", "completed", "success"}), wantErr: `"quality" has no check run`},
		{name: "gh failing", runs: passingRuns(t), fail: true, wantErr: "could not read the check runs"},
		{name: "a branch name instead of a commit", runs: passingRuns(t), sha: "main", wantErr: "not a full commit SHA"},
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
				if called, _ := os.ReadFile(calls); !strings.Contains(string(called), "repos/example/forge/commits/"+someSHA+"/check-runs?filter=latest") {
					t.Errorf("gh was called with %q", called)
				}
				return
			}
			if err == nil {
				t.Fatalf("the release went ahead:\n%s", output)
			}
			if !strings.Contains(output, test.wantErr) {
				t.Errorf("output lacks %q:\n%s", test.wantErr, output)
			}
		})
	}
	t.Run("a refusal says what to do", func(t *testing.T) {
		t.Parallel()
		bin, _ := fakeGH(t, replace("quality", checkRun{"quality", "github-actions", "completed", "failure"}), false)
		output, err := releaseScript(t, []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "GH_REPO=example/forge"}, ciGateScript, someSHA)
		if err == nil || !regexp.MustCompile(`refusing to release `+someSHA+`.*re-run this workflow`).MatchString(output) {
			t.Errorf("err=%v output:\n%s", err, output)
		}
	})
	t.Run("no repository", func(t *testing.T) {
		t.Parallel()
		bin, _ := fakeGH(t, passingRuns(t), false)
		output, err := releaseScript(t, []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "GH_REPO=", "GITHUB_REPOSITORY="}, ciGateScript, someSHA)
		if err == nil || !strings.Contains(output, "set GH_REPO") {
			t.Errorf("err=%v output:\n%s", err, output)
		}
	})
}
