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
	// usesLine matches every spelling of the key: `uses:`, `- uses:`,
	// `"uses":` and `uses :`; the value is unquoted before it is checked.
	usesLine   = regexp.MustCompile(`^\s*(?:-\s*)?["']?uses["']?\s*:\s*(\S+)(.*)$`)
	pinnedUses = regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40}$`)
	versionTag = regexp.MustCompile(`^\s+# v\d+(\.\d+){0,2}$`)
	digestRef  = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	// syftImage and syftScanner are the SBOM generators; every copy must
	// carry the same digest so an update cannot miss one.
	syftImage   = regexp.MustCompile(`ghcr\.io/anchore/syft:[^\s"\\]+`)
	syftScanner = regexp.MustCompile(`docker\.io/docker/buildkit-syft-scanner:[^\s"\\]+`)
)

// containerImage returns the image a line names as a service `image:` or as
// the `container: <image>` shorthand, with its comment and quotes stripped.
// A bare `container:` opens a mapping whose `image:` line is checked on its
// own, and a `- image:` matrix entry (release.yml) is not a container.
func containerImage(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	value, ok := strings.CutPrefix(trimmed, "image:")
	if !ok {
		value, ok = strings.CutPrefix(trimmed, "container:")
	}
	if !ok {
		return "", false
	}
	if code, _, found := strings.Cut(value, " #"); found {
		value = code
	}
	value = strings.Trim(strings.TrimSpace(value), `"'`)
	return value, value != ""
}

// TestWorkflowsPinThirdPartyCode keeps every action pinned to a commit SHA
// with its version in a comment, and every container a workflow runs pinned
// by digest: a moved tag cannot change what CI or a release executes.
func TestWorkflowsPinThirdPartyCode(t *testing.T) {
	t.Parallel()
	references := map[*regexp.Regexp]map[string]bool{syftImage: {}, syftScanner: {}}
	files := append(workflowFiles(t), filepath.Join("scripts", "sbom.sh"))
	for _, path := range files {
		text := readText(t, path)
		checkouts := 0
		for number, line := range strings.Split(text, "\n") {
			if match := usesLine.FindStringSubmatch(line); match != nil {
				reference := strings.Trim(match[1], `"'`)
				if strings.HasPrefix(reference, "actions/checkout@") {
					checkouts++
				}
				if !strings.HasPrefix(reference, "./") && (!pinnedUses.MatchString(reference) || !versionTag.MatchString(match[2])) {
					t.Errorf("%s:%d: %q must be owner/repo@<40-hex commit> # vX.Y.Z", path, number+1, strings.TrimSpace(line))
				}
			}
			if image, ok := containerImage(line); ok && !strings.Contains(path, "templates") && !digestRef.MatchString(image) {
				t.Errorf("%s:%d: service image or container %q must be pinned by digest", path, number+1, image)
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
		if checkouts != strings.Count(text, "persist-credentials: false") {
			t.Errorf("%s: every checkout must set persist-credentials: false", path)
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
	}
}
