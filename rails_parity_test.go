package forge_test

// The drift gate of docs/RAILS_PARITY.md. The matrix is the source of truth for
// the Forge-Go and Forge-Rust parity promise (owner decision, 2026-10-20), so
// its rows must be checkable: every row has a stable ID, a status in a fixed
// vocabulary, a tracking issue for every capability that is not Done, a proof
// for every Forge-Rust Done, and only commands, flags and documents that exist.
//
// The rules are functions of the matrix text, so the negative tests below
// prove each rule on hand-written snippets. Nothing here uses the network: the
// Forge-Go commands come from the command catalog of internal/cli (what
// `forge help --json` prints) and documents are looked up on disk.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/cli"
)

// parityAllowTBD is the environment variable that lets a matrix keep
// `forge-go#TBD-slug` and `forge-rust#TBD-slug` placeholders: issues that are
// about to be filed. A change merges only after the numbers replace them.
const parityAllowTBD = "FORGE_PARITY_ALLOW_TBD"

const parityMatrixPath = "docs/RAILS_PARITY.md"

// parityStatuses is the status vocabulary of the matrix, longest first so that
// "Not applicable" is not read as "Not needed".
var parityStatuses = []string{"Not applicable", "Not needed", "In review", "Partial", "Planned", "Done"}

var (
	parityIDPattern   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	parityGoIssue     = regexp.MustCompile(`forge-go#(\d+|TBD-[a-z0-9]+(?:-[a-z0-9]+)*)`)
	parityRustIssue   = regexp.MustCompile(`forge-rust#(\d+|TBD-[a-z0-9]+(?:-[a-z0-9]+)*)`)
	parityPlaceholder = regexp.MustCompile(`forge-(?:go|rust)#TBD-[a-z0-9]+(?:-[a-z0-9]+)*`)
	parityCodeSpan    = regexp.MustCompile("`([^`]+)`")
	parityFlag        = regexp.MustCompile(`--[a-z][a-z0-9-]*`)
	parityLink        = regexp.MustCompile(`\]\(([^)\s]+)\)`)
)

// parityRow is one row of the matrix; Line is its 1-based line in the file.
type parityRow struct {
	Line                     int
	ID, Capability, Rails    string
	Go, Rust                 string
	GoStatus, RustStatus     string
	GoStatusOK, RustStatusOK bool
}

// splitParityCells splits a table line on its unescaped pipes and unescapes
// the pipes inside cells, so `forge dev\|test` is one backtick span.
func splitParityCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	var cells []string
	var cell strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line) && line[i+1] == '|':
			cell.WriteByte('|')
			i++
		case line[i] == '|':
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
		default:
			cell.WriteByte(line[i])
		}
	}
	return append(cells, strings.TrimSpace(cell.String()))
}

// parityStatus reads the status a cell starts with: one word of the
// vocabulary followed by the end of the cell or punctuation.
func parityStatus(cell string) (string, bool) {
	for _, status := range parityStatuses {
		rest, found := strings.CutPrefix(cell, status)
		if !found {
			continue
		}
		if rest == "" || strings.ContainsRune(" (,;:.", rune(rest[0])) {
			return status, true
		}
	}
	return "", false
}

// parseParityMatrix returns the rows of the first table of the document, whose
// header must be ID, Capability, Rails 8, Forge-Go, Forge-Rust, and the
// structural problems found on the way.
func parseParityMatrix(text string) ([]parityRow, []string) {
	var rows []parityRow
	var problems []string
	inTable := false
	for number, line := range strings.Split(text, "\n") {
		number++
		if !strings.HasPrefix(line, "|") {
			if inTable {
				break
			}
			continue
		}
		cells := splitParityCells(line)
		if !inTable {
			// The first table of the file is the status vocabulary; the matrix
			// is the one whose header starts with the ID column.
			if cells[0] != "ID" {
				continue
			}
			inTable = true
			if !slices.Equal(cells, []string{"ID", "Capability", "Rails 8", "Forge-Go", "Forge-Rust"}) {
				problems = append(problems, fmt.Sprintf("line %d: the header must be ID, Capability, Rails 8, Forge-Go, Forge-Rust", number))
			}
			continue
		}
		if strings.Trim(cells[0], "-: ") == "" {
			continue
		}
		if len(cells) != 5 {
			problems = append(problems, fmt.Sprintf("line %d: a row has %d cells, want 5 (escape a pipe inside a cell as \\|)", number, len(cells)))
			continue
		}
		row := parityRow{Line: number, ID: strings.Trim(cells[0], "`"), Capability: cells[1], Rails: cells[2], Go: cells[3], Rust: cells[4]}
		row.GoStatus, row.GoStatusOK = parityStatus(row.Go)
		row.RustStatus, row.RustStatusOK = parityStatus(row.Rust)
		rows = append(rows, row)
	}
	if !inTable {
		problems = append(problems, "no matrix table: a table whose first column is ID")
	}
	return rows, problems
}

