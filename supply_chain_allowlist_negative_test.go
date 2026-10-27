package forge_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

// D8: every gated workflow stays inside the closed allowlists (the rendered
// generated files are checked by TestGeneratedWorkflowsPassTheGate), and
// every listed runner-file or expression line is still in use.
func TestGatedFilesStayInsideTheClosedAllowlists(t *testing.T) {
	t.Parallel()
	workflows := workflowFiles(t)
	for _, path := range append(workflows, localActionFiles(t, workflows)...) {
		if strings.HasSuffix(path, ".tmpl") {
			continue
		}
		for _, violation := range closedListViolations(filepath.ToSlash(path), readText(t, path)) {
			t.Error(violation)
		}
	}
	for _, entry := range append(slices.Clone(runnerFileLines), expressionLines...) {
		file := entry[0]
		if file == "generated .github/workflows/release.yml" {
			file = "internal/scaffold/templates/github/release.yml"
		}
		used := false
		for _, line := range strings.Split(readText(t, filepath.FromSlash(file)), "\n") {
			used = used || strings.TrimSpace(line) == entry[1]
		}
		if !used {
			t.Errorf("%s: the listed line is no longer used: %s", entry[0], entry[1])
		}
	}
}

// Round-7 repros (A, C, E and the expression and input notes), each refused
// by the closed allowlists.
func TestClosedAllowlistsRefuseEveryRedirect(t *testing.T) {
	t.Parallel()
	for _, rejected := range []string{
		// A: runner files, PATH and shell startup variables.
		"steps:\n  - run: echo /tmp/evil >> \"$GITHUB_PATH\"\n",
		"steps:\n  - run: echo /tmp/evil | tee -a \"${GITHUB_PATH}\"\n",
		"steps:\n  - run: echo x=1 > $GITHUB_OUTPUT\n",
		"steps:\n  - run: echo GOFLAGS=-mod=mod >> $GITHUB_ENV\n",
		"env:\n  PATH: /tmp/evil:/usr/bin\n",
		"steps:\n  - run: PATH=/tmp/evil:$PATH go build ./...\n",
		"env:\n  BASH_ENV: ./ci/env.sh\n",
		"env:\n  ENV: ./ci/env.sh\n",
		"env:\n  LD_PRELOAD: /tmp/evil.so\n",
		"env:\n  LD_LIBRARY_PATH: /tmp/evil\n",
		// C: cgo, workspaces, go env -w and the go command's flags.
		"env:\n  CC: /tmp/evil-cc\n",
		"env:\n  CXX: /tmp/evil-cxx\n",
		"env:\n  CGO_CFLAGS: -fplugin=/tmp/evil.so\n",
		"env:\n  CGO_LDFLAGS: -fuse-ld=/tmp/evil\n",
		"env:\n  CGO_ENABLED: \"1\"\n",
		"env:\n  GOWORK: ./ci/go.work\n",
		"steps:\n  - run: go env -w GOPROXY=https://evil.example\n",
		"steps:\n  - run: go build -toolexec /tmp/evil ./...\n",
		"steps:\n  - run: go test -exec=/tmp/evil ./...\n",
		"steps:\n  - run: go build -modfile=/tmp/go.mod ./...\n",
		"steps:\n  - run: go build --overlay /tmp/overlay.json ./...\n",
		"steps:\n  - run: go build -ldflags \"-X main.version=1 -extld=/tmp/evil\" ./...\n",
		"steps:\n  - run: go build -gcflags=all=-N ./...\n",
		"steps:\n  - run: GOROOT=/tmp/go go build\n",
		// E: HOME in every assignment form.
		"steps:\n  - run: HOME+=/x go build\n",
		"steps:\n  - run: read HOME < /tmp/h\n",
		"steps:\n  - run: for HOME in /tmp/h; do go build; done\n",
		"steps:\n  - run: printf -v HOME /tmp/h\n",
		"steps:\n  - run: echo ${HOME:=/tmp/h}\n",
		"steps:\n  - run: export HOME\n",
		"steps:\n  - run: declare -x HOME=/tmp/h\n",
		"steps:\n  - run: select HOME in a; do :; done\n",
		"steps:\n  - run: exec {HOME}>/tmp/h\n",
		"steps:\n  - run: ((HOME = 1))\n",
		// Expressions that build names or structure, whole-expression env:
		// and with:, and other shells.
		"env:\n  A: ${{ format('GO{0}', 'PROXY') }}\n",
		"jobs:\n  a:\n    env: ${{ fromJSON(vars.ENV) }}\n",
		"steps:\n  - uses: actions/cache@SHA # v6.1.0\n    with: ${{ fromJSON(vars.WITH) }}\n",
		"steps:\n  - run: echo ${{ join(github.event.commits.*.message, ' ') }}\n",
		"steps:\n  - run: echo '${{ ToJson(github) }}'\n",
		"steps:\n  - run: echo x\n    shell: bash --rcfile /tmp/evil {0}\n",
		// Allowlisted actions asked for latest or an unpinned image.
		"steps:\n  - uses: actions/setup-go@SHA # v7.0.0\n    with:\n      go-version: stable\n",
		"steps:\n  - uses: actions/setup-go@SHA # v7.0.0\n    with:\n      go-version: \"1.27.x\"\n",
		"steps:\n  - uses: actions/setup-go@SHA # v7.0.0\n    with:\n      go-version: ${{ env.GO_VERSION }}\n",
		"steps:\n  - uses: actions/setup-go@SHA # v7.0.0\n    with:\n      go-version-file: go.mod\n      check-latest: true\n",
		"steps:\n  - uses: golangci/golangci-lint-action@SHA # v9.3.0\n    with:\n      version: latest\n",
		"steps:\n  - uses: golangci/golangci-lint-action@SHA # v9.3.0\n",
		"steps:\n  - uses: sigstore/cosign-installer@SHA # v4.1.2\n    with:\n      cosign-release: latest\n",
		"steps:\n  - uses: docker/setup-buildx-action@SHA # v4.4.1\n    with:\n      version: latest\n",
		"steps:\n  - uses: docker/setup-buildx-action@SHA # v4.4.1\n    with:\n      driver-opts: image=moby/buildkit:master\n",
		// Round 8, lead 1: continuations. An assignment target split from
		// its `=`, a runner-file write split across lines, a listed line
		// continued from or into more.
		"steps:\n  - run: |\n      HOME\\\n      =/tmp/h go build\n",
		"steps:\n  - run: |\n      export HO\\\n      ME=/tmp/h\n",
		"steps:\n  - run: |\n      echo /tmp/evil >> \"$GITHUB_\\\n      PATH\"\n",
		"steps:\n  - run: |\n      echo /usr/lib/postgresql/18/bin >> \"$GITHUB_PATH\" \\\n        && export HOME=/tmp/h\n",
		"steps:\n  - run: |\n      true \\\n      echo /usr/lib/postgresql/18/bin >> \"$GITHUB_PATH\"\n",
		// Lead 2: runner files named in a heredoc or a `$( )`.
		"steps:\n  - run: |\n      cat <<EOF >> \"$GITHUB_ENV\"\n      A=1\n      EOF\n",
		"steps:\n  - run: |\n      cat <<EOF\n      $GITHUB_OUTPUT\n      EOF\n",
		"steps:\n  - run: |\n      f=\"$(\n      echo $GITHUB_PATH)\"\n",
		// Lead 3: container and service options, keys and ports.
		"jobs:\n  a:\n    services:\n      db:\n        image: x@sha256:DIGEST\n        options: -e GOPROXY=https://evil.example\n",
		"jobs:\n  a:\n    services:\n      db:\n        image: x@sha256:DIGEST\n        options: --env=A=1\n",
		"jobs:\n  a:\n    services:\n      db:\n        image: x@sha256:DIGEST\n        options: --env-file /tmp/e\n",
		"jobs:\n  a:\n    services:\n      db:\n        image: x@sha256:DIGEST\n        options: -v /:/host\n",
		"jobs:\n  a:\n    services:\n      db:\n        image: x@sha256:DIGEST\n        options: --volume=/:/host\n",
		"jobs:\n  a:\n    services:\n      db:\n        image: x@sha256:DIGEST\n        options: --mount type=bind,src=/,dst=/h\n",
		"jobs:\n  a:\n    services:\n      db:\n        image: x@sha256:DIGEST\n        options: --entrypoint /bin/sh\n",
		"jobs:\n  a:\n    services:\n      db:\n        image: x@sha256:DIGEST\n        options: --privileged\n",
		"jobs:\n  a:\n    services:\n      db:\n        image: x@sha256:DIGEST\n        options: --network host\n",
		"jobs:\n  a:\n    container:\n      image: x@sha256:DIGEST\n      options: --cap-add SYS_ADMIN\n",
		"jobs:\n  a:\n    services:\n      db:\n        image: x@sha256:DIGEST\n        options: --health-cmd \"true\"\n",
		"jobs:\n  a:\n    services:\n      db:\n        image: x@sha256:DIGEST\n        volumes:\n          - /:/host\n",
		"jobs:\n  a:\n    container:\n      image: x@sha256:DIGEST\n      credentials:\n        username: u\n",
		"jobs:\n  a:\n    services:\n      db:\n        image: x@sha256:DIGEST\n        entrypoint: /bin/sh\n",
		"jobs:\n  a:\n    services:\n      db:\n        image: x@sha256:DIGEST\n        ports:\n          - 0.0.0.0:5432:5432\n",
		"jobs:\n  a:\n    container:\n      image: x@sha256:DIGEST\n      env:\n        HOME: /tmp/h\n",
		// Git configuration from the environment.
		"env:\n  GIT_CONFIG_COUNT: \"1\"\n",
		"env:\n  GIT_CONFIG_KEY_0: url.https://evil.example/.insteadOf\n",
		"env:\n  GIT_CONFIG_VALUE_0: https://github.com/\n",
		"steps:\n  - run: GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.sshCommand GIT_CONFIG_VALUE_0=/tmp/evil git fetch\n",
	} {
		text := snippet(rejected)
		if _, err := yamlDocuments(text, false); err != nil {
			t.Errorf("the snippet must parse: %s (%v)", text, err)
			continue
		}
		if len(closedListViolations("t.yml", text)) == 0 {
			t.Errorf("accepted: %s", text)
		}
	}
}

