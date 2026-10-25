package forge_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// workflowFiles are the GitHub Actions workflows of the framework and the
// ones `forge new` generates (docs/SUPPLY_CHAIN.md).
func workflowFiles(t *testing.T) []string {
	t.Helper()
	// `.yaml` workflows run too: none may escape the gate.
	var files []string
	for _, pattern := range []string{"*.yml", "*.yaml"} {
		found, err := filepath.Glob(filepath.Join(".github", "workflows", pattern))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, found...)
	}
	if len(files) == 0 {
		t.Fatal("no workflows found")
	}
	return append(files,
		filepath.Join("internal", "scaffold", "templates", "ci.yml.tmpl"),
		filepath.Join("internal", "scaffold", "templates", "github", "release.yml"))
}

func readText(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

var (
	pinnedUses = regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40}$`)
	versionTag = regexp.MustCompile(`^\s+# v\d+(\.\d+){0,2}$`)
	digestRef  = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	// syftImage and syftScanner are the SBOM generators; every copy must
	// carry the same digest so an update cannot miss one.
	syftImage   = regexp.MustCompile(`ghcr\.io/anchore/syft:[^\s"\\]+`)
	syftScanner = regexp.MustCompile(`docker\.io/docker/buildkit-syft-scanner:[^\s"\\]+`)
)

// localActionFiles returns the action.yml of every local `./` action the
// files reach, directly or through another local action: a local action runs
// with the workflow's permissions, so the same rules apply to it.
func localActionFiles(t *testing.T, files []string) []string {
	t.Helper()
	isFile := func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && info.Mode().IsRegular()
	}
	read := func(path string) string { return readText(t, path) }
	actions, problems := localActionTree(files, read, isFile)
	for _, problem := range problems {
		t.Error(problem)
	}
	return actions
}

// localActionTree follows the `uses: ./...` references of files: a local
// reusable workflow must be an existing file of .github/workflows, and a
// local action must have an action.yml (or action.yaml), whose own local
// references are followed in turn.
func localActionTree(files []string, read func(string) string, isFile func(string) bool) ([]string, []string) {
	var pending []string
	for _, path := range files {
		pending = append(pending, localActions(read(path))...)
	}
	seen := map[string]bool{}
	var actions, problems []string
	for len(pending) > 0 {
		action := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[action] {
			continue
		}
		seen[action] = true
		if strings.HasSuffix(action, ".yml") || strings.HasSuffix(action, ".yaml") {
			// Directly in .github/workflows: `../` cannot lead elsewhere.
			file, ok := strings.CutPrefix(action, ".github/workflows/")
			if !ok || strings.Contains(file, "/") || !isFile(action) {
				problems = append(problems, fmt.Sprintf("./%s is not a workflow of .github/workflows", action))
			}
			continue
		}
		found := ""
		for _, name := range []string{"action.yml", "action.yaml"} {
			if isFile(filepath.Join(action, name)) {
				found = filepath.Join(action, name)
				break
			}
		}
		if found == "" {
			problems = append(problems, fmt.Sprintf("./%s has no action.yml", action))
			continue
		}
		actions = append(actions, found)
		pending = append(pending, localActions(read(found))...)
	}
	return actions, problems
}

// TestWorkflowsPinThirdPartyCode keeps every action pinned to a commit SHA
// with its version in a comment, every container a workflow runs pinned by
// digest, and every checkout free of credentials: a moved tag cannot change
// what CI or a release executes.
func TestWorkflowsPinThirdPartyCode(t *testing.T) {
	t.Parallel()
	references := map[*regexp.Regexp]map[string]bool{syftImage: {}, syftScanner: {}}
	used := map[string]bool{}
	workflows := workflowFiles(t)
	files := append(append(workflows, localActionFiles(t, workflows)...), filepath.Join("scripts", "sbom.sh"))
	for _, path := range files {
		text := readText(t, path)
		violations := append(pinViolations(path, text), checkoutViolations(path, text)...)
		violations = append(violations, allowlistViolations(path, text)...)
		for _, line := range strings.Split(text, "\n") {
			code, _ := splitComment(line)
			for _, reference := range keyValues(code, "uses") {
				if repository := actionRepository(reference); repository != "" {
					used[repository] = true
				}
			}
		}
		// Generated templates name their database image through a template
		// field; internal/scaffold pins those values.
		if !strings.Contains(path, "templates") {
			violations = append(violations, containerViolations(path, text)...)
		}
		for _, violation := range violations {
			t.Error(violation)
		}
		for pattern, seen := range references {
			for _, reference := range pattern.FindAllString(text, -1) {
				if !digestRef.MatchString(reference) {
					t.Errorf("%s: %q must be pinned by digest", path, reference)
				}
				seen[reference] = true
			}
		}
	}
	// The allowlist holds what is in use, nothing more.
	for _, allowed := range allowedActions {
		if !used[allowed] {
			t.Errorf("%s is allowlisted but unused", allowed)
		}
	}
	for pattern, seen := range references {
		if len(seen) != 1 {
			t.Errorf("%s: want one pinned reference across the workflows, found %v", pattern, seen)
		}
	}
}

