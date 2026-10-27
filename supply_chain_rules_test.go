package forge_test

// The rules of the supply-chain gate (supply_chain_test.go), each a function
// from a file's text to its violations, so the negative tests below prove on
// hand-written snippets that every known bypass is caught and that the
// accepted spellings stay accepted. The Go twin of the rule functions of
// Forge-Rust's crates/forge/tests/supply_chain.rs.

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"text/template/parse"

	"go.yaml.in/yaml/v3"
)

// splitComment splits a YAML line into its code and its comment: a `#` that
// starts the line or follows a space or a tab.
func splitComment(line string) (string, string) {
	for index := 0; index < len(line); index++ {
		if line[index] == '#' && (index == 0 || line[index-1] == ' ' || line[index-1] == '\t') {
			return line[:index], line[index:]
		}
	}
	return line, ""
}

func indentation(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t"))
}

// keyValues returns the values of every `key` in the code of a line,
// unquoted: block style (`key: x`, `- key: x`, `"key": x`, `key : x`) and
// flow style (`- { key: x, other: y }`). A flow value ends at `,` or `}`.
func keyValues(code, key string) []string {
	var found []string
	for offset := 0; ; {
		position := strings.Index(code[offset:], key)
		if position < 0 {
			return found
		}
		start := offset + position
		offset = start + len(key)
		// What precedes the key, past an opening quote and blanks: the start
		// of the line, a list dash, or a flow collection's `{`, `[` or `,`.
		before := strings.TrimRight(strings.TrimRight(code[:start], `"'`), " \t")
		if before != "" && !strings.HasSuffix(before, "-") && !strings.HasSuffix(before, "{") &&
			!strings.HasSuffix(before, "[") && !strings.HasSuffix(before, ",") {
			continue
		}
		after := strings.TrimLeft(strings.TrimLeft(code[start+len(key):], `"'`), " \t")
		value, ok := strings.CutPrefix(after, ":")
		if !ok {
			continue
		}
		if end := strings.IndexAny(value, ",}]"); end >= 0 {
			value = value[:end]
		}
		found = append(found, strings.Trim(strings.TrimSpace(value), `"'`))
	}
}

// localActions returns the `uses: ./path` references of a workflow or
// action, without `./`.
func localActions(text string) []string {
	var found []string
	for _, line := range strings.Split(text, "\n") {
		code, _ := splitComment(line)
		for _, reference := range keyValues(code, "uses") {
			if path, ok := strings.CutPrefix(reference, "./"); ok {
				found = append(found, strings.TrimRight(path, "/"))
			}
		}
	}
	return found
}

// pinViolations requires every action to be owner/repo[/path]@<40-hex
// commit> # vX.Y.Z and every docker:// action to be pinned by digest, in
// every spelling (flow style, anchors and aliases fail). Local `./` actions
// are checked as files of their own.
func pinViolations(path, text string) []string {
	var violations []string
	for number, line := range strings.Split(text, "\n") {
		code, comment := splitComment(line)
		for _, reference := range keyValues(code, "uses") {
			if strings.HasPrefix(reference, "./") {
				continue
			}
			var pinned bool
			if image, ok := strings.CutPrefix(reference, "docker://"); ok {
				pinned = digestRef.MatchString(image)
			} else {
				pinned = pinnedUses.MatchString(reference) && versionTag.MatchString(" "+strings.TrimSpace(comment))
			}
			if !pinned {
				violations = append(violations, fmt.Sprintf("%s:%d: %q must be owner/repo@<40-hex commit> # vX.Y.Z, or docker://<image>@sha256:<digest>", path, number+1, strings.TrimSpace(line)))
			}
		}
	}
	return violations
}

// allowedActions are the action repositories a workflow may use, compared
// without case (GitHub resolves owner and name case-insensitively). Adding
// one is a supply-chain decision: review the action's code, pin it by SHA,
// and add it here and to docs/SUPPLY_CHAIN.md in the same change. Local `./`
// actions are checked as files of their own; `docker://` actions are not
// allowed.
var allowedActions = []string{
	"actions/attest-build-provenance",
	"actions/cache",
	"actions/checkout",
	"actions/download-artifact",
	"actions/setup-go",
	"actions/upload-artifact",
	"docker/build-push-action",
	"docker/login-action",
	"docker/setup-buildx-action",
	"golangci/golangci-lint-action",
	"ossf/scorecard-action",
	"sigstore/cosign-installer",
}