// Round 8: a listed runner-file line passes only as a complete logical
// line, and the listed container options pass.
func TestListedLinesAndOptionsPassOnlyWhole(t *testing.T) {
	t.Parallel()
	ci := ".github/workflows/ci.yml"
	listed := "steps:\n  - name: x\n    run: |\n      sudo true\n      echo /usr/lib/postgresql/18/bin >> \"$GITHUB_PATH\"\n"
	if violations := closedListViolations(ci, listed); len(violations) != 0 {
		t.Errorf("rejected: %v", violations)
	}
	continued := "steps:\n  - name: x\n    run: |\n      sudo true \\\n      echo /usr/lib/postgresql/18/bin >> \"$GITHUB_PATH\"\n"
	if len(closedListViolations(ci, continued)) == 0 {
		t.Error("a listed line continued from another was accepted")
	}
	for _, options := range allowedContainerOptions {
		text := "jobs:\n  a:\n    services:\n      db:\n        image: x@sha256:" + testDigest + "\n        env:\n          POSTGRES_PASSWORD: forge\n        ports:\n          - 5432:5432\n        options: >-\n          " + options + "\n"
		if violations := closedListViolations("ci.yml", text); len(violations) != 0 {
			t.Errorf("rejected: %v", violations)
		}
	}
}