// parityCatalog is what the Forge-Go command line offers: for every command
// name or alias, the flags it documents.
type parityCatalog struct {
	flags map[string]map[string]bool
	all   map[string]bool
}

type parityCatalogJSON struct {
	GlobalOptions []struct {
		Name string `json:"name"`
	} `json:"global_options"`
	Commands []struct {
		Name     string   `json:"name"`
		Aliases  []string `json:"aliases"`
		Usage    string   `json:"usage"`
		Summary  string   `json:"summary"`
		Examples []string `json:"examples"`
		Flags    []struct {
			Name string `json:"name"`
		} `json:"flags"`
	} `json:"commands"`
}

// newParityCatalog builds the catalog from the JSON `forge help --json`
// prints. A flag counts for a command when its flag list, usage, summary or
// examples name it: a subcommand's flag (`generate authentication --api-auth`)
// is documented in the usage or the examples, not in the flag list.
func newParityCatalog(helpJSON []byte) (parityCatalog, error) {
	var raw parityCatalogJSON
	if err := json.Unmarshal(helpJSON, &raw); err != nil {
		return parityCatalog{}, fmt.Errorf("the command catalog is not JSON: %w", err)
	}
	catalog := parityCatalog{flags: map[string]map[string]bool{}, all: map[string]bool{}}
	for _, option := range raw.GlobalOptions {
		for _, flag := range parityFlag.FindAllString(option.Name, -1) {
			catalog.all[flag] = true
		}
	}
	for _, command := range raw.Commands {
		flags := map[string]bool{}
		for _, flag := range command.Flags {
			flags["--"+strings.TrimLeft(flag.Name, "-")] = true
		}
		for _, text := range append([]string{command.Usage, command.Summary}, command.Examples...) {
			for _, flag := range parityFlag.FindAllString(text, -1) {
				flags[flag] = true
			}
		}
		for flag := range flags {
			catalog.all[flag] = true
		}
		catalog.flags[command.Name] = flags
		for _, alias := range command.Aliases {
			catalog.flags[alias] = flags
		}
	}
	if len(catalog.flags) == 0 {
		return parityCatalog{}, fmt.Errorf("the command catalog lists no command")
	}
	return catalog, nil
}

