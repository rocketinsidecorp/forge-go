package forge_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testSHA    = "3d3c42e5aac5ba805825da76410c181273ba90b1"
	testDigest = "74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336"
)

// snippet expands the SHA and DIGEST placeholders of a test snippet.
func snippet(text string) string {
	return strings.NewReplacer("SHA", testSHA, "DIGEST", testDigest).Replace(text)
}

func TestPinRuleRejectsEveryKnownBypass(t *testing.T) {
	t.Parallel()
	for _, rejected := range []string{
		"      - uses: actions/checkout@v7 # v7.0.1",
		"      - uses: actions/checkout@SHA",
		"      - uses: actions/checkout@SHA # latest",
		"      - uses: actions/checkout@SHA\t# main",
		"      - { uses: actions/checkout@v7, with: { persist-credentials: false } } # v7.0.1",
		`      - { "uses": owner/repo@main }`,
		"    steps: [{ name: x, uses: owner/repo@main }]",
		"      - uses: &pin actions/checkout@SHA # v7.0.1",
		"      - uses: *pin",
		"      - 'uses' : owner/repo@main",
		"      - uses: docker://alpine:3.20",
		"      - uses: docker://alpine@sha256:abc",
	} {
		if text := snippet(rejected); len(pinViolations("t.yml", text)) == 0 {
			t.Errorf("accepted: %s", text)
		}
	}
	for _, accepted := range []string{
		"      - uses: actions/checkout@SHA # v7.0.1",
		"      - uses: actions/checkout@SHA\t# v7",
		"      - { uses: actions/checkout@SHA, with: { persist-credentials: false } } # v7.0.1",
		`      - "uses": "owner/repo/path@SHA" # v1.2`,
		"      - uses: docker://alpine@sha256:DIGEST",
		"      - uses: ./.github/actions/tool",
		"        run: echo uses: anything",
	} {
		if violations := pinViolations("t.yml", snippet(accepted)); len(violations) != 0 {
			t.Errorf("rejected: %v", violations)
		}
	}
}

func TestCheckoutRuleRejectsStepsThatKeepCredentials(t *testing.T) {
	t.Parallel()
	for _, rejected := range []string{
		"      - uses: actions/checkout@SHA # v7.0.1\n",
		"      - uses: Actions/Checkout@SHA # v7.0.1\n        with:\n          fetch-depth: 0\n",
		"      - { uses: ACTIONS/checkout@SHA } # v7.0.1\n",
		"      - uses: actions/checkout@SHA # v7.0.1\n        with:\n          persist-credentials: true\n",
		// The setting belongs to another step: counting would accept it.
		"      - uses: actions/checkout@SHA # v7.0.1\n      - uses: owner/repo@SHA # v1\n        with:\n          persist-credentials: false\n",
		"      - name: checkout\n        uses: actions/checkout@SHA # v7.0.1\n      - with:\n          persist-credentials: false\n",
	} {
		if text := snippet(rejected); len(checkoutViolations("t.yml", text)) == 0 {
			t.Errorf("accepted: %s", text)
		}
	}
	for _, accepted := range []string{
		"      - uses: actions/checkout@SHA # v7.0.1\n        with:\n          persist-credentials: false\n",
		"      - name: checkout\n        uses: Actions/Checkout@SHA # v7.0.1\n        with:\n          fetch-depth: 0\n          persist-credentials: \"false\"\n      - run: true\n",
		"      - { uses: actions/checkout@SHA, with: { persist-credentials: false } } # v7.0.1\n",
	} {
		if violations := checkoutViolations("t.yml", snippet(accepted)); len(violations) != 0 {
			t.Errorf("rejected: %v", violations)
		}
	}
}

