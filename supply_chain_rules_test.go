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

// keyPositionViolations refuses, outside block scalars (`run: |`), what a
// line scanner could misread: a key position (after the indentation, `- `,
// or the `[`, `{`, `,` of a flow collection) that starts with `?` (explicit
// key), `&` (anchor), `*` (alias) or `!` (tag), or a double-quoted key with
// an escape (`"u\x73es"`). Each would let a `uses:` key slip past the pin
// and allowlist rules.
func keyPositionViolations(path, text string) []string {
	var violations []string
	block := -1 // the column of the key whose block scalar is skipped
	for number, line := range strings.Split(text, "\n") {
		code, _ := splitComment(line)
		if strings.TrimSpace(code) == "" {
			continue
		}
		indent := indentation(code)
		if block >= 0 {
			if indent > block {
				continue
			}
			block = -1
		}
		content := strings.TrimLeft(code, " \t")
		for {
			rest, ok := strings.CutPrefix(content, "-")
			if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
				break
			}
			content = strings.TrimLeft(rest, " \t")
		}
		positions := []string{content}
		_, value, _ := mappingEntry(content)
		// Inside a flow collection, what follows `[`, `{` or `,` (outside
		// quotes) is a key position too.
		for _, flow := range []string{content, value} {
			if !strings.HasPrefix(flow, "[") && !strings.HasPrefix(flow, "{") {
				continue
			}
			var quote rune
			for at, character := range flow {
				switch {
				case quote != 0 && character == quote:
					quote = 0
				case quote != 0:
				case character == '"' || character == '\'':
					quote = character
				case character == '[' || character == '{' || character == ',':
					positions = append(positions, strings.TrimLeft(flow[at+1:], " \t"))
				}
			}
		}
		refused := false
		for _, position := range positions {
			if strings.HasPrefix(position, "?") || strings.HasPrefix(position, "&") ||
				strings.HasPrefix(position, "*") || strings.HasPrefix(position, "!") {
				refused = true
			}
			if key, ok := strings.CutPrefix(position, `"`); ok {
				quoted, _, closed := strings.Cut(key, `"`)
				refused = refused || !closed || strings.Contains(quoted, `\`)
			}
		}
		if refused {
			violations = append(violations, fmt.Sprintf("%s:%d: %q: explicit, anchored, aliased, tagged or escaped keys are not read by the gate; write the key plainly", path, number+1, strings.TrimSpace(line)))
		}
		if opensBlock(value) {
			block = len(code) - len(content)
		}
	}
	return violations
}

// opensBlock reports whether a mapping value starts a block scalar (`|`,
// `>-`, `|+2`, ...).
func opensBlock(value string) bool {
	if !strings.HasPrefix(value, "|") && !strings.HasPrefix(value, ">") {
		return false
	}
	for _, character := range value[1:] {
		if character != '-' && character != '+' && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

// templateField is a text/template action in a generated-app template.
var templateField = regexp.MustCompile(`\{\{-?[^}]*-?\}\}`)

// yamlUsesKeys counts the mapping keys a YAML parser reads as `uses` in
// text (each document, aliases resolved), and refuses merge keys, which
// could bring one in from an anchor. Template directives are replaced first:
// a line holding only one is dropped, any other becomes a plain word.
func yamlUsesKeys(text string, template bool) (int, error) {
	if template {
		var lines []string
		for _, line := range strings.Split(text, "\n") {
			if trimmed := strings.TrimSpace(line); templateField.MatchString(trimmed) && templateField.ReplaceAllString(trimmed, "") == "" {
				continue
			}
			lines = append(lines, templateField.ReplaceAllString(line, "x"))
		}
		text = strings.Join(lines, "\n")
	}
	decoder := yaml.NewDecoder(strings.NewReader(text))
	count := 0
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return count, nil
		}
		if err != nil {
			return 0, err
		}
		var walk func(*yaml.Node) error
		walk = func(node *yaml.Node) error {
			if node.Kind == yaml.MappingNode {
				for index := 0; index+1 < len(node.Content); index += 2 {
					key := node.Content[index]
					if key.Kind == yaml.AliasNode && key.Alias != nil {
						key = key.Alias
					}
					if key.Tag == "!!merge" || key.Value == "<<" {
						return fmt.Errorf("line %d: a merge key", key.Line)
					}
					if key.Kind == yaml.ScalarNode && key.Value == "uses" {
						count++
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
			return 0, err
		}
	}
}

// scannedUsesKeys counts the `uses` keys the line scanner reads in text.
func scannedUsesKeys(text string) int {
	count := 0
	for _, line := range strings.Split(text, "\n") {
		code, _ := splitComment(line)
		count += len(keyValues(code, "uses"))
	}
	return count
}

// parserViolations requires the line scanner and a YAML parser to find the
// same number of `uses` keys: a spelling the scanner misses is refused.
func parserViolations(path, text string) []string {
	parsed, err := yamlUsesKeys(text, strings.HasSuffix(path, ".tmpl"))
	if err != nil {
		return []string{fmt.Sprintf("%s: %v", path, err)}
	}
	if scanned := scannedUsesKeys(text); scanned != parsed {
		return []string{fmt.Sprintf("%s: the YAML parser reads %d uses keys, the gate %d: write every uses: key plainly", path, parsed, scanned)}
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
