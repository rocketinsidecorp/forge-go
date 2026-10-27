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
		violations := append(allowlistViolations("t.yml", text), yamlSubsetViolations("t.yml", text)...)
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
		violations := append(allowlistViolations("t.yml", text), yamlSubsetViolations("t.yml", text)...)
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
		// A decoy inside a block scalar cannot balance a hidden key.
		"steps:\n  - run: |\n      uses: actions/cache@c # v4\n  - \"u\\x73es\": evil/tool@main\n",
		"steps:\n  - uses: actions/checkout@c\r  - uses: evil/tool@main\n",
	} {
		if len(parserViolations("t.yml", rejected)) == 0 {
			t.Errorf("accepted: %q", rejected)
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

func TestYAMLSubsetRefusesWhatALineScannerCouldMisread(t *testing.T) {
	t.Parallel()
	decoy := "      - uses: actions/checkout@SHA # v7.0.1"
	for _, rejected := range []string{
		decoy + "\r      - uses: evil/tool@main\n",
		decoy + "\u0085      - uses: evil/tool@main\n",
		decoy + "\u2028      - uses: evil/tool@main\n",
		decoy + "\u2029      - uses: evil/tool@main\n",
		decoy + "\n\uFEFF      - uses: evil/tool@main\n",
		"    steps: !!seq [ {\"u\\x73es\": evil/tool@main} ]\n",
		"    steps: &s [ {uses: actions/checkout@SHA} ]\n",
		"    steps: *s\n",
		"      - <<: *base\n",
		"      - name: x\n        with: !!map {a: b}\n",
		"      - with: {a: &x b}\n",
		"      - ? uses\n        : evil/tool@main\n",
		"    steps: [\n      {uses: evil/tool@main} ]\n",
		"    steps: [ {name: x},\n      uses: evil/tool@main ]\n",
		"      - name: \"x\n        uses: evil/tool@main\"\n",
		"      - \"u\\x73es\": evil/tool@main\n",
	} {
		if text := snippet(rejected); len(yamlSubsetViolations("t.yml", text)) == 0 {
			t.Errorf("accepted: %q", text)
		}
	}
	for _, accepted := range []string{
		"\uFEFFname: CI\non:\n  push:\n    branches: [main]\n    tags: [\"v*\"]\npermissions: {}\n",
		"jobs:\n  x:\n    if: ${{ !cancelled() }}\n    steps:\n      - run: |\n          case \"$x\" in\n            *) echo \"!\" & wait ;;\n          esac\n          # deeper comment\n      - uses: actions/checkout@SHA # v7.0.1\n",
		// A plain scalar continued on deeper lines is text, `&&` and `!` too.
		"    if: github.event_name == 'push'\n      && !cancelled()\n    runs-on: ubuntu-latest\n",
		"      - run: echo \"a #b\" && echo it's\n      - name: 'it''s'\n        with: {a: b, c: [d, e]}\n",
		"    if: >-\n      github.event_name == 'push'\n      && !cancelled()\n",
		"on:\r\n  push:\r\n",
	} {
		if violations := yamlSubsetViolations("t.yml", snippet(accepted)); len(violations) != 0 {
			t.Errorf("rejected: %v", violations)
		}
	}
}

func TestTreeRulesReadCheckoutsAndPermissionsAsYAMLDoes(t *testing.T) {
	t.Parallel()
	for _, rejected := range []string{
		// A name that spells the setting.
		"steps:\n  - name: \"x, persist-credentials: false\"\n    uses: actions/checkout@SHA # v7.0.1\n",
		// The setting in block-scalar text.
		"steps:\n  - uses: actions/checkout@SHA # v7.0.1\n    with:\n      fetch-depth: 0\n  - run: |\n      persist-credentials: false\n",
		// One flow line, two steps, one setting.
		"steps: [{uses: actions/checkout@SHA, with: {persist-credentials: false}}, {uses: Actions/Checkout@SHA}]\n",
		"steps:\n  - uses: actions/checkout@SHA # v7.0.1\n    with:\n      persist-credentials: \"true\"\n",
		// A quoted `#` hiding a write scope.
		"permissions: {a: '#', contents: write}\n",
		"permissions: write-all\n",
		"jobs:\n  a:\n    permissions:\n      checks: write\n",
	} {
		if text := snippet(rejected); len(treeViolations("t.yml", text, false)) == 0 {
			t.Errorf("accepted: %s", text)
		}
	}
	release := "permissions: {}\njobs:\n  a:\n    permissions:\n      contents: read\n      packages: write\n      id-token: write\n"
	if violations := treeViolations("release.yml", release, true); len(violations) != 0 {
		t.Errorf("rejected: %v", violations)
	}
	if len(treeViolations("ci.yml", release, false)) == 0 {
		t.Error("a write scope outside the release workflow was accepted")
	}
}

func TestGatedTemplatesUseOnlySimpleDirectives(t *testing.T) {
	t.Parallel()
	for _, rejected := range []string{
		// A directive that renders a step the template text does not show.
		"steps:\n{{\"      - \\x75ses: evil/act@main\"}}\n",
		"steps:\n{{printf \"%s\" .Name}}\n",
		"{{print .Name}}\n",
		"{{template \"x\"}}\n",
		"{{define \"x\"}}a{{end}}\n",
		"{{with .DB}}{{.Image}}{{end}}\n",
		"{{$x := .Name}}{{$x}}\n",
		"{{if eq .Name \"x\"}}a{{end}}\n",
		"{{index .DB.CIEnv 0}}\n",
		"{{.Name | html}}\n",
	} {
		if len(templateDirectiveViolations("t.yml.tmpl", rejected)) == 0 {
			t.Errorf("accepted: %q", rejected)
		}
	}
	accepted := "{{- if and .DB .DB.Server}}\n  {{.DB.Service}}:\n{{- range .DB.CIEnv}}\n    {{index . 0}}: {{index . 1}}\n{{- end}}\n{{- else}}\n{{- end}}\n{{if not .DB}}x{{end}}\n"
	if violations := templateDirectiveViolations("t.yml.tmpl", accepted); len(violations) != 0 {
		t.Errorf("rejected: %v", violations)
	}
}

// A reader that takes a key's first value and one that takes its last would
// disagree, so no mapping repeats a key, in any case.
func TestTreeRulesRefuseDuplicateKeysInAnyCase(t *testing.T) {
	t.Parallel()
	for _, rejected := range []string{
		"steps:\n  - uses: actions/checkout@SHA # v7.0.1\n    with:\n      persist-credentials: false\n      Persist-Credentials: true\n",
		"steps:\n  - uses: actions/checkout@SHA # v7.0.1\n    USES: evil/act@main\n",
		"permissions:\n  contents: read\nPermissions:\n  contents: write\n",
		"jobs:\n  a:\n    env: {A: x, a: y}\n",
	} {
		text := snippet(rejected)
		if _, err := yamlDocuments(text, false); err == nil || !strings.Contains(err.Error(), "appears twice") {
			t.Errorf("parsed: %s (%v)", text, err)
		}
		if len(treeViolations("t.yml", text, false)) == 0 {
			t.Errorf("the tree rules accepted: %s", text)
		}
		if len(parserViolations("t.yml", text)) == 0 {
			t.Errorf("the parser check accepted: %s", text)
		}
	}
	// The same key in two different mappings is fine.
	if _, err := yamlDocuments("jobs:\n  a:\n    env: {A: x}\n  b:\n    env: {A: y}\n", false); err != nil {
		t.Error(err)
	}
}

// Every redirecting variable is refused in an env: block at any level, in
// any case, and in run text; GOTOOLCHAIN passes only pinned; a $GITHUB_ENV
// write is refused whatever it sets.
func TestEnvironmentRuleRejectsRedirectingVariables(t *testing.T) {
	t.Parallel()
	for _, rejected := range []string{
		"env:\n  GOFLAGS: -mod=mod\n",
		"jobs:\n  a:\n    env:\n      goproxy: https://evil.example\n",
		"steps:\n  - run: go build ./...\n    env:\n      GONOSUMDB: '*'\n",
		"env: {GONOSUMCHECK: \"1\"}\n",
		"env:\n  GOSUMDB: \"off\"\n",
		"env:\n  GOINSECURE: '*'\n",
		"env:\n  GOPRIVATE: evil.example\n",
		"env:\n  GONOPROXY: '*'\n",
		"env:\n  GOENV: ./go.env\n",
		"env:\n  GOTOOLCHAIN: auto\n",
		"env:\n  GOTOOLCHAIN: go1.25.3+auto\n",
		"env:\n  GOTOOLCHAIN: go1.25\n",
		"steps:\n  - run: GOTOOLCHAIN=$V go test ./...\n",
		"steps:\n  - run: go env -w GOPROXY=https://evil.example\n",
		"steps:\n  - run: export GOFLAGS=-insecure\n",
		"steps:\n  - run: echo \"GOPROXY=https://evil.example\" >> \"$GITHUB_ENV\"\n",
		"steps:\n  - run: echo \"A=b\" >> $GITHUB_ENV\n",
		"steps:\n  - run: |\n      printf '%s\\n' X=1 >> \"${github_env}\"\n",
		// Round 6: the go env file moved through XDG_CONFIG_HOME or HOME,
		// the toolchain, module cache and cache program, and the deprecated
		// workflow commands with their switch.
		"steps:\n  - run: XDG_CONFIG_HOME=\"$PWD/.ci\" go mod download\n",
		"env:\n  XDG_CONFIG_HOME: ${{ github.workspace }}/.ci\n",
		"env:\n  HOME: ${{ github.workspace }}\n",
		"env: {\"HOME\": /tmp/ci}\n",
		"steps:\n  - run: HOME=\"$PWD\" go mod download\n",
		"steps:\n  - run: env HOME=/tmp/ci go build ./...\n",
		"env:\n  GOCACHEPROG: /tmp/prog\n",
		"env:\n  GOROOT: /tmp/go\n",
		"env:\n  GOMODCACHE: /tmp/mods\n",
		"env:\n  GOPATH: /tmp/gopath\n",
		"env:\n  ACTIONS_ALLOW_UNSECURE_COMMANDS: \"true\"\n",
		"steps:\n  - run: echo \"::set-env name=GOFLAGS::-mod=mod\"\n",
		"steps:\n  - run: echo '::add-path::/tmp/bin'\n",
	} {
		if text := snippet(rejected); len(environmentViolations("t.yml", text)) == 0 {
			t.Errorf("accepted: %s", text)
		}
	}
	for _, accepted := range []string{
		"env:\n  GOTOOLCHAIN: local\n  CGO_ENABLED: \"0\"\n",
		"env:\n  GOTOOLCHAIN: go1.25.3\n",
		"steps:\n  - run: GOTOOLCHAIN=local go test ./...\n",
		"env:\n  FORGE_GOPROXYISH: x\n",
		"steps:\n  - run: echo \"$HOME/bin\" >> \"$GITHUB_PATH\"\n",
		"steps:\n  - run: ls \"${HOME}/.cache\" ~/.config\n",
	} {
		if violations := environmentViolations("t.yml", snippet(accepted)); len(violations) != 0 {
			t.Errorf("rejected: %v", violations)
		}
	}
}

// A YAML escape in a double-quoted scalar spells text the rules never read
// (`"g\x6f env -w"` is `go env -w`), so no double-quoted scalar, key or
// value, holds a backslash.
func TestYAMLSubsetRefusesEscapesInDoubleQuotedValues(t *testing.T) {
	t.Parallel()
	for _, rejected := range []string{
		"steps:\n  - run: \"g\\x6f env -w GOPR\\x4fXY=https://evil.example\"\n",
		"steps:\n  - run: \"echo GOFL\\x41GS=-mod=mod >> $GITHUB_\\x45NV\"\n",
		"steps: [{run: \"g\\u006f install evil@latest\"}]\n",
		"env:\n  A: \"\\\"quoted\\\"\"\n",
		"\"u\\x73es\": evil/act@main\n",
	} {
		if len(yamlSubsetViolations("t.yml", rejected)) == 0 {
			t.Errorf("accepted: %s", rejected)
		}
	}
	for _, accepted := range []string{
		// Plain and single-quoted scalars and block text have no escapes.
		"steps:\n  - run: grep -r \"^v\\d\" .\n",
		"steps:\n  - run: 'echo \\x61'\n",
		"steps:\n  - run: |\n      printf \"a\\n\"\n",
		"env:\n  A: \"no escape\"\n",
	} {
		if violations := yamlSubsetViolations("t.yml", accepted); len(violations) != 0 {
			t.Errorf("rejected: %v", violations)
		}
	}
}

// TestWriteScopesBelongToTheFrameworkRelease: `contents` and `attestations`
// are writable only in the framework's own release workflow, never in the
// generated application's or in another workflow.
func TestWriteScopesBelongToTheFrameworkRelease(t *testing.T) {
	t.Parallel()
	workflow := "permissions: {}\njobs:\n  a:\n    permissions:\n      contents: write\n      attestations: write\n      id-token: write\n"
	if violations := treeViolations(frameworkReleasePath, workflow, true); len(violations) != 0 {
		t.Errorf("rejected in the framework release: %v", violations)
	}
	for _, path := range []string{"internal/scaffold/templates/github/release.yml", "generated .github/workflows/release.yml", ".github/workflows/ci.yml"} {
		if len(treeViolations(path, workflow, filepath.Base(path) == "release.yml")) == 0 {
			t.Errorf("%s: contents: write and attestations: write were accepted", path)
		}
	}
	for _, scope := range []string{"actions", "checks", "deployments", "packages", "pull-requests", "security-events", "statuses"} {
		text := "permissions: {}\njobs:\n  a:\n    permissions:\n      " + scope + ": write\n"
		if scope == "packages" {
			continue // the image job's scope
		}
		if len(treeViolations(frameworkReleasePath, text, true)) == 0 {
			t.Errorf("%s: write was accepted in the release workflow", scope)
		}
	}
}

func TestReleasePermissionsRuleRejectsDrift(t *testing.T) {
	t.Parallel()
	good := "permissions: {}\njobs:\n  verify:\n    permissions:\n      contents: read\n      actions: read\n  images:\n    permissions:\n      contents: read\n      packages: write\n      id-token: write\n  sbom:\n    permissions:\n      contents: read\n  binaries:\n    permissions:\n      contents: read\n      id-token: write\n      attestations: write\n  draft:\n    permissions:\n      contents: write\n"
	check := func(text string) []string {
		documents, err := yamlDocuments(text, false)
		if err != nil {
			t.Fatal(err)
		}
		return releasePermissionViolations("release.yml", documents)
	}
	if violations := check(good); len(violations) != 0 {
		t.Fatalf("rejected the expected permissions: %v", violations)
	}
	for name, text := range map[string]string{
		"top-level permissions":      strings.Replace(good, "permissions: {}\njobs", "permissions:\n  contents: read\njobs", 1),
		"no top-level permissions":   strings.Replace(good, "permissions: {}\n", "", 1),
		"the draft job also signs":   strings.Replace(good, "  draft:\n    permissions:\n      contents: write\n", "  draft:\n    permissions:\n      contents: write\n      id-token: write\n", 1),
		"the binary job writes":      strings.Replace(good, "      attestations: write\n", "      attestations: write\n      packages: write\n", 1),
		"the binary job cannot sign": strings.Replace(good, "      id-token: write\n      attestations: write\n", "      attestations: write\n", 1),
		"verify cannot read actions": strings.Replace(good, "      actions: read\n", "", 1),
		"verify writes actions":      strings.Replace(good, "      actions: read\n", "      actions: write\n", 1),
		"verify reads checks":        strings.Replace(good, "      actions: read\n", "      actions: read\n      checks: read\n", 1),
		"a new job":                  good + "  extra:\n    permissions:\n      contents: read\n",
		"a job without permissions":  strings.Replace(good, "  sbom:\n    permissions:\n      contents: read\n", "  sbom:\n    runs-on: x\n", 1),
		"a job is gone":              strings.Replace(good, "  draft:\n    permissions:\n      contents: write\n", "", 1),
	} {
		if len(check(text)) == 0 {
			t.Errorf("accepted: %s", name)
		}
	}
}

func TestBinaryReleaseRuleRejectsAMissingGuarantee(t *testing.T) {
	t.Parallel()
	workflow := readText(t, frameworkReleasePath)
	draft := readText(t, filepath.Join("scripts", "release-draft.sh"))
	if violations := binaryReleaseViolations(frameworkReleasePath, workflow, draft); len(violations) != 0 {
		t.Fatalf("the release is rejected: %v", violations)
	}
	for _, step := range binaryReleaseSteps {
		if len(binaryReleaseViolations(frameworkReleasePath, strings.Replace(workflow, step, "echo skipped", 1), draft)) == 0 {
			t.Errorf("a release without %q was accepted", step)
		}
	}
	for _, flag := range []string{"--draft", "--verify-tag", "--generate-notes"} {
		if len(binaryReleaseViolations(frameworkReleasePath, workflow, strings.ReplaceAll(draft, flag, ""))) == 0 {
			t.Errorf("a draft script without %q was accepted", flag)
		}
	}
	for _, publishing := range []string{"gh release edit v1 --draft=false", "gh release create v1 --latest"} {
		if len(binaryReleaseViolations(frameworkReleasePath, workflow, draft+"\n"+publishing+"\n")) == 0 {
			t.Errorf("a script that runs %q was accepted", publishing)
		}
	}
}

func TestBinaryReleaseRuleReadsCodeNotComments(t *testing.T) {
	t.Parallel()
	workflow := readText(t, frameworkReleasePath)
	draft := readText(t, filepath.Join("scripts", "release-draft.sh"))
	for _, step := range binaryReleaseSteps {
		// The step survives only as a comment.
		commented := strings.Replace(workflow, step, "echo skipped # "+step, 1)
		if len(binaryReleaseViolations(frameworkReleasePath, commented, draft)) == 0 {
			t.Errorf("a release whose %q is only a comment was accepted", step)
		}
	}
	for _, flag := range []string{"--draft", "--verify-tag", "--generate-notes", "gh release create"} {
		commented := strings.ReplaceAll(draft, flag, "") + "\n# " + flag + "\n"
		if len(binaryReleaseViolations(frameworkReleasePath, workflow, commented)) == 0 {
			t.Errorf("a draft script with %q only in a comment was accepted", flag)
		}
	}
	// A refused command in a comment is not a command.
	if violations := binaryReleaseViolations(frameworkReleasePath, workflow, draft+"\n# gh release edit v1 --draft=false\n"); len(violations) != 0 {
		t.Errorf("a comment was read as a command: %v", violations)
	}
}

func TestReleaseSetupGoRuleRequiresCleanCaches(t *testing.T) {
	t.Parallel()
	workflow := readText(t, frameworkReleasePath)
	if violations := releaseSetupGoViolations(frameworkReleasePath, workflow); len(violations) != 0 {
		t.Fatalf("the release is rejected: %v", violations)
	}
	if n := strings.Count(workflow, "          cache: false\n"); n < 2 {
		t.Fatalf("want cache: false on the sbom and binaries jobs, found %d", n)
	}
	for _, replacement := range []string{"", "          cache: true\n"} {
		// Each setup-go step in turn loses `cache: false`.
		for index := 0; index < strings.Count(workflow, "          cache: false\n"); index++ {
			mutated, seen := "", 0
			for _, part := range strings.SplitAfter(workflow, "          cache: false\n") {
				if strings.HasSuffix(part, "          cache: false\n") {
					if seen == index {
						part = strings.TrimSuffix(part, "          cache: false\n") + replacement
					}
					seen++
				}
				mutated += part
			}
			if len(releaseSetupGoViolations(frameworkReleasePath, mutated)) == 0 {
				t.Errorf("setup-go number %d with %q was accepted", index, replacement)
			}
		}
	}
}

func TestInstallRefRule(t *testing.T) {
	t.Parallel()
	for _, rejected := range []string{
		"go install github.com/rocketinsidecorp/forge-go/cmd/forge@v0.1.0",
		"`go install github.com/rocketinsidecorp/forge-go/cmd/forge-proxy@v1.2.3-rc.1`",
		"go install \"github.com/rocketinsidecorp/forge-go/cmd/forge@v0.0.0\"",
		"go install github.com/rocketinsidecorp/forge-go/cmd/forge@main",
		"go install github.com/rocketinsidecorp/forge-go/cmd/forge@0.1.0",
	} {
		if len(installRefViolations("d.md", rejected)) == 0 {
			t.Errorf("accepted: %q", rejected)
		}
	}
	for _, accepted := range []string{
		"go install github.com/rocketinsidecorp/forge-go/cmd/forge@latest",
		"go install github.com/rocketinsidecorp/forge-go/cmd/forge@vX.Y.Z",
		"go install \"github.com/rocketinsidecorp/forge-go/cmd/forge@$VERSION\"",
		"go get github.com/rocketinsidecorp/forge-go@v0.1.0",
	} {
		if violations := installRefViolations("d.md", accepted); len(violations) != 0 {
			t.Errorf("rejected %q: %v", accepted, violations)
		}
	}
}

func TestPipeToShellRule(t *testing.T) {
	t.Parallel()
	for _, rejected := range []string{
		"curl -fsSL https://example.com/install.sh | sh",
		"curl -fsSL https://example.com/install.sh | bash -s -- --yes",
		"curl https://example.com/i | sudo bash",
		"wget -qO- https://example.com/i | sudo -E sh",
		"wget -qO- https://example.com/i | /bin/sh",
		"CURL -sSf https://example.com | ZSH",
		"bash <(curl -s https://example.com/i)",
		"sh -c \"$(curl -fsSL https://example.com/i)\"",
		"curl -sSf https://sh.rustup.rs | sh",
	} {
		if !pipeToShell.MatchString(rejected) {
			t.Errorf("accepted: %q", rejected)
		}
	}
	for _, accepted := range []string{
		"curl -s http://127.0.0.1:8080/v1/status | jq .",
		"curl -s http://127.0.0.1:8080/health | head",
		"cosign verify-blob --bundle b checksums.txt && sha256sum -c checksums.txt",
		"go install github.com/rocketinsidecorp/forge-go/cmd/forge@v0.1.0",
		"curl -sSLO https://example.com/forge.tar.gz",
		"forge new shop --auth | tee log",
	} {
		if pipeToShell.MatchString(accepted) {
			t.Errorf("rejected: %q", accepted)
		}
	}
}
