package forge_test

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/rocketinsidecorp/forge-go/internal/frontend"
	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
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

// releaseScripts are the shell scripts the release and CI workflows run. They
// are read for the pins and the go-command redirects the gate refuses (not as
// YAML); what a script does inside is left to review (docs/SUPPLY_CHAIN.md,
// "Threat model and residual risk").
var releaseScripts = []string{
	filepath.Join("scripts", "sbom.sh"),
	filepath.Join("scripts", "release-build.sh"),
	filepath.Join("scripts", "release-checksums.sh"),
	filepath.Join("scripts", "release-draft.sh"),
	filepath.Join("scripts", "release-require-ci.sh"),
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
	files := append(append(workflows, localActionFiles(t, workflows)...), releaseScripts...)
	for _, path := range files {
		text := readText(t, path)
		violations := append(pinViolations(path, text), checkoutViolations(path, text)...)
		violations = append(violations, allowlistViolations(path, text)...)
		script := slices.Contains(releaseScripts, path)
		if !script {
			violations = append(violations, yamlSubsetViolations(path, text)...)
			violations = append(violations, parserViolations(path, text)...)
			violations = append(violations, treeViolations(path, text, filepath.Base(path) == "release.yml")...)
		}
		violations = append(violations, environmentViolations(path, text)...)
		// D8: the closed allowlists read workflows and local actions as
		// written; a template is read as rendered
		// (TestGeneratedWorkflowsPassTheGate).
		if !script && !strings.HasSuffix(path, ".tmpl") {
			violations = append(violations, closedListViolations(filepath.ToSlash(path), text)...)
		}
		if strings.HasSuffix(path, ".tmpl") {
			violations = append(violations, templateDirectiveViolations(path, text)...)
		}
		for _, line := range strings.Split(text, "\n") {
			code, _ := splitComment(line)
			for _, reference := range keyValues(code, "uses") {
				if repository := actionRepository(reference); repository != "" {
					used[repository] = true
				}
			}
		}
		// Generated templates name their database image through a template
		// field: the rendered workflows are gated (pinned by digest, the
		// framework CI's pins) by TestGeneratedWorkflowsPassTheGate.
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
				if !slices.Contains(writeScopesOf(path, release), scope) {
					t.Errorf("%s:%d: %q is not allowed", path, number+1, scope+": write")
				}
			}
		}
		// The framework's release workflow holds several write scopes in
		// several jobs: TestReleaseJobsHoldOnlyTheirPermissions fixes them.
		for _, permission := range []string{"id-token: write", "packages: write"} {
			count := strings.Count(text, permission)
			if (release && count != 1 && filepath.ToSlash(path) != frameworkReleasePath) || (!release && count != 0) {
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

// frameworkReleasePermissions are the permissions each job of the framework's
// release workflow holds, exactly: adding a job or a scope is a reviewed
// change to this table and to docs/SUPPLY_CHAIN.md. Least privilege: only
// the image job writes packages, only the binary job attests, and only the
// draft job can write contents (create a draft release); the draft job signs
// nothing and the signing jobs cannot touch the release.
var frameworkReleasePermissions = map[string]map[string]string{
	"verify":   {"contents": "read", "actions": "read"},
	"images":   {"contents": "read", "packages": "write", "id-token": "write"},
	"sbom":     {"contents": "read"},
	"binaries": {"contents": "read", "id-token": "write", "attestations": "write"},
	"draft":    {"contents": "write"},
}

// TestReleaseJobsHoldOnlyTheirPermissions reads the parsed release workflow:
// no permissions at the top, and every job exactly the permissions of
// frameworkReleasePermissions.
func TestReleaseJobsHoldOnlyTheirPermissions(t *testing.T) {
	t.Parallel()
	documents, err := yamlDocuments(readText(t, frameworkReleasePath), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range releasePermissionViolations(frameworkReleasePath, documents) {
		t.Error(problem)
	}
}

// releasePermissionViolations compares the release workflow's top-level and
// per-job permissions with frameworkReleasePermissions.
func releasePermissionViolations(path string, documents []*yaml.Node) []string {
	var violations []string
	if len(documents) != 1 || documents[0].Kind != yaml.DocumentNode || len(documents[0].Content) != 1 {
		return []string{path + ": want one YAML document"}
	}
	root := documents[0].Content[0]
	if top := yamlGet(root, "permissions"); top == nil || top.Kind != yaml.MappingNode || len(top.Content) != 0 {
		violations = append(violations, path+": the top-level permissions must be {}")
	}
	jobs := yamlGet(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return append(violations, path+": no jobs")
	}
	seen := map[string]bool{}
	for index := 0; index+1 < len(jobs.Content); index += 2 {
		name, job := jobs.Content[index].Value, jobs.Content[index+1]
		seen[name] = true
		want, listed := frameworkReleasePermissions[name]
		if !listed {
			violations = append(violations, fmt.Sprintf("%s: job %q is not in frameworkReleasePermissions", path, name))
			continue
		}
		got := map[string]string{}
		permissions := yamlGet(job, "permissions")
		if permissions == nil || permissions.Kind != yaml.MappingNode {
			violations = append(violations, fmt.Sprintf("%s: job %q has no permissions mapping", path, name))
			continue
		}
		for entry := 0; entry+1 < len(permissions.Content); entry += 2 {
			got[permissions.Content[entry].Value] = permissions.Content[entry+1].Value
		}
		if !maps.Equal(got, want) {
			violations = append(violations, fmt.Sprintf("%s: job %q holds %v, want %v", path, name, got, want))
		}
	}
	for name := range frameworkReleasePermissions {
		if !seen[name] {
			violations = append(violations, fmt.Sprintf("%s: job %q is gone", path, name))
		}
	}
	return violations
}

// TestReleaseWorkflowShipsSignedChecksummedBinaries keeps the pieces that make
// the binary release verifiable, and every action of the workflow pinned.
func TestReleaseWorkflowShipsSignedChecksummedBinaries(t *testing.T) {
	t.Parallel()
	workflow := readText(t, frameworkReleasePath)
	for _, violation := range binaryReleaseViolations(frameworkReleasePath, workflow, readText(t, filepath.Join("scripts", "release-draft.sh"))) {
		t.Error(violation)
	}
	for _, violation := range pinViolations(frameworkReleasePath, workflow) {
		t.Error(violation)
	}
	// Every uses: of the workflow is a full commit SHA.
	for number, line := range strings.Split(workflow, "\n") {
		code, _ := splitComment(line)
		for _, reference := range keyValues(code, "uses") {
			if !pinnedUses.MatchString(reference) {
				t.Errorf("%s:%d: %q is not pinned by a full commit SHA", frameworkReleasePath, number+1, reference)
			}
		}
	}
}

// pipeToShell is a download piped into a shell, in the spellings an
// installer one-liner takes: `curl ... | sh`, `wget -qO- ... | sudo bash`,
// `bash <(curl ...)` and `sh -c "$(curl ...)"`.
var pipeToShell = regexp.MustCompile(`(?i)\b(?:curl|wget)\b[^\n]*\|\s*(?:sudo\s+(?:-\S+\s+)*)?(?:env\s+\S+\s+)?(?:/\S*/)?(?:ba|z|da|k|c|fi)?sh\b|\b(?:ba|z|da|k)?sh\s+(?:-\S+\s+)*(?:<\(|"?\$\()\s*(?:curl|wget)\b`)

// TestNoDocumentTellsToPipeADownloadIntoAShell: Forge ships no installer
// script. Its install instructions download an archive, verify it and then
// run it (README.md, docs/SUPPLY_CHAIN.md): no document may teach the
// opposite, which would also be the first thing an attacker copies.
func TestNoDocumentTellsToPipeADownloadIntoAShell(t *testing.T) {
	t.Parallel()
	var documents []string
	for _, pattern := range []string{"*.md", filepath.Join("docs", "*.md"), filepath.Join("docs", "adr", "*.md")} {
		found, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, found...)
	}
	if !slices.Contains(documents, "README.md") {
		t.Fatal("README.md not found")
	}
	for _, path := range documents {
		for number, line := range strings.Split(readText(t, path), "\n") {
			if pipeToShell.MatchString(line) {
				t.Errorf("%s:%d: a download piped into a shell: %q", path, number+1, strings.TrimSpace(line))
			}
		}
	}
	// No script of the repository is the installer either.
	for _, name := range []string{"install.sh", "install", "get.sh"} {
		for _, dir := range []string{".", "scripts"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				t.Errorf("%s: Forge has no installer script (docs/SUPPLY_CHAIN.md)", filepath.Join(dir, name))
			}
		}
	}
}

// installedRef is the version of a `go install .../forge-go/cmd/<name>@REF`
// in a document.
var installedRef = regexp.MustCompile(`forge-go/cmd/[A-Za-z0-9-]+@([^\s"'\x60)]+)`)

// placeholderRefs are the only refs a document may install: the newest
// release, or a placeholder the reader replaces. A document that names a real
// version (@v0.1.0) is wrong before that tag exists, and stale after the next.
var placeholderRefs = []string{"latest", "vX.Y.Z", "VERSION", "$VERSION"}

// installRefViolations returns the `go install` refs of text that are not
// placeholders, as "line: ref".
func installRefViolations(path, text string) []string {
	var violations []string
	for number, line := range strings.Split(text, "\n") {
		for _, match := range installedRef.FindAllStringSubmatch(line, -1) {
			if !slices.Contains(placeholderRefs, match[1]) {
				violations = append(violations, fmt.Sprintf("%s:%d: installs @%s; use @latest or a placeholder (vX.Y.Z, VERSION) (no tag may be assumed to exist)", path, number+1, match[1]))
			}
		}
	}
	return violations
}

// TestDocsNeverInstallAVersionThatIsNotATag: the README and the docs install
// `@latest` or a `vX.Y.Z` placeholder, never a literal version, so no copyable
// command fails with "unknown revision" before a release exists (and none
// goes stale after it).
func TestDocsNeverInstallAVersionThatIsNotATag(t *testing.T) {
	t.Parallel()
	var documents []string
	for _, pattern := range []string{"*.md", filepath.Join("docs", "*.md"), filepath.Join("docs", "adr", "*.md")} {
		found, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, found...)
	}
	for _, path := range documents {
		for _, violation := range installRefViolations(path, readText(t, path)) {
			t.Error(violation)
		}
	}
	// The README and Getting started do install the CLI, with @latest.
	for _, path := range []string{"README.md", filepath.Join("docs", "GETTING_STARTED.md")} {
		if !strings.Contains(readText(t, path), "forge-go/cmd/forge@latest") {
			t.Errorf("%s: no `go install ...forge-go/cmd/forge@latest`", path)
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
		if len(trigger) < 2 || strings.TrimSpace(trigger[1]) != "push:\n    tags: [\"v*\"]" {
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

// TestGeneratedWorkflowsPassTheGate renders the generated application for
// every database (and none) with the real template data and gates the
// workflows it writes, so a template cannot produce a step the gate on the
// template text never sees.
func TestGeneratedWorkflowsPassTheGate(t *testing.T) {
	t.Parallel()
	frameworkCI := readText(t, filepath.Join(".github", "workflows", "ci.yml"))
	// What `forge new` writes under .github. Every file there is gated
	// whatever its name; a new one also fails here until it is listed.
	expected := []string{"dependabot.yml", "workflows/ci.yml", "workflows/release.yml"}
	// Every database (and none), two names and every locale: a template field
	// that renders differently for another option cannot pass unread.
	for _, database := range append([]string{""}, scaffold.Databases...) {
		for _, name := range []string{"gateapp", "shop"} {
			for _, locale := range scaffold.Locales {
				dir := filepath.Join(t.TempDir(), name)
				if _, err := scaffold.Generate(scaffold.Options{Name: name, Dir: dir, Database: database, Locale: locale}); err != nil {
					t.Fatalf("%s %s %s: %v", database, name, locale, err)
				}
				if found := toolConfigurationFiles(dir, nil); len(found) != 0 {
					t.Errorf("generated (%s, %s, %s): tool configuration files %v", database, name, locale, found)
				}
				files := generatedGitHubFiles(t, filepath.Join(dir, ".github"))
				if !slices.Equal(files, expected) {
					t.Errorf("generated (%s, %s, %s) .github holds %v, want %v", database, name, locale, files, expected)
				}
				for _, file := range files {
					text := readText(t, filepath.Join(dir, ".github", filepath.FromSlash(file)))
					shown := fmt.Sprintf("generated (%s, %s, %s) .github/%s", database, name, locale, file)
					violations := generatedFileViolations(shown, file, text)
					// The database service runs the image the framework's CI
					// tests with, pinned to the same digest.
					for _, container := range containerImages(text) {
						if !strings.Contains(frameworkCI, "image: "+container.image+"\n") {
							violations = append(violations, fmt.Sprintf("%s:%d: %q is not the image .github/workflows/ci.yml pins", shown, container.line, container.image))
						}
					}
					for _, violation := range violations {
						t.Error(violation)
					}
				}
			}
		}
	}
}

// generatedGitHubFiles lists every file below root (a generated
// application's .github), as sorted slash-separated relative paths.
func generatedGitHubFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(root, path)
		files = append(files, filepath.ToSlash(relative))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	return files
}

// generatedFileViolations runs every rule on a generated .github file: the
// YAML, parser, pin, allowlist and environment rules on all of them, and the
// workflow rules (checkouts, containers, the parsed tree) on workflows.
func generatedFileViolations(shown, file, text string) []string {
	violations := append(pinViolations(shown, text), allowlistViolations(shown, text)...)
	violations = append(violations, yamlSubsetViolations(shown, text)...)
	violations = append(violations, parserViolations(shown, text)...)
	violations = append(violations, environmentViolations(shown, text)...)
	// The closed allowlists list generated files without the variant.
	for _, violation := range closedListViolations("generated .github/"+file, text) {
		violations = append(violations, shown+": "+violation)
	}
	if strings.HasPrefix(file, "workflows/") {
		violations = append(violations, checkoutViolations(shown, text)...)
		violations = append(violations, containerViolations(shown, text)...)
		violations = append(violations, treeViolations(shown, text, path.Base(file) == "release.yml")...)
	}
	return violations
}

// goEnvFiles returns every go env file in root or below it: a file named
// `go.env`, or `env` in a directory named `go` (the go command reads
// $XDG_CONFIG_HOME/go/env, or $HOME/.config/go/env, and GOENV may name a
// go.env). A linked `go` directory counts. The walk does not enter skipped
// directories nor follow links to directories.
func goEnvFiles(root string, skipped []string) []string {
	var found []string
	for _, path := range toolConfigurationFiles(root, skipped) {
		base := strings.TrimSuffix(filepath.Base(path), ".tmpl")
		if base == "go.env" || base == "env" {
			found = append(found, path)
		}
	}
	return found
}

// TestNoGoEnvFileExistsInTheRepository: a committed go env file, reached by
// pointing HOME or XDG_CONFIG_HOME at the checkout, would set GOPROXY,
// GOSUMDB or GOFLAGS without naming them in a gated file. A future need goes
// through review and changes this test.
func TestNoGoEnvFileExistsInTheRepository(t *testing.T) {
	t.Parallel()
	if found := goEnvFiles(".", []string{".git"}); len(found) != 0 {
		t.Errorf("go env files found: %v", found)
	}
	scratch := t.TempDir()
	write := func(relative string) {
		target := filepath.Join(scratch, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("GOPROXY=https://evil.example\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a/go.env")
	write(".ci/go/env")
	write("b/.config/go/env")
	write("elsewhere/env")
	write("skipped/go.env")
	if err := os.MkdirAll(filepath.Join(scratch, "c"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(scratch, "elsewhere"), filepath.Join(scratch, "c", "go")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(scratch, ".ci", "go", "env"),
		filepath.Join(scratch, "a", "go.env"),
		filepath.Join(scratch, "b", ".config", "go", "env"),
		filepath.Join(scratch, "c", "go", "env"),
	}
	slices.Sort(want)
	if found := goEnvFiles(scratch, []string{filepath.Join(scratch, "skipped")}); !slices.Equal(found, want) {
		t.Errorf("found %v, want %v", found, want)
	}
}

// A workflow a new template adds under .github is listed by the walk and
// gated like the others, whatever its name.
func TestGeneratedGitHubWalkGatesEveryFile(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), ".github")
	if err := os.MkdirAll(filepath.Join(root, "workflows"), 0o750); err != nil {
		t.Fatal(err)
	}
	lint := "name: lint\non: [push]\npermissions:\n  contents: read\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: evil/act@main\n"
	if err := os.WriteFile(filepath.Join(root, "workflows", "lint.yml"), []byte(lint), 0o600); err != nil {
		t.Fatal(err)
	}
	files := generatedGitHubFiles(t, root)
	if !slices.Equal(files, []string{"workflows/lint.yml"}) {
		t.Fatalf("walked %v", files)
	}
	if violations := generatedFileViolations("lint.yml", files[0], lint); len(violations) == 0 {
		t.Error("an unpinned, unlisted action in a new generated workflow was accepted")
	}
}

// dockerfileImages returns the image of every FROM line of a Dockerfile,
// without its --platform flag and stage name.
func dockerfileImages(text string) []string {
	var images []string
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}
		fields = fields[1:]
		for len(fields) > 0 && strings.HasPrefix(fields[0], "--") {
			fields = fields[1:]
		}
		if len(fields) > 0 {
			images = append(images, fields[0])
		}
	}
	return images
}

// TestDockerfilesPinBaseImagesByDigest: a moved tag must not change what an
// image is built from. Forge's own Dockerfiles and the one `forge new`
// generates, for every database, pin each base image by digest, and the
// generated one uses exactly the images Forge builds with, so one reviewed
// update (Dependabot, docker) covers both.
func TestDockerfilesPinBaseImagesByDigest(t *testing.T) {
	t.Parallel()
	framework := map[string]bool{}
	for _, path := range []string{"Dockerfile", filepath.Join("cmd", "forge-proxy", "Dockerfile")} {
		images := dockerfileImages(readText(t, path))
		if len(images) == 0 {
			t.Fatalf("%s has no FROM line", path)
		}
		for _, image := range images {
			if !digestRef.MatchString(image) {
				t.Errorf("%s: base image %q must be pinned by digest", path, image)
			}
			framework[image] = true
		}
	}
	for _, database := range append([]string{""}, scaffold.Databases...) {
		dir := filepath.Join(t.TempDir(), "shop")
		if _, err := scaffold.Generate(scaffold.Options{Name: "shop", Dir: dir, Database: database}); err != nil {
			t.Fatalf("%q: %v", database, err)
		}
		images := dockerfileImages(readText(t, filepath.Join(dir, "Dockerfile")))
		if len(images) != 2 {
			t.Fatalf("database %q: generated Dockerfile has images %v, want a build and a runtime stage", database, images)
		}
		for _, image := range images {
			if !digestRef.MatchString(image) {
				t.Errorf("database %q: generated base image %q must be pinned by digest", database, image)
			} else if !framework[image] {
				t.Errorf("database %q: generated base image %q is not one Forge's own Dockerfiles pin", database, image)
			}
		}
	}
}

// nodeTool matches the Node package managers a workflow could run.
var nodeTool = regexp.MustCompile(`\b(npm|npx|yarn|pnpm|corepack|bun)\b`)

// TestTypeScriptCheckerIsTheOnlyNpmPackageAndIsPinned (ADR 0049): the one thing
// any workflow installs from npm is the TypeScript checker, at the exact
// release frontend.TypeScriptVersion, without lifecycle scripts. The
// framework's CI and the generated CI install the same one command, so an
// update is a single reviewed change.
func TestTypeScriptCheckerIsTheOnlyNpmPackageAndIsPinned(t *testing.T) {
	t.Parallel()
	want := "npm install --global --ignore-scripts typescript@" + frontend.TypeScriptVersion
	installs := 0
	for _, path := range workflowFiles(t) {
		for number, line := range strings.Split(readText(t, path), "\n") {
			code, _ := splitComment(line)
			code = strings.ReplaceAll(code, "{{.TypeScriptVersion}}", frontend.TypeScriptVersion) // the template field
			if !nodeTool.MatchString(code) {
				continue
			}
			if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(code), "run:")) != want {
				t.Errorf("%s:%d runs %q: the only Node command allowed is %q", path, number+1, strings.TrimSpace(code), want)
				continue
			}
			installs++
		}
	}
	// The framework's generated job and the generated application's test job.
	if installs != 2 {
		t.Errorf("found %d installs of the TypeScript checker, want 2 (.github/workflows/ci.yml and the generated CI)", installs)
	}
}

