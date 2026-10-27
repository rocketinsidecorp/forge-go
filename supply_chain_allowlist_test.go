package forge_test

// Closed allowlists of the supply-chain gate (engineering decision D8,
// docs/SUPPLY_CHAIN.md "Threat model and residual risk"): a gated file sets
// only listed variables, writes the runner's files only on listed lines,
// builds no names from expressions, passes the go command no flag that runs
// another program or swaps its inputs, and passes allowlisted actions only
// exact versions. Each list holds what the gated files use today; growing
// one is a reviewed change to this file. The Go twin of the D8 rules of
// Forge-Rust's crates/forge/tests/gate/rules.rs.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// anyValue marks an allowlisted variable whose value is not fixed.
const anyValue = "\x00any"

// allowedVariables are the variables a gated file may set: an `env:` key at
// any level (a workflow, job, step, container or service) or a shell
// assignment in `run` text. Case matters, as it does on the runner. A name
// whose value a tool reads as flags or a program takes exactly one value.
var allowedVariables = map[string]string{
	// Test configuration the framework's own tests read.
	"FORGE_TEST_POSTGRES_URL":     anyValue,
	"FORGE_TEST_MYSQL_URL":        anyValue,
	"FORGE_TEST_DATABASE_URL":     anyValue,
	"FORGE_TEST_REQUIRE_DATABASE": "true",
	"FORGE_TEST_REQUIRE_SSHD":     "true",
	"FORGE_TEST_REQUIRE_TSC":      "true",
	// Database and Grafana service containers.
	"POSTGRES_USER":          anyValue,
	"POSTGRES_PASSWORD":      anyValue,
	"MYSQL_ROOT_PASSWORD":    anyValue,
	"MARIADB_ROOT_PASSWORD":  anyValue,
	"GRAFANA_ADMIN_PASSWORD": anyValue,
	// Read by the go command and Syft: fixed values.
	"GOARCH":                    "${{ matrix.arch }}",
	"SYFT_CHECK_FOR_APP_UPDATE": "false",
	// Inputs of the workflows' own steps and scripts.
	"PACKAGE":        anyValue,
	"TARGET":         anyValue,
	"FUZZTIME":       anyValue,
	"DEFAULT_BRANCH": anyValue,
	"NAME":           anyValue,
	"IMAGE":          anyValue,
	"DIGEST":         anyValue,
	// The release workflow's binary steps: the attestation bundle to keep
	// with the release, and gh's token (the only secret release.yml may
	// read) and repository for the draft release.
	"BUNDLE":   "${{ steps.attest.outputs.bundle-path }}",
	"GH_TOKEN": "${{ secrets.GITHUB_TOKEN }}",
	"GH_REPO":  "${{ github.repository }}",
	// Shell locals of `run` scripts (and curl's `query=` argument, which
	// reads like an assignment).
	"module":  anyValue,
	"major":   anyValue,
	"want":    anyValue,
	"base":    anyValue,
	"overlay": anyValue,
	"what":    anyValue,
	"source":  anyValue,
	"query":   anyValue,
	"_":       anyValue,
}

// runnerFileLines are the only lines that write the runner's $GITHUB_ENV,
// $GITHUB_PATH or $GITHUB_OUTPUT files, as gated file and trimmed line.
var runnerFileLines = [][2]string{
	{".github/workflows/ci.yml", `echo /usr/lib/postgresql/18/bin >> "$GITHUB_PATH"`},
	{".github/workflows/fuzz.yml", `run: echo "matrix=$(scripts/fuzz.sh -j)" >> "$GITHUB_OUTPUT"`},
	{".github/workflows/fuzz.yml", `run: echo "dir=$(go env GOCACHE)/fuzz" >> "$GITHUB_OUTPUT"`},
	{".github/workflows/release.yml", `run: echo "image=ghcr.io/${GITHUB_REPOSITORY_OWNER,,}/${NAME}" >> "$GITHUB_OUTPUT"`},
	{"internal/scaffold/templates/github/release.yml", `run: echo "image=ghcr.io/${GITHUB_REPOSITORY,,}" >> "$GITHUB_OUTPUT"`},
	{"generated .github/workflows/release.yml", `run: echo "image=ghcr.io/${GITHUB_REPOSITORY,,}" >> "$GITHUB_OUTPUT"`},
}

// expressionLines are the only lines that use an expression function that
// builds text or structure (format, fromJSON, join, toJSON).
var expressionLines = [][2]string{
	{".github/workflows/fuzz.yml", "include: ${{ fromJSON(needs.targets.outputs.matrix) }}"},
}