// D8: an unlisted name is refused in every position that sets it.
func TestAnUnlistedVariableIsRefusedWhereverItIsSet(t *testing.T) {
	t.Parallel()
	for _, rejected := range []string{
		"env:\n  UNLISTED: x\n",
		"jobs:\n  a:\n    env:\n      UNLISTED: x\n",
		"steps:\n  - run: x\n    env:\n      UNLISTED: x\n",
		"env: {UNLISTED: x}\n",
		"env:\n  \"UNLISTED\": x\n",
		"env:\n  'UNLISTED': x\n",
		"jobs:\n  a:\n    container:\n      image: x@sha256:DIGEST\n      env:\n        UNLISTED: x\n",
		"jobs:\n  a:\n    services:\n      db:\n        image: x@sha256:DIGEST\n        env:\n          UNLISTED: x\n",
		"env:\n  FORGE_TEST_REQUIRE_DATABASE: \"false\"\n",
		"steps:\n  - run: SYFT_CHECK_FOR_APP_UPDATE=true syft scan\n",
		"steps:\n  - run: UNLISTED=x go build\n",
		"steps:\n  - run: UNLISTED+=x\n",
		"steps:\n  - run: arr[0]=x; UNLISTED[1]=x\n",
		"steps:\n  - run: export UNLISTED=x\n",
		"steps:\n  - run: export UNLISTED\n",
		"steps:\n  - run: env UNLISTED=x make\n",
		"steps:\n  - run: sudo UNLISTED=x make\n",
		"steps:\n  - run: declare UNLISTED\n",
		"steps:\n  - run: typeset UNLISTED\n",
		"steps:\n  - run: readonly UNLISTED=x\n",
		"steps:\n  - run: local UNLISTED\n",
		"steps:\n  - run: read -r UNLISTED\n",
		"steps:\n  - run: for UNLISTED in a; do :; done\n",
		"steps:\n  - run: printf -v UNLISTED x\n",
		"steps:\n  - run: printf -vUNLISTED x\n",
		"steps:\n  - run: echo \"${UNLISTED:=x}\"\n",
		"steps:\n  - run: echo \"${UNLISTED=x}\"\n",
		"steps:\n  - run: let UNLISTED=1\n",
		"steps:\n  - run: ((UNLISTED++))\n",
		"steps:\n  - run: ((++UNLISTED))\n",
		"steps:\n  - run: (( UNLISTED += 1 ))\n",
		"steps:\n  - run: getopts ab UNLISTED\n",
		"steps:\n  - run: coproc UNLISTED { cat; }\n",
		"steps:\n  - run: mapfile UNLISTED < f\n",
		"steps:\n  - run: |\n      go build \\\n        && UNLISTED=x make\n",
	} {
		text := snippet(rejected)
		if _, err := yamlDocuments(text, false); err != nil {
			t.Errorf("the snippet must parse: %s (%v)", text, err)
			continue
		}
		refused := false
		for _, violation := range closedListViolations("t.yml", text) {
			refused = refused || strings.Contains(violation, "UNLISTED") || strings.Contains(violation, "allowedVariables")
		}
		if !refused {
			t.Errorf("accepted: %s", text)
		}
	}
	for _, accepted := range []string{
		"env:\n  FORGE_TEST_REQUIRE_DATABASE: \"true\"\n  FORGE_TEST_DATABASE_URL: postgres://x\n",
		"steps:\n  - run: echo \"$HOME/bin\" \"${HOME}\" \"${HOME:-/root}\" \"$(go env GOROOT)\"\n",
		"steps:\n  - run: go test -race -count=1 -run='^$' -bench=. ./...\n",
		"steps:\n  - run: test -z \"$(find . -name '*.go' -exec gofmt -l {} +)\"\n",
		"steps:\n  - run: go build -ldflags=-s ./... || true\n",
	} {
		if strings.Contains(accepted, "ldflags") {
			// -ldflags=-s passes an option: refused without an allowlist entry.
			if len(closedListViolations("t.yml", snippet(accepted))) == 0 {
				t.Errorf("accepted an unlisted -ldflags option: %s", accepted)
			}
			continue
		}
		if violations := closedListViolations("t.yml", snippet(accepted)); len(violations) != 0 {
			t.Errorf("rejected: %v", violations)
		}
	}
}

