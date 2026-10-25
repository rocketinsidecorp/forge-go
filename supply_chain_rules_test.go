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
	"regexp"
	"sort"
	"strings"

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
	"actions/cache",
	"actions/checkout",
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
//     and a double-quoted key with an escape.
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
					isKey := strings.HasPrefix(strings.TrimLeft(string(characters[end+1:]), " \t"), ":")
					if character == '"' && strings.Contains(quoted, `\`) && isKey {
						problems = append(problems, yamlProblem{number, "a double-quoted key with an escape"})
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

// parsedUses returns every `uses` key a YAML parser reads in text, as
// "line:value" (each document, aliases resolved), and refuses merge keys.
// In a template, a line holding only directives becomes blank (line numbers
// stay) and any other directive a plain word.
func parsedUses(text string, template bool) ([]string, error) {
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
	var found []string
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return found, nil
		}
		if err != nil {
			return nil, err
		}
		var walk func(*yaml.Node) error
		walk = func(node *yaml.Node) error {
			if node.Kind == yaml.MappingNode {
				for index := 0; index+1 < len(node.Content); index += 2 {
					key, value := node.Content[index], node.Content[index+1]
					if key.Kind == yaml.AliasNode && key.Alias != nil {
						key = key.Alias
					}
					if key.Tag == "!!merge" || key.Value == "<<" {
						return fmt.Errorf("line %d: a merge key", key.Line)
					}
					if key.Kind == yaml.ScalarNode && key.Value == "uses" {
						found = append(found, fmt.Sprintf("%d:%s", key.Line, value.Value))
					}
				}
			}
			for _, child := range node.Content {
				if err := walk(child); err != nil {
					return err
				}
			}
			return nil
		}
		if err := walk(&document); err != nil {
			return nil, err
		}
	}
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