// TestTypeScriptTestsRunInTheJobThatPinsTsc keeps TestGeneratedTypeScript*
// (which run the real tsc) out of the race job, where tsc is whatever the
// runner has and a missing one skips, and in the `generated` job, which
// installs the pinned tsc and sets FORGE_TEST_REQUIRE_TSC.
func TestTypeScriptTestsRunInTheJobThatPinsTsc(t *testing.T) {
	t.Parallel()
	lines := strings.Split(readText(t, filepath.Join(".github", "workflows", "ci.yml")), "\n")
	jobOf := func(line int) string {
		for ; line >= 0; line-- {
			if l := lines[line]; strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ") && strings.HasSuffix(l, ":") {
				return strings.TrimSpace(strings.TrimSuffix(l, ":"))
			}
		}
		return ""
	}
	var race, generated, install, require string
	for number, line := range lines {
		code, _ := splitComment(line)
		switch {
		case strings.Contains(code, "go test -race") && strings.Contains(code, "-skip") && strings.Contains(code, "TestGeneratedApplication"):
			race = jobOf(number) + ": " + code
		case strings.Contains(code, "go test") && strings.Contains(code, "-run '^(TestGeneratedApplication"):
			generated = jobOf(number) + ": " + code
		case strings.Contains(code, "npm install --global --ignore-scripts typescript@"):
			install = jobOf(number)
		case strings.Contains(code, "FORGE_TEST_REQUIRE_TSC"):
			require = jobOf(number)
		}
	}
	const pattern = "TestGeneratedTypeScript.*"
	if !strings.Contains(race, pattern) {
		t.Errorf("the race job must skip %s (tsc is pinned in the generated job): %q", pattern, race)
	}
	if !strings.Contains(generated, pattern) {
		t.Errorf("the generated job must run %s: %q", pattern, generated)
	}
	runJob, _, _ := strings.Cut(generated, ":")
	if runJob == "" || runJob != install || runJob != require {
		t.Errorf("the job that runs the TypeScript tests (%q) must install the pinned tsc (%q) and set FORGE_TEST_REQUIRE_TSC (%q)", runJob, install, require)
	}
}