func TestContainerRuleReadsOnlyContainers(t *testing.T) {
	t.Parallel()
	for _, rejected := range []string{
		"    services:\n      db:\n        image: postgres:18\n",
		"    services:\n      db:\n        image: \"postgres:18\" # 18.6\n",
		"    container: node:24\n",
		"    container:\n      image: node:24\n      options: --cpus 1\n",
		"    services: { db: { image: postgres:18 } }\n",
		"    services:\n      db: { image: postgres:18, ports: [\"5432:5432\"] }\n",
		"    container: { image: node:24 }\n",
	} {
		if len(containerViolations("t.yml", rejected)) == 0 {
			t.Errorf("accepted: %s", rejected)
		}
	}
	for _, accepted := range []string{
		snippet("    services:\n      db:\n        image: postgres:18@sha256:DIGEST\n"),
		snippet("    container:\n      image: node@sha256:DIGEST\n"),
		// An action input and a matrix entry are not containers.
		"    steps:\n      - uses: owner/repo@x\n        with:\n          image: forge-proxy\n",
		"      - { uses: owner/repo@x, with: { image: forge-proxy } }\n",
		"      matrix:\n        include:\n          - image: forge-proxy\n            dockerfile: Dockerfile\n",
	} {
		if violations := containerViolations("t.yml", accepted); len(violations) != 0 {
			t.Errorf("rejected: %v", violations)
		}
	}
}

func TestLocalActionsAreFound(t *testing.T) {
	t.Parallel()
	got := localActions("      - uses: ./.github/actions/tool/\n      - { uses: ./.github/workflows/reuse.yml }\n      - uses: owner/repo@x\n")
	if strings.Join(got, ",") != ".github/actions/tool,.github/workflows/reuse.yml" {
		t.Errorf("got %v", got)
	}
}

func TestReleaseRuleRequiresTheModuleVersionCheck(t *testing.T) {
	t.Parallel()
	path := filepath.Join(".github", "workflows", "release.yml")
	text := readText(t, path)
	if violations := releaseViolations(path, text); len(violations) != 0 {
		t.Fatal(violations)
	}
	check := `if [ "$base$want" != "$module" ]; then`
	if len(releaseViolations(path, strings.Replace(text, check, "if false; then", 1))) == 0 {
		t.Error("a release without the module version check was accepted")
	}
}

func TestLocalActionTreeRefusesMissingTargets(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"ci.yml": "      - uses: ./.github/actions/tool\n" +
			"  call:\n    uses: ./.github/workflows/reuse.yml\n" +
			"  gone:\n    uses: ./.github/workflows/missing.yml\n" +
			"  outside:\n    uses: ./elsewhere/flow.yml\n" +
			"      - uses: ./.github/actions/absent\n",
		".github/actions/tool/action.yml":    "    - uses: ./.github/actions/nested\n",
		".github/actions/nested/action.yaml": "    - run: true\n",
		".github/workflows/reuse.yml":        "on: workflow_call\n",
		// A directory named like a workflow is not one.
		".github/workflows/missing.yml/": "",
	}
	read := func(path string) string { return files[path] }
	isFile := func(path string) bool {
		_, ok := files[filepath.ToSlash(path)]
		return ok
	}
	actions, problems := localActionTree([]string{"ci.yml"}, read, isFile)
	got := map[string]bool{}
	for _, action := range actions {
		got[filepath.ToSlash(action)] = true
	}
	if len(actions) != 2 || !got[".github/actions/tool/action.yml"] || !got[".github/actions/nested/action.yaml"] {
		t.Errorf("actions %v", actions)
	}
	want := []string{
		"./.github/actions/absent has no action.yml",
		"./.github/workflows/missing.yml is not a workflow of .github/workflows",
		"./elsewhere/flow.yml is not a workflow of .github/workflows",
	}
	for _, problem := range want {
		if !strings.Contains(strings.Join(problems, "\n"), problem) {
			t.Errorf("missing problem %q in %v", problem, problems)
		}
	}
	if len(problems) != len(want) {
		t.Errorf("problems %v", problems)
	}
}

func TestLocalActionTreeRefusesTraversalAndDirectories(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflows := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(filepath.Join(workflows, "dir.yml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflows, "real.yml"), []byte("on: workflow_call\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "outside.yml"), []byte("on: workflow_call\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"ci.yml": "" +
		"  a:\n    uses: ./.github/workflows/real.yml\n" +
		"  b:\n    uses: ./.github/workflows/dir.yml\n" +
		"  c:\n    uses: ./.github/workflows/../../outside.yml\n"}
	read := func(path string) string { return files[path] }
	isFile := func(path string) bool {
		info, err := os.Stat(filepath.Join(root, path))
		return err == nil && info.Mode().IsRegular()
	}
	_, problems := localActionTree([]string{"ci.yml"}, read, isFile)
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"./.github/workflows/dir.yml", "./.github/workflows/../../outside.yml"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s was accepted: %v", want, problems)
		}
	}
	if len(problems) != 2 {
		t.Errorf("problems %v", problems)
	}
}