// allowedShells are the shells a step may name (`shell:`).
var allowedShells = []string{"bash"}

// allowedGoFlags are the exact -gcflags/-ldflags values that pass options:
// none today.
var allowedGoFlags = []string{}

// refusedGoFlags run another program or swap the go command's inputs.
var refusedGoFlags = []string{"-toolexec", "-exec", "-modfile", "-overlay"}

// declaring are the shell words that declare or set the variables named
// after them.
var declaring = []string{"export", "declare", "typeset", "readonly", "local", "read", "mapfile", "readarray", "getopts", "let"}

// buildingFunction is an expression function that builds names or
// structure, in any case.
var buildingFunction = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_])(?:format|fromjson|join|tojson)\s*\(`)

var identifierPrefix = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)

func leadingIdentifier(word string) string {
	return identifierPrefix.FindString(word)
}

// shellComment strips a shell comment: a `#` that starts a word, outside
// quotes.
func shellComment(line string) string {
	quote := byte(0)
	for index := 0; index < len(line); index++ {
		switch character := line[index]; {
		case quote != 0:
			if character == quote {
				quote = 0
			}
		case character == '\'' || character == '"':
			quote = character
		case character == '#' && (index == 0 || line[index-1] == ' ' || line[index-1] == '\t'):
			return line[:index]
		}
	}
	return line
}

// shellLine is one reading of a logical line of a script, and whether it
// spans more than one physical line.
type shellLine struct {
	text      string
	continued bool
}

// shellLines returns every reading of a script's lines the gate analyses:
// the physical lines, the lines joined as bash joins them (backslash-newline
// removed), and joined with a space, each with and without comments
// stripped. A continuation or a comment the gate would misread cannot hide
// text from every reading.
func shellLines(script string) []shellLine {
	seen := map[shellLine]bool{}
	var found []shellLine
	add := func(line shellLine) {
		if !seen[line] {
			seen[line] = true
			found = append(found, line)
		}
	}
	for _, strip := range []bool{true, false} {
		for _, glue := range []string{"none", "", " "} {
			var current *shellLine
			previousContinues := false
			for _, line := range strings.Split(script, "\n") {
				if strip {
					line = shellComment(line)
				}
				code, continues := strings.CutSuffix(strings.TrimRight(line, " \t\r"), `\`)
				if !continues {
					code = line
				}
				switch {
				case glue == "none":
					add(shellLine{code, continues || previousContinues})
				case current != nil && previousContinues:
					current.text += glue + code
					current.continued = true
				default:
					if current != nil {
						add(*current)
					}
					current = &shellLine{text: code}
				}
				previousContinues = continues
			}
			if current != nil {
				add(*current)
			}
		}
	}
	return found
}

// namesRunnerFile reports whether a line names one of the runner's files, in
// any case.
func namesRunnerFile(line string) bool {
	upper := strings.ToUpper(line)
	return strings.Contains(upper, "GITHUB_ENV") || strings.Contains(upper, "GITHUB_PATH") || strings.Contains(upper, "GITHUB_OUTPUT")
}

var shellSeparators = []string{"&&", "||", "$(", ";", "|", "(", ")", "`"}

// shellWords splits a logical line into words, with the shell separators
// as words of their own and quotes and backslashes removed.
func shellWords(code string) []string {
	var spaced strings.Builder
	for rest := code; rest != ""; {
		matched := false
		for _, separator := range shellSeparators {
			if strings.HasPrefix(rest, separator) {
				spaced.WriteString(" " + separator + " ")
				rest = rest[len(separator):]
				matched = true
				break
			}
		}
		if !matched {
			spaced.WriteByte(rest[0])
			rest = rest[1:]
		}
	}
	var words []string
	for _, word := range strings.Fields(spaced.String()) {
		if word = strings.NewReplacer(`'`, "", `"`, "", `\`, "").Replace(word); word != "" {
			words = append(words, word)
		}
	}
	return words
}

func isShellSeparator(word string) bool {
	return slices.Contains(shellSeparators, word)
}

// assignment is a variable a run script sets, with the value of a
// `NAME=value` form.
type assignment struct {
	name     string
	value    string
	hasValue bool
}

// assignments returns every variable a shell script sets, in every form the
// gate knows: `NAME=`, `NAME+=`, `NAME[i]=` (anywhere, `env` and `sudo`
// arguments included), export/declare/typeset/readonly/local/read/mapfile/
// readarray/getopts/let arguments, `printf -v NAME`, `for NAME`, `select
// NAME`, `coproc NAME`, `${NAME:=...}`/`${NAME=...}`, `{NAME}>`
// redirections and arithmetic (`((NAME = 1))`, `((NAME++))`). It reads
// words, not a shell grammar, and so over-approximates.
func assignments(script string) []assignment {
	var found []assignment
	add := func(name string) { found = append(found, assignment{name: name}) }
	for _, reading := range shellLines(script) {
		line := reading.text
		for position := strings.Index(line, "{"); position >= 0; {
			rest := line[position+1:]
			if name := leadingIdentifier(rest); name != "" {
				after := rest[len(name):]
				expansion := position > 0 && line[position-1] == '$'
				if expansion && (strings.HasPrefix(after, ":=") || strings.HasPrefix(after, "=")) ||
					!expansion && strings.HasPrefix(after, "}") && len(after) > 1 && (after[1] == '>' || after[1] == '<') {
					add(name)
				}
			}
			next := strings.Index(rest, "{")
			if next < 0 {
				break
			}
			position += next + 1
		}
		for start := strings.Index(line, "(("); start >= 0; {
			span := line[start+2:]
			if end := strings.Index(span, "))"); end >= 0 {
				span = span[:end]
			}
			for rest := span; ; {
				offset := strings.IndexFunc(rest, func(r rune) bool { return r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' })
				if offset < 0 {
					break
				}
				before := strings.TrimRight(rest[:offset], " \t")
				name := leadingIdentifier(rest[offset:])
				after := strings.TrimLeft(rest[offset+len(name):], " \t")
				operator := strings.HasPrefix(after, "=") && !strings.HasPrefix(after, "==")
				for _, candidate := range []string{"<<=", ">>=", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "++", "--"} {
					operator = operator || strings.HasPrefix(after, candidate)
				}
				if !strings.HasSuffix(before, "$") && (operator || strings.HasSuffix(before, "++") || strings.HasSuffix(before, "--")) {
					add(name)
				}
				rest = rest[offset+len(name):]
			}
			next := strings.Index(line[start+2:], "((")
			if next < 0 {
				break
			}
			start += next + 2
		}
		words := shellWords(line)
		for index, word := range words {
			if name := leadingIdentifier(word); name != "" {
				after := word[len(name):]
				if strings.HasPrefix(after, "[") {
					if end := strings.Index(after, "]"); end >= 0 {
						after = after[end+1:]
					} else {
						after = ""
					}
				}
				if value, ok := strings.CutPrefix(after, "+="); ok {
					found = append(found, assignment{name: name, value: value, hasValue: true})
				} else if value, ok := strings.CutPrefix(after, "="); ok {
					found = append(found, assignment{name: name, value: value, hasValue: true})
				}
			}
			var following []string
			for _, next := range words[index+1:] {
				if isShellSeparator(next) {
					break
				}
				following = append(following, next)
			}
			switch {
			case slices.Contains(declaring, word):
				for _, argument := range following {
					if !strings.HasPrefix(argument, "-") && !strings.HasPrefix(argument, "+") {
						if name := leadingIdentifier(argument); name != "" {
							add(name)
						}
					}
				}
			case word == "printf":
				for position, argument := range following {
					if argument == "-v" && position+1 < len(following) {
						if name := leadingIdentifier(following[position+1]); name != "" {
							add(name)
						}
					} else if name, ok := strings.CutPrefix(argument, "-v"); ok && leadingIdentifier(name) != "" {
						add(leadingIdentifier(name))
					}
				}
			case word == "for" || word == "select" || word == "coproc":
				if len(following) > 0 && leadingIdentifier(following[0]) != "" {
					add(leadingIdentifier(following[0]))
				}
			}
		}
	}
	return found
}

// goCommandProblems reads the go commands of a run script: `go env -w`,
// the flags of refusedGoFlags, and -gcflags/-ldflags values that pass an
// option outside allowedGoFlags.
func goCommandProblems(script string) []string {
	var problems []string
	for _, reading := range shellLines(script) {
		words := shellWords(reading.text)
		for index, word := range words {
			if word != "go" && !strings.HasSuffix(word, "/go") {
				continue
			}
			var command []string
			for _, next := range words[index+1:] {
				if isShellSeparator(next) {
					break
				}
				command = append(command, next)
			}
			if len(command) > 1 && command[0] == "env" && slices.Contains(command[1:], "-w") {
				problems = append(problems, "`go env -w` writes the go env file")
			}
			for position, argument := range command {
				flag := "-" + strings.TrimLeft(argument, "-")
				name, value, hasValue := strings.Cut(flag, "=")
				for _, refused := range refusedGoFlags {
					if name == refused {
						problems = append(problems, fmt.Sprintf("the go flag %#q", argument))
					}
				}
				if name == "-gcflags" || name == "-ldflags" || name == "-asmflags" {
					if !hasValue && position+1 < len(command) {
						value = command[position+1]
					}
					if strings.Contains(value, "-") && !slices.Contains(allowedGoFlags, name+"="+value) {
						problems = append(problems, fmt.Sprintf("the go flag %#q passes options (allowedGoFlags)", name))
					}
				}
			}
		}
	}
	return problems
}

// listedLine reports whether line (trimmed) is listed for the gated file.
func listedLine(list [][2]string, file, line string) bool {
	for _, entry := range list {
		if entry[0] == file && entry[1] == strings.TrimSpace(line) {
			return true
		}
	}
	return false
}

func variableAllowed(name, value string, hasValue bool) bool {
	fixed, ok := allowedVariables[name]
	return ok && (fixed == anyValue || hasValue && value == fixed)
}

// closedListViolations applies the closed allowlists of D8 to a gated file
// (a workflow, local action or rendered generated file; templates are gated
// as rendered): listed variables only, runner files written only on listed
// lines, no expression function outside listed lines, no env:/with: that is
// a whole expression, only bash as shell:, no refused go command or flag,
// and exact versions in allowlisted actions' inputs.
func closedListViolations(path, text string) []string {
	documents, err := yamlDocuments(text, false)
	if err != nil {
		return []string{fmt.Sprintf("%s: %v", path, err)}
	}
	var violations []string
	refuse := func(line int, format string, arguments ...any) {
		violations = append(violations, fmt.Sprintf("%s:%d: ", path, line)+fmt.Sprintf(format, arguments...))
	}
	for _, node := range yamlNodes(documents) {
		if node.Kind != yaml.MappingNode {
			continue
		}
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			switch strings.ToLower(key.Value) {
			case "env", "with":
				if value.Kind != yaml.MappingNode {
					refuse(key.Line, "`%s:` must be a mapping, not an expression or list", key.Value)
					continue
				}
				if strings.ToLower(key.Value) == "with" {
					continue
				}
				for entry := 0; entry+1 < len(value.Content); entry += 2 {
					name, setting := value.Content[entry], value.Content[entry+1]
					if !variableAllowed(name.Value, setting.Value, setting.Kind == yaml.ScalarNode) {
						refuse(name.Line, "the env: name %q is not allowlisted with this value (allowedVariables)", name.Value)
					}
				}
			case "shell":
				if value.Kind != yaml.ScalarNode || !slices.Contains(allowedShells, value.Value) {
					refuse(key.Line, "a `shell:` other than bash")
				}
			case "run":
				if value.Kind != yaml.ScalarNode {
					continue
				}
				// A listed runner-file line is reviewed as a whole: it must be
				// a complete logical line, and every other line that names a
				// runner file is refused, in every reading (continued, in a
				// heredoc or a `$( )`).
				listed := func(line string) bool {
					return listedLine(runnerFileLines, path, line) || listedLine(runnerFileLines, path, "run: "+strings.TrimSpace(line))
				}
				var kept []string
				for _, reading := range shellLines(value.Value) {
					if namesRunnerFile(reading.text) && (reading.continued || !listed(reading.text)) {
						refuse(value.Line, "a runner file (GITHUB_ENV, GITHUB_PATH, GITHUB_OUTPUT) is written only on a complete listed line (runnerFileLines): %q", strings.TrimSpace(reading.text))
					}
					if reading.continued || !listed(reading.text) {
						kept = append(kept, reading.text)
					}
				}
				script := strings.Join(kept, "\n")
				for _, set := range assignments(script) {
					if !variableAllowed(set.name, set.value, set.hasValue) {
						refuse(value.Line, "the run script sets %q, which is not allowlisted with this value (allowedVariables)", set.name)
					}
				}
				for _, problem := range goCommandProblems(script) {
					refuse(value.Line, "%s", problem)
				}
			}
		}
	}
	violations = append(violations, actionInputViolations(path, documents)...)
	violations = append(violations, containerServiceViolations(path, documents)...)
	for number, line := range strings.Split(text, "\n") {
		upper := strings.ToUpper(line)
		if (strings.Contains(upper, "GITHUB_ENV") || strings.Contains(upper, "GITHUB_PATH") || strings.Contains(upper, "GITHUB_OUTPUT")) &&
			!listedLine(runnerFileLines, path, line) {
			refuse(number+1, "a runner file (GITHUB_ENV, GITHUB_PATH, GITHUB_OUTPUT) is written only on a listed line (runnerFileLines)")
		}
		if buildingFunction.MatchString(line) && !listedLine(expressionLines, path, line) {
			refuse(number+1, "an expression function that builds names or structure (format, fromJSON, join, toJSON) outside a listed line (expressionLines)")
		}
	}
	slices.Sort(violations)
	return slices.Compact(violations)
}

// allowedContainerOptions are the `options:` a job container or service may
// take, exactly (a folded `>-` value, as the parser reads it): the database
// health checks of the framework's CI and of the generated CI.
var allowedContainerOptions = []string{
	`--health-cmd "pg_isready -U forge" --health-interval 2s --health-timeout 3s --health-retries 15`,
	`--health-cmd "pg_isready -U forge" --health-interval 2s --health-timeout 3s --health-retries 30`,
	`--health-cmd "mysqladmin ping -h 127.0.0.1 -uroot -pforge" --health-interval 2s --health-timeout 3s --health-retries 30`,
	`--health-cmd "healthcheck.sh --connect --innodb_initialized" --health-interval 2s --health-timeout 3s --health-retries 30`,
}

// containerKeys are the keys a job container or service may use: no
// volumes, credentials, entrypoint or command.
var containerKeys = []string{"image", "env", "ports", "options"}

// refusedContainerFlags are docker options refused in any container or
// service `options:`, even a listed one.
var refusedContainerFlags = []string{"-e", "--env", "--env-file", "-v", "--volume", "--mount", "--entrypoint", "--privileged", "--network", "--cap-add"}

var numericPort = regexp.MustCompile(`^\d+(?::\d+)?$`)

// containerServiceViolations reads job containers and services: only
// containerKeys, `options:` exactly one of allowedContainerOptions and
// without refusedContainerFlags, and numeric `ports:`. Their `env:` falls
// under allowedVariables.
func containerServiceViolations(path string, documents []*yaml.Node) []string {
	var containers []*yaml.Node
	for _, node := range yamlNodes(documents) {
		if node.Kind != yaml.MappingNode {
			continue
		}
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			switch {
			case key.Value == "container" && value.Kind == yaml.MappingNode:
				containers = append(containers, value)
			case key.Value == "services" && value.Kind == yaml.MappingNode:
				for service := 1; service < len(value.Content); service += 2 {
					containers = append(containers, value.Content[service])
				}
			}
		}
	}
	var violations []string
	refuse := func(line int, format string, arguments ...any) {
		violations = append(violations, fmt.Sprintf("%s:%d: ", path, line)+fmt.Sprintf(format, arguments...))
	}
	for _, container := range containers {
		if container.Kind != yaml.MappingNode {
			refuse(container.Line, "a service must be a mapping")
			continue
		}
		for index := 0; index+1 < len(container.Content); index += 2 {
			key, value := container.Content[index], container.Content[index+1]
			if !slices.Contains(containerKeys, key.Value) {
				refuse(key.Line, "the container or service key %q is not allowed (containerKeys)", key.Value)
			}
			switch key.Value {
			case "options":
				for _, word := range strings.Fields(value.Value) {
					word = strings.Trim(word, `"'`)
					for _, flag := range refusedContainerFlags {
						if word == flag || strings.HasPrefix(word, flag+"=") || len(flag) == 2 && strings.HasPrefix(word, flag) {
							refuse(key.Line, "the container option %q is refused", word)
						}
					}
				}
				if value.Kind != yaml.ScalarNode || !slices.Contains(allowedContainerOptions, value.Value) {
					refuse(key.Line, "container options must be one of allowedContainerOptions exactly")
				}
			case "ports":
				fine := value.Kind == yaml.SequenceNode
				for _, port := range value.Content {
					fine = fine && port.Kind == yaml.ScalarNode && numericPort.MatchString(port.Value)
				}
				if !fine {
					refuse(key.Line, "ports must be a list of PORT or PORT:PORT")
				}
			}
		}
	}
	return violations
}

var (
	exactVersion  = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	exactVVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
)

// actionInputViolations reads the with: inputs of the allowlisted actions
// that choose a version: an exact version only, never `latest` or a check
// for it, and a buildx driver image pinned by digest.
func actionInputViolations(path string, documents []*yaml.Node) []string {
	var violations []string
	for _, node := range yamlNodes(documents) {
		uses := yamlGet(node, "uses")
		if uses == nil || uses.Kind != yaml.ScalarNode {
			continue
		}
		action, _, _ := strings.Cut(strings.ToLower(uses.Value), "@")
		with := yamlGet(node, "with")
		input := func(key string) (string, bool) {
			value := yamlGet(with, key)
			if value == nil {
				return "", false
			}
			return value.Value, true
		}
		refuse := func(problem string) {
			violations = append(violations, fmt.Sprintf("%s:%d: %s: %s", path, uses.Line, action, problem))
		}
		if with != nil && with.Kind == yaml.MappingNode {
			for index := 0; index+1 < len(with.Content); index += 2 {
				if strings.Contains(strings.ToLower(with.Content[index+1].Value), "latest") {
					refuse(fmt.Sprintf("the input %q asks for latest", with.Content[index].Value))
				}
			}
		}
		switch action {
		case "actions/setup-go":
			version, hasVersion := input("go-version")
			file, _ := input("go-version-file")
			exact := hasVersion && exactVersion.MatchString(version)
			if !exact && file != "go.mod" {
				refuse("go-version must be X.Y.Z, or go-version-file go.mod")
			}
			if check, ok := input("check-latest"); ok && check != "false" {
				refuse("check-latest must be absent or false")
			}
		case "golangci/golangci-lint-action":
			if version, _ := input("version"); !exactVVersion.MatchString(version) {
				refuse("version must be vX.Y.Z")
			}
		case "sigstore/cosign-installer":
			if release, ok := input("cosign-release"); ok && !exactVVersion.MatchString(release) {
				refuse("cosign-release must be vX.Y.Z")
			}
		case "docker/setup-buildx-action":
			if version, ok := input("version"); ok && !exactVVersion.MatchString(version) {
				refuse("version must be vX.Y.Z")
			}
			if options, ok := input("driver-opts"); ok {
				pinned := false
				for _, option := range strings.FieldsFunc(options, func(r rune) bool { return r == ',' || r == '\n' }) {
					image, found := strings.CutPrefix(strings.TrimSpace(option), "image=")
					pinned = pinned || found && digestRef.MatchString(image)
				}
				if !pinned {
					refuse("driver-opts must give image= pinned by digest")
				}
			}
		}
	}
	return violations
}

// toolConfigurationFiles returns the files below root that configure a tool
// from the tree (a template's .tmpl suffix ignored): a go workspace
// (go.work, go.work.sum), a go env file (go.env, go/env), a cargo
// configuration (.cargo/config, .cargo/config.toml), a rust-toolchain file
// naming a path, and shell startup files (.bashrc, .profile, .bash_profile,
// .envrc). Parity note: Forge-Rust refuses every rust-toolchain file whose
// content is not exactly listed, since rustup reads more spellings than a
// line rule follows; Go projects have no rust-toolchain file, so this scan
// keeps the `path =` rule as a backstop. A linked `go` or `.cargo` directory counts; the walk does not
// enter skipped directories nor follow links to directories.
func toolConfigurationFiles(root string, skipped []string) []string {
	var found []string
	inside := func(directory string, names ...string) {
		for _, name := range names {
			if _, err := os.Stat(filepath.Join(directory, name)); err == nil {
				found = append(found, filepath.Join(directory, name))
			}
		}
	}
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() && slices.Contains(skipped, path) {
			return filepath.SkipDir
		}
		name := strings.TrimSuffix(entry.Name(), ".tmpl")
		switch name {
		case "go":
			inside(path, "env", "env.tmpl")
		case ".cargo":
			inside(path, "config", "config.toml", "config.tmpl", "config.toml.tmpl")
		case "go.work", "go.work.sum", "go.env", ".bashrc", ".profile", ".bash_profile", ".envrc":
			found = append(found, path)
		case "rust-toolchain", "rust-toolchain.toml":
			content, _ := os.ReadFile(path)
			for _, line := range strings.Split(string(content), "\n") {
				if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "path"); ok && strings.HasPrefix(strings.TrimSpace(rest), "=") {
					found = append(found, path)
					break
				}
			}
		}
		return nil
	})
	slices.Sort(found)
	return slices.Compact(found)
}
