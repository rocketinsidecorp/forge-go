package forge_test

import (
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
	files, err := filepath.Glob(filepath.Join(".github", "workflows", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
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
	usesLine   = regexp.MustCompile(`^\s*(?:-\s+)?uses:\s*(\S+)(.*)$`)
	pinnedUses = regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40}$`)
	versionTag = regexp.MustCompile(`^\s+# v\d+(\.\d+){0,2}$`)
	digestRef  = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	// syftImage and syftScanner are the SBOM generators; every copy must
	// carry the same digest so an update cannot miss one.
	syftImage   = regexp.MustCompile(`ghcr\.io/anchore/syft:[^\s"\\]+`)
	syftScanner = regexp.MustCompile(`docker\.io/docker/buildkit-syft-scanner:[^\s"\\]+`)
)

// TestWorkflowsPinThirdPartyCode keeps every action pinned to a commit SHA
// with its version in a comment, and every container a workflow runs pinned
// by digest: a moved tag cannot change what CI or a release executes.
func TestWorkflowsPinThirdPartyCode(t *testing.T) {
	t.Parallel()
	references := map[*regexp.Regexp]map[string]bool{syftImage: {}, syftScanner: {}}
	files := append(workflowFiles(t), filepath.Join("scripts", "sbom.sh"))
	for _, path := range files {
		text := readText(t, path)
		for number, line := range strings.Split(text, "\n") {
			if match := usesLine.FindStringSubmatch(line); match != nil && !strings.HasPrefix(match[1], "./") {
				if !pinnedUses.MatchString(match[1]) || !versionTag.MatchString(match[2]) {
					t.Errorf("%s:%d: %q must be owner/repo@<40-hex commit> # vX.Y.Z", path, number+1, strings.TrimSpace(line))
				}
			}
			if image, ok := strings.CutPrefix(strings.TrimSpace(line), "image: "); ok && !strings.Contains(path, "templates") && !digestRef.MatchString(image) {
				t.Errorf("%s:%d: service image %q must be pinned by digest", path, number+1, image)
			}
		}
		for pattern, seen := range references {
			for _, reference := range pattern.FindAllString(text, -1) {
				if !digestRef.MatchString(reference) {
					t.Errorf("%s: %q must be pinned by digest", path, reference)
				}
				seen[reference] = true
			}
		}
		// The token stays out of .git/config, where later steps could read it.
		if strings.Count(text, "uses: actions/checkout@") != strings.Count(text, "persist-credentials: false") {
			t.Errorf("%s: every checkout must set persist-credentials: false", path)
		}
	}
	for pattern, seen := range references {
		if len(seen) != 1 {
			t.Errorf("%s: want one pinned reference across the workflows, found %v", pattern, seen)
		}
	}
}

// TestWorkflowPermissionsAreLeastPrivilege requires a top-level permissions
// block in every workflow, and keeps the write permissions of the release
// workflows (packages, OIDC token) to the one job that publishes.
func TestWorkflowPermissionsAreLeastPrivilege(t *testing.T) {
	t.Parallel()
	for _, path := range workflowFiles(t) {
		text := readText(t, path)
		if !regexp.MustCompile(`(?m)^permissions:`).MatchString(text) {
			t.Errorf("%s: no top-level permissions", path)
		}
		if regexp.MustCompile(`(?m)^permissions:\s*write-all|^\s+contents: write`).MatchString(text) {
			t.Errorf("%s: write access to the repository contents", path)
		}
		release := filepath.Base(path) == "release.yml"
		for _, permission := range []string{"id-token: write", "packages: write"} {
			count := strings.Count(text, permission)
			if (release && count != 1) || (!release && count != 0) {
				t.Errorf("%s: %q appears %d times", path, permission, count)
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
	}
}
