package forge_test

import (
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Every lint suppression states why it is safe (audit M-4, gap 11): a
// `#nosec` carries gosec rule IDs and a `-- reason`, a `nolint` names its
// linters and carries a `// reason`. A suppression with no reason is a
// finding somebody stopped reading.
var (
	// The form is: #nosec, one or more gosec rule IDs separated by commas or
	// spaces, two dashes and the reason.
	nosecForm = regexp.MustCompile(`^#nosec G\d{3}([, ]G\d{3})* -- \S`)
	// The form is: nolint, a colon, the linters, a second comment marker and
	// the reason.
	nolintForm = regexp.MustCompile(`^//nolint:[a-z][a-z0-9-]*(,[a-z][a-z0-9-]*)* // \S`)
	nolintWord = regexp.MustCompile(`//\s*nolint`)
	// reasonWords is the least a reason says: more than one word.
	reasonWords = regexp.MustCompile(`\S+\s+\S+`)
)

// directiveViolations scans the comments of one Go source and returns the
// suppressions that lack a reason, naming file and line. Only comments are
// read, so a string literal that mentions the directive is not one.
func directiveViolations(shown, source string) (violations []string, directives int) {
	file := token.NewFileSet().AddFile(shown, -1, len(source))
	var lexer scanner.Scanner
	// Template placeholders and fixtures are not valid Go; comments are still
	// found, and the scanner's errors are not this test's business.
	lexer.Init(file, []byte(source), func(token.Position, string) {}, scanner.ScanComments)
	for {
		position, kind, text := lexer.Scan()
		if kind == token.EOF {
			return violations, directives
		}
		if kind != token.COMMENT {
			continue
		}
		line := file.Position(position).Line
		where := shown + ":" + strconv.Itoa(line)
		// A comment that merely mentions the word is prose; one that starts
		// with it is the directive.
		if body := strings.TrimSpace(strings.TrimPrefix(text, "//")); strings.HasPrefix(body, "#nosec") {
			directives++
			if !nosecForm.MatchString(body) || !reasonWords.MatchString(body[strings.Index(body, "-- ")+3:]) {
				violations = append(violations, where+": #nosec needs gosec rule IDs and a reason: `#nosec G204 -- why it is safe`: "+text)
			}
		}
		if loc := nolintWord.FindStringIndex(text); loc != nil {
			directives++
			switch {
			case loc[0] != 0:
				violations = append(violations, where+": a nolint directive must start its comment, or the linter ignores it: "+text)
			case !nolintForm.MatchString(text):
				violations = append(violations, where+": nolint needs the linters it silences and a reason: `//nolint:linter // why it is safe`: "+text)
			case !reasonWords.MatchString(text[strings.Index(text, " // ")+4:]):
				violations = append(violations, where+": the nolint reason must say more than one word: "+text)
			}
		}
	}
}

func TestSuppressionsStateTheirReason(t *testing.T) {
	skipped := []string{".git", "vendor", "dist", "node_modules"}
	var problems []string
	var files, directives int
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if slices.Contains(skipped, entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		// Go sources, and the templates and fixtures that generate Go sources.
		if name := entry.Name(); !strings.HasSuffix(name, ".go") && !strings.Contains(name, ".go.") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found, count := directiveViolations(filepath.ToSlash(path), string(content))
		problems = append(problems, found...)
		directives += count
		files++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		t.Error(problem)
	}
	// A scan that finds nothing proves nothing: the repository has dozens.
	if files < 200 || directives < 50 {
		t.Errorf("scanned %d files and found %d suppressions: the scan is not reaching the sources", files, directives)
	}
}

func TestSuppressionRulesCatchWhatTheyAreMeantTo(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		source string
		want   int // violations
	}{
		"nosec with reason":            {"package p\nvar _ = 1 // #nosec G204 -- a fixed executable\n", 0},
		"nosec with several rules":     {"package p\nvar _ = 1 // #nosec G204,G304 -- fixed paths only\n", 0},
		"bare nosec":                   {"package p\nvar _ = 1 // #nosec\n", 1},
		"nosec without reason":         {"package p\nvar _ = 1 // #nosec G204\n", 1},
		"nosec with empty reason":      {"package p\nvar _ = 1 // #nosec G204 --\n", 1},
		"nosec with one word":          {"package p\nvar _ = 1 // #nosec G204 -- trust\n", 1},
		"nosec without rule":           {"package p\nvar _ = 1 // #nosec -- it is fine really\n", 1},
		"nolint with reason":           {"package p\nvar _ = 1 //nolint:noctx // owned by Stop, not a context\n", 0},
		"nolint with several linters":  {"package p\nvar _ = 1 //nolint:noctx,nilerr // owned by Stop, not a context\n", 0},
		"bare nolint":                  {"package p\nvar _ = 1 //nolint\n", 1},
		"nolint without reason":        {"package p\nvar _ = 1 //nolint:noctx\n", 1},
		"nolint with empty reason":     {"package p\nvar _ = 1 //nolint:noctx //\n", 1},
		"nolint with one-word reason":  {"package p\nvar _ = 1 //nolint:noctx // deliberate\n", 1},
		"nolint with a space":          {"package p\nvar _ = 1 // nolint:noctx // owned by Stop, not a context\n", 1},
		"nolint inside another note":   {"package p\nvar _ = 1 // #nosec G204 -- fixed //nolint:noctx // owned by Stop\n", 1},
		"nolint on its own line":       {"package p\n//nolint:noctx // the lifetime is owned by Stop\nvar _ = 1\n", 0},
		"string literals are not code": {"package p\nvar _ = \"#nosec\" + \"//nolint\"\n", 0},
		"plain comments are ignored":   {"package p\n// nothing to suppress here\nvar _ = 1\n", 0},
	} {
		got, _ := directiveViolations("fixture.go", test.source)
		if len(got) != test.want {
			t.Errorf("%s: %d violations %v, want %d", name, len(got), got, test.want)
		}
	}
}