// forbiddenSecretAccess matches the spellings that hand a workflow's secrets
// on wholesale or by computed name, outside the reviewed list: `secrets[`,
// `toJSON(secrets)`, `join(secrets, ...)`, `{ x: secrets }` and
// `secrets: inherit`, in any case and spacing (expression contexts are
// case-insensitive).
var forbiddenSecretAccess = regexp.MustCompile(`(?i)\bsecrets\s*(?:[\[),}]|:\s*inherit)`)

// secretReference is a `secrets.NAME` read, in any case.
var secretReference = regexp.MustCompile(`(?i)\bsecrets\s*\.\s*[A-Za-z0-9_.]*`)

// writePermission is a `<scope>: write` pair, in block (`contents: write`) or
// flow style (`permissions: { contents: write }`), quoted or not.
var writePermission = regexp.MustCompile(`["']?([A-Za-z][A-Za-z-]*)["']?\s*:\s*["']?write["']?\s*(?:$|[,}])`)

// lineComment is a YAML comment: a `#` at the start of a line or after
// whitespace.
var lineComment = regexp.MustCompile(`(?:^|\s)#.*$`)

// TestWorkflowPermissionsAreLeastPrivilege requires a top-level permissions
// block in every workflow and refuses every `<scope>: write`; the only
// exceptions are the release workflow's packages and OIDC token, which belong
// to the one job that publishes. `write-all` and handing secrets on wholesale
// are refused everywhere.
func TestWorkflowPermissionsAreLeastPrivilege(t *testing.T) {
	t.Parallel()
	for _, path := range workflowFiles(t) {
		text := readText(t, path)
		if !regexp.MustCompile(`(?m)^permissions:`).MatchString(text) {
			t.Errorf("%s: no top-level permissions", path)
		}
		if strings.Contains(text, "write-all") {
			t.Errorf("%s: write access to everything", path)
		}
		if strings.Contains(text, "pull_request_target") {
			t.Errorf("%s: pull_request_target runs fork code with secrets", path)
		}
		release := filepath.Base(path) == "release.yml"
		for number, line := range strings.Split(text, "\n") {
			code := lineComment.ReplaceAllString(line, "")
			for _, match := range writePermission.FindAllStringSubmatch(code, -1) {
				scope := match[1]
				if !release || (scope != "id-token" && scope != "packages") {
					t.Errorf("%s:%d: %q is not allowed", path, number+1, scope+": write")
				}
			}
		}
		for _, permission := range []string{"id-token: write", "packages: write"} {
			count := strings.Count(text, permission)
			if (release && count != 1) || (!release && count != 0) {
				t.Errorf("%s: %q appears %d times", path, permission, count)
			}
		}
		for _, forbidden := range forbiddenSecretAccess.FindAllString(text, -1) {
			t.Errorf("%s: %q reaches secrets outside the reviewed list", path, forbidden)
		}
		// The only secret any workflow reads is the release job's token.
		for _, reference := range secretReference.FindAllString(text, -1) {
			if !release || reference != "secrets.GITHUB_TOKEN" {
				t.Errorf("%s: unexpected %q", path, reference)
			}
		}
	}
}

// TestReleaseWorkflowsRunOnlyForVersionTags keeps publishing out of pull
// requests, branch pushes, schedules and manual runs.
func TestReleaseWorkflowsRunOnlyForVersionTags(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		filepath.Join(".github", "workflows", "release.yml"),
		filepath.Join("internal", "scaffold", "templates", "github", "release.yml"),
	} {
		text := readText(t, path)
		trigger := regexp.MustCompile(`(?s)\non:\n(.*?)\n\S`).FindStringSubmatch(text)
		if trigger == nil || strings.TrimSpace(trigger[1]) != "push:\n    tags: [\"v*\"]" {
			t.Errorf("%s: release must trigger only on push of v* tags, got %q", path, trigger)
		}
		if !regexp.MustCompile(`(?m)^permissions: \{\}$`).MatchString(text) {
			t.Errorf("%s: release must default to no permissions", path)
		}
		for _, required := range []string{"cosign sign --yes \"${IMAGE}@${DIGEST}\"", "provenance: mode=max", "sbom: generator=", "git merge-base --is-ancestor"} {
			if !strings.Contains(text, required) {
				t.Errorf("%s: missing %q", path, required)
			}
		}
		// The framework's tag is its module version: the major version must
		// match the module path (a generated application is no module).
		if !strings.Contains(path, "templates") {
			for _, violation := range releaseViolations(path, text) {
				t.Error(violation)
			}
		}
	}
}