// forgeCatalog returns the live catalog: what `forge help --json` prints.
func forgeCatalog(t *testing.T) parityCatalog {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := cli.Run([]string{"help", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("forge help --json exited %d: %s", code, stderr.String())
	}
	catalog, err := newParityCatalog(stdout.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// spanClaims checks one backtick span of a Forge-Go cell. A span that starts
// with `forge COMMAND` must name commands the catalog lists (`a\|b` lists
// several) and flags that one of them documents. A span that is a bare flag
// must be documented by some command. It returns what the catalog lacks.
func (c parityCatalog) spanClaims(span string) []string {
	fields := strings.Fields(span)
	var missing []string
	switch {
	case len(fields) >= 2 && fields[0] == "forge" && !strings.HasPrefix(fields[1], "-") && !strings.HasPrefix(fields[1], "["):
		var documented []map[string]bool
		for name := range strings.SplitSeq(fields[1], "|") {
			flags, known := c.flags[name]
			if !known {
				missing = append(missing, "command forge "+name)
				continue
			}
			documented = append(documented, flags)
		}
		if len(documented) == 0 {
			return missing
		}
		for _, flag := range parityFlag.FindAllString(strings.Join(fields[2:], " "), -1) {
			if !slices.ContainsFunc(documented, func(flags map[string]bool) bool { return flags[flag] }) {
				missing = append(missing, "flag "+flag+" of forge "+fields[1])
			}
		}
	case len(fields) >= 1 && strings.HasPrefix(fields[0], "--"):
		for _, flag := range parityFlag.FindAllString(span, -1) {
			if !c.all[flag] {
				missing = append(missing, "flag "+flag)
			}
		}
	}
	return missing
}

// needsIssue reports whether a status must cite the issue that tracks it.
func needsIssue(status string) bool {
	return status == "Planned" || status == "Partial" || status == "In review"
}

// parityViolations applies every rule of the matrix to its text. allowTBD
// accepts the issue placeholders; exists says whether a path (relative to the
// document's directory) is a file or directory.
func parityViolations(text string, catalog parityCatalog, exists func(string) bool, allowTBD bool) []string {
	rows, problems := parseParityMatrix(text)
	seen := map[string]int{}
	for _, row := range rows {
		where := fmt.Sprintf("line %d (%s)", row.Line, row.ID)
		if !parityIDPattern.MatchString(row.ID) {
			problems = append(problems, fmt.Sprintf("line %d: the ID %q is not lower-case words joined by hyphens", row.Line, row.ID))
		} else if first, repeated := seen[row.ID]; repeated {
			problems = append(problems, fmt.Sprintf("%s: the ID repeats line %d", where, first))
		}
		seen[row.ID] = row.Line
		if !row.GoStatusOK {
			problems = append(problems, fmt.Sprintf("%s: the Forge-Go status must start with one of %s", where, strings.Join(parityStatuses, ", ")))
		}
		if !row.RustStatusOK {
			problems = append(problems, fmt.Sprintf("%s: the Forge-Rust status must start with one of %s", where, strings.Join(parityStatuses, ", ")))
		}
		if row.RustStatusOK && needsIssue(row.RustStatus) && !parityRustIssue.MatchString(row.Rust) {
			problems = append(problems, fmt.Sprintf("%s: Forge-Rust is %s and cites no forge-rust#NN issue", where, row.RustStatus))
		}
		if row.GoStatusOK && needsIssue(row.GoStatus) && !parityGoIssue.MatchString(row.Go) {
			problems = append(problems, fmt.Sprintf("%s: Forge-Go is %s and cites no forge-go#NN issue", where, row.GoStatus))
		}
		if row.RustStatus == "Done" && !parityCodeSpan.MatchString(row.Rust) {
			problems = append(problems, fmt.Sprintf("%s: Forge-Rust is Done and names no command, file or setting in backticks as proof", where))
		}
		if row.GoStatus == "Done" {
			for _, span := range parityCodeSpan.FindAllStringSubmatch(row.Go, -1) {
				for _, missing := range catalog.spanClaims(span[1]) {
					problems = append(problems, fmt.Sprintf("%s: Forge-Go is Done but `forge help --json` has no %s (in `%s`)", where, missing, span[1]))
				}
			}
		}
		if row.GoStatus == "Planned" {
			for _, span := range parityCodeSpan.FindAllStringSubmatch(row.Go, -1) {
				if parityFlag.MatchString(span[1]) && strings.HasPrefix(span[1], "forge ") && len(catalog.spanClaims(span[1])) == 0 {
					problems = append(problems, fmt.Sprintf("%s: Forge-Go is Planned but `%s` already exists: mark it Done", where, span[1]))
				}
			}
		}
	}
	if len(rows) == 0 {
		problems = append(problems, "the matrix has no rows")
	}
	if !allowTBD {
		found := parityPlaceholder.FindAllString(text, -1)
		slices.Sort(found)
		for _, placeholder := range slices.Compact(found) {
			problems = append(problems, fmt.Sprintf("%s is a placeholder: file the issue and write its number (%s=1 allows placeholders while issues are filed)", placeholder, parityAllowTBD))
		}
	}
	for _, link := range parityLink.FindAllStringSubmatch(text, -1) {
		target := link[1]
		if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
			continue
		}
		path, _, _ := strings.Cut(target, "#")
		if !exists(path) {
			problems = append(problems, fmt.Sprintf("the link %q points to a document that does not exist", target))
		}
	}
	return problems
}

func TestRailsParityMatrix(t *testing.T) {
	text, err := os.ReadFile(parityMatrixPath)
	if err != nil {
		t.Fatal(err)
	}
	exists := func(path string) bool {
		_, err := os.Stat(filepath.Join(filepath.Dir(parityMatrixPath), filepath.FromSlash(path)))
		return err == nil
	}
	for _, problem := range parityViolations(string(text), forgeCatalog(t), exists, os.Getenv(parityAllowTBD) == "1") {
		t.Error(problem)
	}
}

// paritySample is a valid matrix every negative test breaks in one way.
const paritySample = `# Parity

| Status | Meaning |
| --- | --- |
| Done | on main |

| ID | Capability | Rails 8 | Forge-Go | Forge-Rust |
| --- | --- | --- | --- | --- |
| ` + "`new-app`" + ` | New application | ` + "`rails new`" + ` | Done (` + "`forge new --api`" + `, [guide](GUIDE.md#top)) | Done (` + "`forge new --api`" + `) |
| ` + "`jobs-switch`" + ` | Wake-up | — | Planned ([forge-go#72](https://example.test/72)) | Done (` + "`FORGE_JOBS_NOTIFY`" + `) |
| ` + "`later`" + ` | Later | — | Done (` + "`forge dev\\|test\\|db --no-reload`" + `) | Planned, after forge-go 100% (D37; forge-rust#97) |
| ` + "`ts`" + ` | Partial | — | Done | Partial (forge-rust#91; ` + "`--x`" + ` missing) |
`

func paritySampleCatalog(t *testing.T) parityCatalog {
	t.Helper()
	catalog, err := newParityCatalog([]byte(`{"global_options":[{"name":"--error-format=json"}],"commands":[
		{"name":"new","usage":"forge new NAME [flags]","flags":[{"name":"--api"}]},
		{"name":"dev","usage":"forge dev [--no-reload]","flags":[{"name":"--no-reload"}]},
		{"name":"test","usage":"forge test","flags":[]},
		{"name":"db","aliases":["database"],"usage":"forge db status","flags":[]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func sampleExists(path string) bool { return path == "GUIDE.md" }

func TestParitySampleIsClean(t *testing.T) {
	if problems := parityViolations(paritySample, paritySampleCatalog(t), sampleExists, false); len(problems) != 0 {
		t.Fatalf("the valid sample was rejected: %v", problems)
	}
}

// TestParityRulesFire proves that every rule rejects the way it exists to
// reject, on the sample with one change each.
func TestParityRulesFire(t *testing.T) {
	catalog := paritySampleCatalog(t)
	for name, test := range map[string]struct {
		edit func(string) string
		want string
	}{
		"a Rust Planned row without an issue": {
			func(s string) string {
				return strings.Replace(s, "Planned, after forge-go 100% (D37; forge-rust#97)", "Planned, after forge-go 100% (D37)", 1)
			},
			"Forge-Rust is Planned and cites no forge-rust#NN issue",
		},
		"a Rust Partial row without an issue": {
			func(s string) string { return strings.Replace(s, "Partial (forge-rust#91;", "Partial (", 1) },
			"Forge-Rust is Partial and cites no forge-rust#NN issue",
		},
		"a Rust In review row without an issue": {
			func(s string) string {
				return strings.Replace(s, "Planned, after forge-go 100% (D37; forge-rust#97)", "In review (a pull request)", 1)
			},
			"Forge-Rust is In review and cites no forge-rust#NN issue",
		},
		"a Go Planned row without an issue": {
			func(s string) string {
				return strings.Replace(s, "Planned ([forge-go#72](https://example.test/72))", "Planned (soon)", 1)
			},
			"Forge-Go is Planned and cites no forge-go#NN issue",
		},
		"a Rust Done row without proof": {
			func(s string) string { return strings.Replace(s, "Done (`FORGE_JOBS_NOTIFY`)", "Done", 1) },
			"Forge-Rust is Done and names no command, file or setting",
		},
		"a Go command the CLI lacks": {
			func(s string) string {
				return strings.Replace(s, "`forge dev\\|test\\|db --no-reload`", "`forge dev\\|nope --no-reload`", 1)
			},
			"has no command forge nope",
		},
		"a Go flag the command lacks": {
			func(s string) string {
				return strings.Replace(s, "`forge new --api`, [guide]", "`forge new --skip-docker`, [guide]", 1)
			},
			"has no flag --skip-docker of forge new",
		},
		"a bare Go flag no command has": {
			func(s string) string {
				return strings.Replace(s, "Done (`forge new --api`, [guide]", "Done (`--ghost`, [guide]", 1)
			},
			"has no flag --ghost",
		},
		"a Planned Go row whose command exists": {
			func(s string) string {
				return strings.Replace(s, "Planned ([forge-go#72](https://example.test/72))", "Planned ([forge-go#72](https://example.test/72), `forge new --api`)", 1)
			},
			"already exists: mark it Done",
		},
		"a link to a missing document": {
			func(s string) string { return strings.Replace(s, "GUIDE.md#top", "MISSING.md#top", 1) },
			"points to a document that does not exist",
		},
		"a placeholder": {
			func(s string) string { return strings.Replace(s, "forge-rust#97", "forge-rust#TBD-typescript", 1) },
			"forge-rust#TBD-typescript is a placeholder",
		},
		"a Go placeholder": {
			func(s string) string {
				return strings.Replace(s, "[forge-go#72](https://example.test/72)", "forge-go#TBD-switch", 1)
			},
			"forge-go#TBD-switch is a placeholder",
		},
		"an unknown status": {
			func(s string) string {
				return strings.Replace(s, "Planned ([forge-go#72]", "Pending ([forge-go#72]", 1)
			},
			"the Forge-Go status must start with one of",
		},
		"a status glued to a word": {
			func(s string) string {
				return strings.Replace(s, "| Done (`forge new --api`) |", "| Donezo (`forge new --api`) |", 1)
			},
			"the Forge-Rust status must start with one of",
		},
		"a repeated ID": {
			func(s string) string { return strings.Replace(s, "`later`", "`new-app`", 1) },
			"the ID repeats line",
		},
		"a malformed ID": {
			func(s string) string { return strings.Replace(s, "`later`", "`Later_One`", 1) },
			"is not lower-case words joined by hyphens",
		},
		"a row with a missing cell": {
			func(s string) string { return strings.Replace(s, "| — | Done | Partial", "| Done | Partial", 1) },
			"a row has 4 cells",
		},
		"a wrong header": {
			func(s string) string { return strings.Replace(s, "| ID | Capability |", "| ID | Feature |", 1) },
			"the header must be",
		},
	} {
		t.Run(name, func(t *testing.T) {
			problems := parityViolations(test.edit(paritySample), catalog, sampleExists, false)
			if !slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, test.want) }) {
				t.Fatalf("no problem contains %q: %q", test.want, problems)
			}
		})
	}
}

// TestParityPlaceholdersAreAllowedOnRequest proves that the escape hatch
// accepts a placeholder and nothing else.
func TestParityPlaceholdersAreAllowedOnRequest(t *testing.T) {
	catalog := paritySampleCatalog(t)
	text := strings.Replace(paritySample, "forge-rust#97", "forge-rust#TBD-typescript", 1)
	if problems := parityViolations(text, catalog, sampleExists, true); len(problems) != 0 {
		t.Fatalf("a placeholder was rejected although allowed: %v", problems)
	}
	// A placeholder is an issue reference, so a cell that has none still fails.
	text = strings.Replace(paritySample, "Planned, after forge-go 100% (D37; forge-rust#97)", "Planned", 1)
	if problems := parityViolations(text, catalog, sampleExists, true); len(problems) == 0 {
		t.Fatal("a Planned row without an issue was accepted")
	}
}

// TestParityCellsKeepEscapedPipes proves that a pipe inside a code span is part
// of the cell, so `forge dev|test` is one claim.
func TestParityCellsKeepEscapedPipes(t *testing.T) {
	cells := splitParityCells("| `a` | `forge dev\\|test` | x |")
	if want := []string{"`a`", "`forge dev|test`", "x"}; !slices.Equal(cells, want) {
		t.Fatalf("cells = %q, want %q", cells, want)
	}
}

func TestParityCatalogRejectsNonJSON(t *testing.T) {
	if _, err := newParityCatalog([]byte("not json")); err == nil {
		t.Fatal("a catalog that is not JSON was accepted")
	}
	if _, err := newParityCatalog([]byte(`{"commands":[]}`)); err == nil {
		t.Fatal("a catalog without commands was accepted")
	}
}