// D8: no tool configuration file exists anywhere in the repository,
// templates included (a .tmpl suffix is ignored).
func TestNoToolConfigurationExistsInTheRepository(t *testing.T) {
	t.Parallel()
	if found := toolConfigurationFiles(".", []string{".git"}); len(found) != 0 {
		t.Errorf("tool configuration found: %v", found)
	}
	scratch := t.TempDir()
	for relative, content := range map[string]string{
		"a/go.work":                         "go 1.27\nreplace x => /tmp/evil\n",
		"a/go.work.sum":                     "",
		"b/go.env":                          "",
		"c/.ci/go/env":                      "GOPROXY=https://evil.example\n",
		"d/.cargo/config.toml":              "",
		"d/.cargo/config":                   "",
		"e/rust-toolchain.toml":             "[toolchain]\npath = \"/opt/evil\"\n",
		"ok/rust-toolchain.toml":            "[toolchain]\nchannel = \"1.98.1\"\n",
		"f/.bashrc":                         "",
		"f/.profile":                        "",
		"f/.bash_profile":                   "",
		"f/.envrc":                          "",
		"templates/go.work.tmpl":            "",
		"templates/.cargo/config.toml.tmpl": "",
		"skipped/go.work":                   "",
	} {
		path := filepath.Join(scratch, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var found []string
	for _, path := range toolConfigurationFiles(scratch, []string{filepath.Join(scratch, "skipped")}) {
		relative, err := filepath.Rel(scratch, path)
		if err != nil {
			t.Fatal(err)
		}
		found = append(found, filepath.ToSlash(relative))
	}
	want := []string{
		"a/go.work", "a/go.work.sum", "b/go.env", "c/.ci/go/env", "d/.cargo/config", "d/.cargo/config.toml",
		"e/rust-toolchain.toml", "f/.bash_profile", "f/.bashrc", "f/.envrc", "f/.profile",
		"templates/.cargo/config.toml.tmpl", "templates/go.work.tmpl",
	}
	if !slices.Equal(found, want) {
		t.Errorf("found %v, want %v", found, want)
	}
}

// C (round 7): a template that wrote a go.work, or any other tool
// configuration file, anywhere in a generated application is found by the
// scan the rendered gate runs on every generated application.
func TestAGeneratedToolConfigurationIsFoundAnywhereInTheApplication(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "gateapp")
	if _, err := scaffold.Generate(scaffold.Options{Name: "gateapp", Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if found := toolConfigurationFiles(dir, nil); len(found) != 0 {
		t.Fatalf("a clean application holds %v", found)
	}
	for _, relative := range []string{"go.work", "internal/go.work.sum", ".config/go/env", ".envrc"} {
		path := filepath.Join(dir, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("go 1.27\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(toolConfigurationFiles(dir, nil), path) {
			t.Errorf("%s was not found", relative)
		}
	}
}

// A `run:` script never interpolates an expression: GitHub pastes the value
// into the shell text, so an attacker-chosen tag, branch name or commit message
// would run as code. The safe spelling passes the value through env:.
func TestRunScriptsRefuseInterpolatedExpressions(t *testing.T) {
	t.Parallel()
	for _, rejected := range []string{
		"steps:\n  - run: echo \"${{ github.ref_name }}\"\n",
		"steps:\n  - run: echo '${{ github.event.head_commit.message }}'\n",
		"steps:\n  - run: |\n      set -e\n      echo ${{ github.event.pull_request.title }}\n",
		"steps:\n  - run: >-\n      go run ./cmd/forge new x --database \"${{ matrix.database }}\"\n",
		"steps:\n  - name: x\n    run: echo $\\{{ x }}${{github.head_ref}}\n",
		"steps:\n  - run: echo ${{github.ref_name}}\n",
		"steps:\n  - run: echo ${{ env.A }}\n    shell: bash\n",
	} {
		if len(closedListViolations("w.yml", rejected)) == 0 {
			t.Errorf("accepted: %q", rejected)
		}
	}
	for _, accepted := range []string{
		"steps:\n  - env:\n      NAME: ${{ github.ref_name }}\n    run: echo \"$NAME\"\n",
		"steps:\n  - name: ${{ matrix.image }}\n    run: echo hi\n    working-directory: ${{ runner.temp }}/x\n",
		"steps:\n  - uses: actions/upload-artifact@x\n    with:\n      name: a-${{ github.ref_name }}\n",
		"steps:\n  - if: ${{ github.event_name == 'push' }}\n    run: echo hi\n",
	} {
		if violations := closedListViolations("w.yml", accepted); len(violations) != 0 {
			t.Errorf("rejected %q: %v", accepted, violations)
		}
	}
}