func TestAllowlistRejectsUnlistedActionsHoweverPinned(t *testing.T) {
	t.Parallel()
	for _, rejected := range []string{
		"      - uses: taiki-e/install-action@SHA # v2.62.1",
		"      - uses: baptiste0928/cargo-install@SHA # v3.3.2",
		"      - { uses: some/setup-tool@SHA } # v1",
		"      - uses: actions/checkout-evil@SHA # v7.0.1",
		"      - uses: docker://alpine@sha256:DIGEST",
		"      - uses: checkout@SHA # v7",
		"      - uses: actions/checkout/../../taiki-e/install-action@SHA # v2",
		"      - uses: actions/checkout/./x@SHA # v7",
		"      - uses: actions/checkout//x@SHA # v7",
		// YAML spellings of the key that a line scanner would not see.
		"      - \"u\\x73es\": taiki-e/install-action@SHA # v2",
		"      - \"use\\s\": taiki-e/install-action@SHA # v2",
		"      - ? uses\n        : taiki-e/install-action@SHA # v2",
		"    steps: [uses: taiki-e/install-action@SHA] # v2",
		"      - &k uses: taiki-e/install-action@SHA # v2",
		"      - *k : taiki-e/install-action@SHA # v2",
		"      - !!str uses: taiki-e/install-action@SHA # v2",
		"    steps: [{name: x, !!str uses: taiki-e/install-action@SHA}] # v2",
	} {
		text := snippet(rejected)
		violations := append(allowlistViolations("t.yml", text), keyPositionViolations("t.yml", text)...)
		if len(violations) == 0 {
			t.Errorf("accepted: %s", text)
		}
	}
	for _, accepted := range []string{
		"      - uses: actions/checkout@SHA # v7.0.1",
		"      - uses: Actions/Setup-Go@SHA # v7.0.0",
		"      - uses: docker/build-push-action/subdir@SHA # v7",
		"      - uses: ./.github/actions/tool",
		"    steps: [uses: actions/checkout@SHA] # v7.0.1",
		// Inside a block scalar, `*`, `&` and `!` are shell, not YAML.
		"      - run: |\n          case \"$x\" in\n            *) echo \"!\" & wait ;;\n          esac\n      - uses: actions/checkout@SHA # v7.0.1",
		"    branches: [main]\n    tags: [\"v*\"]",
	} {
		text := snippet(accepted)
		violations := append(allowlistViolations("t.yml", text), keyPositionViolations("t.yml", text)...)
		if len(violations) != 0 {
			t.Errorf("rejected: %v", violations)
		}
	}
}

func TestParserAndScannerAgreeOnUsesKeys(t *testing.T) {
	t.Parallel()
	for _, rejected := range []string{
		"steps:\n  - \"u\\x73es\": a/b@c\n",
		"steps:\n  - ? uses\n    : a/b@c\n",
		"steps:\n  - &k uses: a/b@c\n  - *k : d/e@f\n",
		"steps:\n  - !!str uses: a/b@c\n",
		"base: &base\n  uses: a/b@c\nsteps:\n  - <<: *base\n",
		"steps: [\n",
	} {
		if len(parserViolations("t.yml", rejected)) == 0 {
			t.Errorf("accepted: %s", rejected)
		}
	}
	for _, accepted := range []string{
		"steps:\n  - uses: a/b@c\n  - { uses: d/e@f }\n  - run: |\n      echo uses\n",
		"steps: [uses: a/b@c]\n",
	} {
		if violations := parserViolations("t.yml", accepted); len(violations) != 0 {
			t.Errorf("rejected: %v", violations)
		}
	}
	// A template's directives are not YAML.
	template := "{{- if .DB}}\n    services:\n      {{.DB.Service}}:\n        image: {{.DB.Image}}\n{{- end}}\n    steps:\n      - uses: a/b@c\n"
	if violations := parserViolations("ci.yml.tmpl", template); len(violations) != 0 {
		t.Errorf("rejected: %v", violations)
	}
}