// actionRepository returns the owner/repo of a `uses:` reference, lower
// case, or "" when it names none.
func actionRepository(reference string) string {
	action, _, _ := strings.Cut(reference, "@")
	parts := strings.SplitN(action, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return strings.ToLower(parts[0] + "/" + parts[1])
}

// allowlistViolations requires every `uses:` to name an allowlisted
// repository (or a local `./` action), whatever its pin: an installer action
// would otherwise put any tool on PATH, pinned or not.
func allowlistViolations(path, text string) []string {
	var violations []string
	for number, line := range strings.Split(text, "\n") {
		code, _ := splitComment(line)
		for _, reference := range keyValues(code, "uses") {
			if strings.HasPrefix(reference, "./") {
				continue
			}
			repository := actionRepository(reference)
			// owner/repo[/sub/path]: no empty, `.` or `..` segment can walk
			// from an allowlisted repository into another one.
			action, _, _ := strings.Cut(reference, "@")
			plainPath := true
			for _, segment := range strings.Split(action, "/") {
				plainPath = plainPath && segment != "" && segment != "." && segment != ".."
			}
			allowed := false
			for _, candidate := range allowedActions {
				allowed = allowed || (plainPath && repository != "" && !strings.HasPrefix(reference, "docker://") && candidate == repository)
			}
			if !allowed {
				violations = append(violations, fmt.Sprintf("%s:%d: %q is not an allowlisted action (allowedActions, docs/SUPPLY_CHAIN.md)", path, number+1, reference))
			}
		}
	}
	return violations
}

// yamlProblem is a line where a gated file leaves the simple YAML subset.
type yamlProblem struct {
	line    int // 0-based
	problem string
}

// yamlSubset returns which lines of a gated file are nodes (keys and values
// the rules read; the others are block-scalar text, plain-scalar
// continuations, comments and blank lines) and what makes the file leave
// the simple YAML subset the gate reads exactly (docs/SUPPLY_CHAIN.md,
// "Gated files are written in a simple YAML subset"):
//   - a `\r` not followed by `\n`, U+0085, U+2028 or U+2029 (hidden line
//     breaks), or U+FEFF anywhere but at the start;
//   - outside block scalars and plain-scalar continuations, an anchor (`&`),
//     alias (`*`), tag (`!`), explicit key (`?`) or merge key (`<<`) where a
//     key or value starts (after the indentation, `- `, `: `, or a flow
//     collection's `[`, `{`, `,`);
//   - a flow collection or quoted scalar that does not close on its line,
//     and a double-quoted scalar (key or value) with an escape.
func yamlSubset(text string) ([]bool, []yamlProblem) {
	var problems []yamlProblem
	line := 0
	for offset, character := range text {
		if character == '\n' {
			line++
		}
		hidden := character == '\u0085' || character == '\u2028' || character == '\u2029' ||
			(character == '\r' && !strings.HasPrefix(text[offset+1:], "\n")) ||
			(character == '\uFEFF' && offset > 0)
		if hidden {
			problems = append(problems, yamlProblem{line, "a hidden line break (lone CR, NEL, LS, PS) or a byte-order mark"})
		}
	}
	var nodes []bool
	textUnder := -1 // the column whose deeper lines are scalar text
	for number, raw := range strings.Split(text, "\n") {
		current := strings.TrimSuffix(raw, "\r")
		content := strings.TrimLeft(current, " \t")
		indent := len(current) - len(content)
		if content == "" || strings.HasPrefix(content, "#") {
			// A comment no deeper than the scalar's node ends that scalar.
			if strings.HasPrefix(content, "#") && textUnder >= 0 && indent <= textUnder {
				textUnder = -1
			}
			nodes = append(nodes, false)
			continue
		}
		if textUnder >= 0 {
			if indent > textUnder {
				nodes = append(nodes, false)
				continue
			}
			textUnder = -1
		}
		nodes = append(nodes, true)
		for {
			rest, ok := strings.CutPrefix(content, "-")
			if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
				break
			}
			content = strings.TrimLeft(rest, " \t")
		}
		column := len(current) - len(content)
		characters := []rune(content)
		depth := 0
		atNode := true
		lastPlain, lastBlock := false, false
	scan:
		for index := 0; index < len(characters); index++ {
			character := characters[index]
			if atNode {
				if character == ' ' || character == '\t' {
					continue
				}
				atNode = false
				lastPlain, lastBlock = false, false
				if strings.ContainsRune("&*!?", character) || strings.HasPrefix(string(characters[index:]), "<<") {
					problems = append(problems, yamlProblem{number, "an anchor, alias, tag, explicit key or merge key"})
				}
				switch character {
				case '"', '\'':
					end := -1
					escaped := false
					for scan := index + 1; scan < len(characters); scan++ {
						inner := characters[scan]
						if character == '"' && inner == '\\' && !escaped {
							escaped = true
							continue
						}
						if inner == character && !escaped {
							if character == '\'' && scan+1 < len(characters) && characters[scan+1] == '\'' {
								scan++
								continue
							}
							end = scan
							break
						}
						escaped = false
					}
					if end < 0 {
						problems = append(problems, yamlProblem{number, "a quoted scalar that does not close on its line"})
						break scan
					}
					quoted := string(characters[index:end])
					// An escape spells any text (`"c\x61rgo"`, `"u\x73es"`)
					// where the text rules would not read it: no
					// double-quoted scalar, key or value, holds a backslash.
					if character == '"' && strings.Contains(quoted, `\`) {
						problems = append(problems, yamlProblem{number, "a double-quoted scalar with an escape"})
					}
					index = end
				case '[', '{':
					depth++
					atNode = true
				case ']', '}':
					if depth > 0 {
						depth--
					} else {
						lastPlain = true
					}
				case '|', '>':
					lastBlock = depth == 0
				case '#':
					break scan
				default:
					// Only a plain scalar outside a flow collection may go
					// on, more indented, on the next lines.
					lastPlain = depth == 0
				}
				continue
			}
			switch {
			case character == '#' && index > 0 && (characters[index-1] == ' ' || characters[index-1] == '\t'):
				break scan
			case character == ':' && (index+1 == len(characters) || strings.ContainsRune(" \t,]}", characters[index+1])):
				// A key ends: what follows is its value, not the key.
				atNode = true
				lastPlain, lastBlock = false, false
			case character == ',' && depth > 0:
				atNode = true
			case (character == ']' || character == '}') && depth > 0:
				depth--
			}
		}
		if depth > 0 {
			problems = append(problems, yamlProblem{number, "a flow collection that spans lines"})
		}
		if depth == 0 && (lastBlock || lastPlain) {
			textUnder = column
		}
	}
	return nodes, problems
}

// yamlSubsetViolations reports what yamlSubset refuses.
func yamlSubsetViolations(path, text string) []string {
	lines := strings.Split(text, "\n")
	_, problems := yamlSubset(text)
	var violations []string
	for _, problem := range problems {
		shown := ""
		if problem.line < len(lines) {
			shown = strings.TrimSpace(lines[problem.line])
		}
		violations = append(violations, fmt.Sprintf("%s:%d: %q: %s: gated files are written in a simple YAML subset (docs/SUPPLY_CHAIN.md); write a multi-line expression as a `>-` block scalar", path, problem.line+1, shown, problem.problem))
	}
	return violations
}

// templateField is a text/template action in a generated-app template.
var templateField = regexp.MustCompile(`\{\{-?[^}]*-?\}\}`)

// yamlDocuments parses text with yaml/v3 into its documents. In a
// template, a line holding only directives becomes blank (line numbers stay)
// and any other directive a plain word; the rendered outputs are gated on
// their own (TestGeneratedWorkflowsPassTheGate).
func yamlDocuments(text string, template bool) ([]*yaml.Node, error) {
	if template {
		lines := strings.Split(text, "\n")
		for index, line := range lines {
			if trimmed := strings.TrimSpace(line); templateField.MatchString(trimmed) && templateField.ReplaceAllString(trimmed, "") == "" {
				lines[index] = ""
				continue
			}
			lines[index] = templateField.ReplaceAllString(line, "x")
		}
		text = strings.Join(lines, "\n")
	}
	decoder := yaml.NewDecoder(strings.NewReader(text))
	var documents []*yaml.Node
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return documents, nil
		}
		if err != nil {
			return nil, err
		}
		if err := duplicateKeys(&document); err != nil {
			return nil, err
		}
		documents = append(documents, &document)
	}
}

// duplicateKeys refuses a mapping that holds a key twice, compared without
// case: a reader that takes the first value and one that takes the last
// (yamlGet takes the first) would disagree.
func duplicateKeys(document *yaml.Node) error {
	for _, node := range yamlNodes([]*yaml.Node{document}) {
		if node.Kind != yaml.MappingNode {
			continue
		}
		seen := map[string]bool{}
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode {
				continue
			}
			folded := strings.ToLower(key.Value)
			if seen[folded] {
				return fmt.Errorf("line %d: the key %q appears twice", key.Line, key.Value)
			}
			seen[folded] = true
		}
	}
	return nil
}

// yamlNodes returns every node of the documents, depth first.
func yamlNodes(documents []*yaml.Node) []*yaml.Node {
	var found []*yaml.Node
	var walk func(*yaml.Node)
	walk = func(node *yaml.Node) {
		found = append(found, node)
		for _, child := range node.Content {
			walk(child)
		}
	}
	for _, document := range documents {
		walk(document)
	}
	return found
}

// yamlGet returns the value of key in a mapping node, or nil.
func yamlGet(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Kind == yaml.ScalarNode && node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

// parsedUses returns every `uses` key a YAML parser reads in text, as
// "line:value" (aliases resolved), and refuses merge keys.
func parsedUses(text string, template bool) ([]string, error) {
	documents, err := yamlDocuments(text, template)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, node := range yamlNodes(documents) {
		if node.Kind != yaml.MappingNode {
			continue
		}
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			if key.Kind == yaml.AliasNode && key.Alias != nil {
				key = key.Alias
			}
			if key.Tag == "!!merge" || key.Value == "<<" {
				return nil, fmt.Errorf("line %d: a merge key", key.Line)
			}
			if key.Kind == yaml.ScalarNode && key.Value == "uses" {
				found = append(found, fmt.Sprintf("%d:%s", key.Line, value.Value))
			}
		}
	}
	return found, nil
}

// releaseWriteScopes are the `write` permissions any release workflow (the
// framework's and the generated application's) may grant its jobs: the OIDC
// token for keyless signing and the registry for the images.
// frameworkReleaseWriteScopes are those the framework's own release workflow
// may grant besides: `attestations` (GitHub build provenance of the binary
// archives) and `contents` (the draft release). The scopes each job holds are
// fixed by TestReleaseJobsHoldOnlyTheirPermissions.
var (
	releaseWriteScopes          = []string{"id-token", "packages"}
	frameworkReleaseWriteScopes = []string{"attestations", "contents", "id-token", "packages"}
	frameworkReleasePath        = ".github/workflows/release.yml"
)

// writeScopesOf returns the write scopes a gated file's permissions may
// grant: none outside a release workflow.
func writeScopesOf(path string, release bool) []string {
	switch {
	case !release:
		return nil
	case filepath.ToSlash(path) == frameworkReleasePath:
		return frameworkReleaseWriteScopes
	}
	return releaseWriteScopes
}

// treeViolations reads the parsed tree: every step that uses
// actions/checkout (any case) sets `with: persist-credentials: false`
// itself, and no `permissions` grants `write` beyond writeScopesOf (or
// `write-all`).
func treeViolations(path, text string, release bool) []string {
	documents, err := yamlDocuments(text, strings.HasSuffix(path, ".tmpl"))
	if err != nil {
		return []string{fmt.Sprintf("%s: %v", path, err)}
	}
	var violations []string
	for _, node := range yamlNodes(documents) {
		if node.Kind != yaml.MappingNode {
			continue
		}
		if uses := yamlGet(node, "uses"); uses != nil && uses.Kind == yaml.ScalarNode {
			action, _, _ := strings.Cut(uses.Value, "@")
			kept := yamlGet(yamlGet(node, "with"), "persist-credentials")
			if strings.EqualFold(action, "actions/checkout") && (kept == nil || kept.Kind != yaml.ScalarNode || kept.Value != "false") {
				violations = append(violations, fmt.Sprintf("%s:%d: this checkout step must set with: persist-credentials: false", path, uses.Line))
			}
		}
		permissions := yamlGet(node, "permissions")
		if permissions == nil {
			continue
		}
		switch permissions.Kind {
		case yaml.ScalarNode:
			if permissions.Value != "read-all" && permissions.Value != "" {
				violations = append(violations, fmt.Sprintf("%s:%d: permissions: %s is not allowed", path, permissions.Line, permissions.Value))
			}
		case yaml.MappingNode:
			for index := 0; index+1 < len(permissions.Content); index += 2 {
				scope, access := permissions.Content[index], permissions.Content[index+1]
				allowed := access.Kind == yaml.ScalarNode && (access.Value == "read" || access.Value == "none" ||
					(access.Value == "write" && slices.Contains(writeScopesOf(path, release), scope.Value)))
				if !allowed {
					violations = append(violations, fmt.Sprintf("%s:%d: %s: %s is not allowed", path, scope.Line, scope.Value, access.Value))
				}
			}
		default:
			violations = append(violations, fmt.Sprintf("%s:%d: permissions must be a mapping", path, permissions.Line))
		}
	}
	return violations
}

// templateDirectiveViolations allows only the directive forms a gated
// template needs, so a template cannot render text the gate never sees:
// `{{.Field}}`, `{{index . N}}`, and `if`/`else`/`range`/`end` over fields
// (`and`, `or`, `not` of fields). String literals, variables, `print*`,
// `with`, `template` and every other function are refused.
func templateDirectiveViolations(path, text string) []string {
	functions := map[string]any{"and": true, "or": true, "not": true, "index": true}
	trees, err := parse.Parse(path, text, "{{", "}}", functions)
	if err != nil {
		return []string{fmt.Sprintf("%s: %v", path, err)}
	}
	var violations []string
	refuse := func(node parse.Node) {
		violations = append(violations, fmt.Sprintf("%s: %q is not a gated directive form (.Field, index . N, if/else/range/end over fields)", path, node.String()))
	}
	isField := func(node parse.Node) bool {
		switch node.(type) {
		case *parse.FieldNode, *parse.DotNode:
			return true
		}
		return false
	}
	condition := func(pipe *parse.PipeNode) bool {
		if pipe == nil || len(pipe.Decl) != 0 || len(pipe.Cmds) != 1 {
			return false
		}
		args := pipe.Cmds[0].Args
		if len(args) == 1 {
			return isField(args[0])
		}
		identifier, ok := args[0].(*parse.IdentifierNode)
		if !ok || (identifier.Ident != "and" && identifier.Ident != "or" && identifier.Ident != "not") {
			return false
		}
		for _, arg := range args[1:] {
			if !isField(arg) {
				return false
			}
		}
		return true
	}
	output := func(pipe *parse.PipeNode) bool {
		if pipe == nil || len(pipe.Decl) != 0 || len(pipe.Cmds) != 1 {
			return false
		}
		args := pipe.Cmds[0].Args
		if len(args) == 1 {
			_, field := args[0].(*parse.FieldNode)
			return field
		}
		identifier, ok := args[0].(*parse.IdentifierNode)
		if !ok || identifier.Ident != "index" || len(args) != 3 {
			return false
		}
		_, dot := args[1].(*parse.DotNode)
		_, number := args[2].(*parse.NumberNode)
		return dot && number
	}
	var walk func(parse.Node)
	walk = func(node parse.Node) {
		switch typed := node.(type) {
		case nil:
		case *parse.ListNode:
			if typed == nil {
				return
			}
			for _, child := range typed.Nodes {
				walk(child)
			}
		case *parse.TextNode:
		case *parse.ActionNode:
			if !output(typed.Pipe) {
				refuse(typed)
			}
		case *parse.IfNode:
			if !condition(typed.Pipe) {
				refuse(typed)
			}
			walk(typed.List)
			walk(typed.ElseList)
		case *parse.RangeNode:
			if !condition(typed.Pipe) {
				refuse(typed)
			}
			walk(typed.List)
			walk(typed.ElseList)
		default:
			refuse(typed)
		}
	}
	for name, tree := range trees {
		if name != path {
			violations = append(violations, fmt.Sprintf("%s: a define block %q", path, name))
		}
		walk(tree.Root)
	}
	return violations
}

// scannedUses returns every `uses` the line scanner reads on node lines, as
// "line:value": block-scalar text is not counted, so a decoy there cannot
// balance a key the scanner misses.
func scannedUses(text string) []string {
	nodes, _ := yamlSubset(text)
	var found []string
	for number, line := range strings.Split(text, "\n") {
		if number >= len(nodes) || !nodes[number] {
			continue
		}
		code, _ := splitComment(strings.TrimSuffix(line, "\r"))
		for _, value := range keyValues(code, "uses") {
			found = append(found, fmt.Sprintf("%d:%s", number+1, value))
		}
	}
	return found
}

// parserViolations requires the line scanner and a YAML parser to read the
// same `uses` keys: the same values on the same lines.
func parserViolations(path, text string) []string {
	parsed, err := parsedUses(text, strings.HasSuffix(path, ".tmpl"))
	if err != nil {
		return []string{fmt.Sprintf("%s: %v", path, err)}
	}
	scanned := scannedUses(text)
	sort.Strings(parsed)
	sort.Strings(scanned)
	if strings.Join(parsed, "\n") != strings.Join(scanned, "\n") {
		return []string{fmt.Sprintf("%s: the YAML parser reads uses keys %q, the gate %q: write every uses: key plainly", path, parsed, scanned)}
	}
	return nil
}

// stepLines returns the lines of the step (list item) that line index
// belongs to: from its `- ` to the next line indented no deeper than that
// dash.
func stepLines(lines []string, index int) []string {
	own := indentation(lines[index])
	start := index
	for candidate := index; candidate >= 0; candidate-- {
		code, _ := splitComment(lines[candidate])
		if strings.HasPrefix(strings.TrimLeft(code, " \t"), "-") && indentation(lines[candidate]) <= own {
			start = candidate
			break
		}
	}
	dash := indentation(lines[start])
	end := len(lines)
	for candidate := start + 1; candidate < len(lines); candidate++ {
		code, _ := splitComment(lines[candidate])
		if strings.TrimSpace(code) != "" && indentation(lines[candidate]) <= dash {
			end = candidate
			break
		}
	}
	return lines[start:end]
}

// checkoutViolations requires every actions/checkout step, in any spelling
// of the owner and name (GitHub resolves them case-insensitively), to set
// persist-credentials: false itself: the token stays out of .git/config,
// where later steps could read it.
func checkoutViolations(path, text string) []string {
	lines := strings.Split(text, "\n")
	var violations []string
	for index, line := range lines {
		code, _ := splitComment(line)
		checkout := false
		for _, reference := range keyValues(code, "uses") {
			action, _, _ := strings.Cut(reference, "@")
			checkout = checkout || strings.EqualFold(action, "actions/checkout")
		}
		if !checkout {
			continue
		}
		kept := false
		for _, stepLine := range stepLines(lines, index) {
			code, _ := splitComment(stepLine)
			for _, value := range keyValues(code, "persist-credentials") {
				kept = kept || value == "false"
			}
		}
		if !kept {
			violations = append(violations, fmt.Sprintf("%s:%d: this checkout step must set persist-credentials: false", path, index+1))
		}
	}
	return violations
}

// mappingEntry returns the key (unquoted) and value of a `key: value` entry;
// false for anything else, such as a shell line of a `run:` block.
func mappingEntry(content string) (string, string, bool) {
	key, value, ok := strings.Cut(content, ":")
	if !ok {
		return "", "", false
	}
	key = strings.Trim(strings.TrimSpace(key), `"'`)
	plain := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
	}
	if key == "" || strings.IndexFunc(key, func(r rune) bool { return !plain(r) }) >= 0 {
		return "", "", false
	}
	if value != "" && value[0] != ' ' && value[0] != '\t' {
		return "", "", false
	}
	return key, strings.TrimSpace(value), true
}

type containerRef struct {
	line  int
	image string
}

// containerImages returns the containers a workflow runs: a service's
// `image:` (services.<id>.image), a job's container.image, the
// `container: <image>` shorthand, and the same in flow style. An `image:`
// anywhere else, such as an action input under a step's `with:` or a matrix
// entry, is not a container.
func containerImages(text string) []containerRef {
	type parent struct {
		indent int
		key    string
	}
	var parents []parent
	var found []containerRef
	pop := func(indent int) {
		for len(parents) > 0 && parents[len(parents)-1].indent >= indent {
			parents = parents[:len(parents)-1]
		}
	}
	for number, line := range strings.Split(text, "\n") {
		code, _ := splitComment(line)
		if strings.TrimSpace(code) == "" {
			continue
		}
		indent := indentation(code)
		content := strings.TrimSpace(code)
		// A list item's mapping starts after its dash, one level deeper.
		if rest, ok := strings.CutPrefix(content, "-"); ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
			pop(indent)
			item := strings.TrimLeft(rest, " \t")
			indent += len(content) - len(item)
			content = item
		}
		pop(indent)
		key, value, ok := mappingEntry(content)
		if !ok {
			continue
		}
		var parentKey, grandparentKey string
		if len(parents) > 0 {
			parentKey = parents[len(parents)-1].key
		}
		if len(parents) > 1 {
			grandparentKey = parents[len(parents)-2].key
		}
		flow := strings.HasPrefix(value, "{")
		image := key == "image" && (parentKey == "container" || grandparentKey == "services")
		shorthand := key == "container" && value != "" && !flow
		if image || shorthand {
			found = append(found, containerRef{number + 1, strings.Trim(value, `"'`)})
		}
		if flow && (key == "services" || key == "container" || parentKey == "services") {
			for _, image := range keyValues(value, "image") {
				found = append(found, containerRef{number + 1, image})
			}
		}
		parents = append(parents, parent{indent, key})
	}
	return found
}

func containerViolations(path, text string) []string {
	var violations []string
	for _, container := range containerImages(text) {
		if !digestRef.MatchString(container.image) {
			violations = append(violations, fmt.Sprintf("%s:%d: service image or container %q must be pinned by digest", path, container.line, container.image))
		}
	}
	return violations
}

// releaseViolations requires the release workflow to refuse a tag whose
// major version does not match the module path (a v2+ module ends in /vN).
func releaseViolations(path, text string) []string {
	var violations []string
	for _, required := range []string{
		"cosign sign --yes \"${IMAGE}@${DIGEST}\"",
		"provenance: mode=max",
		"sbom: generator=",
		"git merge-base --is-ancestor",
		"if [[ \"${module##*/}\" =~ ^v[0-9]+$ ]]; then",
		"if [ \"$base$want\" != \"$module\" ]; then",
	} {
		if !strings.Contains(text, required) {
			violations = append(violations, fmt.Sprintf("%s: missing %q", path, required))
		}
	}
	return violations
}

// binaryReleaseSteps are the steps the framework's release workflow needs to
// ship signed, checksummed, attested binaries as a draft release: the build
// script, the checksums and their signature (cosign, keyless, verified by the
// job itself), the provenance attestation of every file the checksums list,
// and a draft (never a published) release. Dropping one removes a guarantee
// docs/SUPPLY_CHAIN.md makes.
var binaryReleaseSteps = []string{
	"scripts/release-build.sh \"$GITHUB_REF_NAME\"",
	"scripts/release-checksums.sh \"$GITHUB_REF_NAME\"",
	"cosign sign-blob --yes --bundle \"$RUNNER_TEMP/release/checksums.txt.sigstore.json\" \"$RUNNER_TEMP/release/checksums.txt\"",
	"cosign verify-blob",
	"--bundle \"$RUNNER_TEMP/release/checksums.txt.sigstore.json\" \\",
	"actions/attest-build-provenance@",
	"subject-checksums: ${{ runner.temp }}/release/checksums.txt",
	"scripts/release-draft.sh \"$GITHUB_REF_NAME\"",
}

// binaryReleaseViolations requires binaryReleaseSteps in the framework's
// release workflow, and the draft script to create a draft, verify the tag
// and never publish.
func binaryReleaseViolations(path, workflow, draftScript string) []string {
	var violations []string
	for _, required := range binaryReleaseSteps {
		if !strings.Contains(workflow, required) {
			violations = append(violations, fmt.Sprintf("%s: missing %q", path, required))
		}
	}
	for _, required := range []string{"gh release create", "--draft", "--verify-tag", "--generate-notes"} {
		if !strings.Contains(draftScript, required) {
			violations = append(violations, fmt.Sprintf("scripts/release-draft.sh: missing %q", required))
		}
	}
	for _, refused := range []string{"--draft=false", "gh release edit", "gh release upload", "--latest"} {
		if strings.Contains(draftScript, refused) {
			violations = append(violations, fmt.Sprintf("scripts/release-draft.sh: %q publishes or changes a release", refused))
		}
	}
	return violations
}

// redirectingVariables redirect where the go command fetches modules,
// checksums and toolchains, which flags and settings it reads (GOENV and
// XDG_CONFIG_HOME move the go env file), which toolchain, module cache and
// cache program it uses, and the runner switch that re-enables the
// deprecated `::set-env` and `::add-path` commands.
var redirectingVariables = []string{
	"GOFLAGS", "GOPROXY", "GONOSUMDB", "GONOSUMCHECK", "GOSUMDB", "GOINSECURE",
	"GOPRIVATE", "GONOPROXY", "GOENV", "GOTOOLCHAIN", "GOCACHEPROG",
	"GOMODCACHE", "GOPATH", "XDG_CONFIG_HOME", "ACTIONS_ALLOW_UNSECURE_COMMANDS",
}

// assignedOnlyVariables may be read (`$HOME`, `go env GOROOT`) but never
// set: HOME moves ~/.config/go/env, the go env file, and GOROOT the
// toolchain.
var assignedOnlyVariables = []string{"HOME", "GOROOT"}

// refusedText is refused anywhere in a gated file, without case: a
// `$GITHUB_ENV` write (it sets variables for every later step under a name
// the shell may build at run time) and the deprecated `::set-env` and
// `::add-path` workflow commands.
var refusedText = [][2]string{
	{"GITHUB_ENV", "gated files do not write $GITHUB_ENV; set variables in an env: block"},
	{"::SET-ENV", "the ::set-env workflow command is refused"},
	{"::ADD-PATH", "the ::add-path workflow command is refused"},
}

// pinnedToolchain is a GOTOOLCHAIN setting that downloads nothing it was
// not pinned to: `local`, or one exact `goX.Y.Z` release.
var pinnedToolchain = regexp.MustCompile(`^\s*[:=]\s*["']?(?:local|go\d+\.\d+\.\d+)["']?(?:\s|$)`)

// isWordByte reports whether b continues a variable name.
func isWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}

// environmentViolations refuses every redirecting variable anywhere in a
// gated file, in any case: in an `env:` block (workflow, job or step) or in
// `run` text. GOTOOLCHAIN passes only set to a fixed pinned value. HOME is
// refused where it is set: an `env:` key (`HOME:`, `"HOME":`) or a shell
// assignment (`HOME=`), not where it is read (`$HOME`, `${HOME}`).
// refusedText is refused anywhere. The subset refuses escapes in
// double-quoted scalars, so the text read here is the text YAML yields; a
// name the shell assembles at run time is the accepted residual risk
// (docs/SUPPLY_CHAIN.md, "Threat model and residual risk").
func environmentViolations(path, text string) []string {
	// ASCII case folding keeps every byte offset of text.
	upper := []byte(text)
	for index, b := range upper {
		if b >= 'a' && b <= 'z' {
			upper[index] = b - 'a' + 'A'
		}
	}
	return redirectingViolations(path, text, string(upper))
}

// wordOffsets returns the offsets of name in upper where it is a whole
// word.
func wordOffsets(upper, name string) []int {
	var found []int
	for offset := 0; ; {
		at := strings.Index(upper[offset:], name)
		if at < 0 {
			return found
		}
		start, end := offset+at, offset+at+len(name)
		offset = end
		if start > 0 && isWordByte(upper[start-1]) || end < len(upper) && isWordByte(upper[end]) {
			continue
		}
		found = append(found, start)
	}
}

func redirectingViolations(path, text, upper string) []string {
	var violations []string
	line := func(offset int) int { return strings.Count(upper[:offset], "\n") + 1 }
	for _, refused := range refusedText {
		for offset := 0; ; {
			found := strings.Index(upper[offset:], refused[0])
			if found < 0 {
				break
			}
			violations = append(violations, fmt.Sprintf("%s:%d: %s", path, line(offset+found), refused[1]))
			offset += found + len(refused[0])
		}
	}
	for _, variable := range redirectingVariables {
		for _, start := range wordOffsets(upper, variable) {
			end := start + len(variable)
			if variable == "GOTOOLCHAIN" && pinnedToolchain.MatchString(strings.SplitN(text[end:], "\n", 2)[0]) {
				continue
			}
			violations = append(violations, fmt.Sprintf("%s:%d: %s redirects where the go command fetches from or what it runs", path, line(start), variable))
		}
	}
	for _, variable := range assignedOnlyVariables {
		for _, start := range wordOffsets(upper, variable) {
			if start > 0 && (upper[start-1] == '$' || upper[start-1] == '{') {
				continue
			}
			rest := strings.TrimLeft(upper[start+len(variable):], "\"' \t")
			if strings.HasPrefix(rest, ":") || strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, "+=") {
				violations = append(violations, fmt.Sprintf("%s:%d: gated files do not set %s", path, line(start), variable))
			}
		}
	}
	return violations
}
