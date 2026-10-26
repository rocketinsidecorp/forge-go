package forge_test

import (
	"go/ast"
	"go/parser"
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

// A test that sleeps to let something happen is a race that a loaded CI runner
// loses (audit T-1, T-2). Wait for the event instead (a channel, a marker
// file, internal/waittest.Until) or run the code in a testing/synctest bubble
// where the clock is fake. What is left carries a reason:
//
//   - a real-clock marker (a comment on the sleep's line or the line above that
//     starts with "real-clock:" and gives a reason) names why only the real
//     clock will do: an expiry stamped by a database or a real HTTP server, a
//     helper process that waits to be killed, a negative check;
//   - a fake-clock marker is for a sleep inside a synctest bubble, which costs
//     no real time and is deterministic. The marker is valid only there, and a
//     real-clock marker is invalid there.
//
// maxRealClockSleeps is a ratchet: it only goes down.
const maxRealClockSleeps = 43

// sleepExemptDirectories hold tests that another change converts to synctest.
// Their sleeps are counted against the ratchet but need no marker yet. Remove
// an entry when its package is converted. None is left: jobs and cache were
// converted (QG9).
var sleepExemptDirectories = []string{}

var (
	realClockMarker = regexp.MustCompile(`^real-clock: \S+ \S+`)
	fakeClockMarker = regexp.MustCompile(`^fake-clock: \S+ \S+`)
)

// sleepFindings parses one test file and returns what is wrong with its
// time.Sleep calls, plus how many carry a real-clock marker and how many are
// unmarked and exempt.
func sleepFindings(shown string, source []byte, exempt bool) (problems []string, realClock, exempted int) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, shown, source, parser.ParseComments)
	if err != nil {
		return []string{shown + ": " + err.Error()}, 0, 0
	}
	// The comment text that starts on each line, keyed by line: a marker is a
	// comment on the sleep's own line or the line above it.
	comments := map[int]string{}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			line := fset.Position(comment.Pos()).Line
			comments[line] = strings.TrimSpace(strings.TrimPrefix(comment.Text, "//"))
		}
	}
	marker := func(line int) string {
		if text, ok := comments[line]; ok {
			return text
		}
		return comments[line-1]
	}
	var bubbles []ast.Node
	visit := func(node ast.Node) bool {
		if node == nil {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
			if pkg, ok := selector.X.(*ast.Ident); ok {
				switch {
				case pkg.Name == "synctest" && (selector.Sel.Name == "Test" || selector.Sel.Name == "Run"):
					bubbles = append(bubbles, call)
				case pkg.Name == "time" && selector.Sel.Name == "Sleep":
					line := fset.Position(call.Pos()).Line
					where := shown + ":" + strconv.Itoa(line)
					text := marker(line)
					inBubble := slices.ContainsFunc(bubbles, func(b ast.Node) bool { return b.Pos() <= call.Pos() && call.End() <= b.End() })
					switch {
					case fakeClockMarker.MatchString(text):
						if !inBubble {
							problems = append(problems, where+": a fake-clock marker is only valid inside a synctest bubble; this sleep runs on the real clock")
						}
					case realClockMarker.MatchString(text):
						if inBubble {
							problems = append(problems, where+": this sleep is inside a synctest bubble, so it is a fake-clock sleep; mark it `fake-clock: reason`")
						}
						realClock++
					case exempt:
						exempted++
					default:
						problems = append(problems, where+": time.Sleep in a test needs a comment `real-clock: reason` (or `fake-clock: reason` inside a synctest bubble); prefer waittest.Until or a synctest bubble")
					}
				}
			}
		}
		return true
	}
	ast.Inspect(file, visit)
	return problems, realClock, exempted
}

func TestTestSleepsAreAnnotatedAndBudgeted(t *testing.T) {
	skipped := []string{".git", "vendor", "dist", "node_modules"}
	var problems []string
	var realClock, exempted, files int
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
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		slash := filepath.ToSlash(path)
		exempt := slices.ContainsFunc(sleepExemptDirectories, func(dir string) bool { return strings.HasPrefix(slash, dir+"/") })
		found, real, free := sleepFindings(slash, source, exempt)
		problems = append(problems, found...)
		realClock += real
		exempted += free
		files++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		t.Error(problem)
	}
	if files < 200 {
		t.Errorf("scanned %d test files: the scan is not reaching the tests", files)
	}
	if total := realClock + exempted; total > maxRealClockSleeps {
		t.Errorf("%d real-clock sleeps in tests (%d marked, %d in exempt directories), budget %d: wait for the event (internal/waittest) instead of adding one", total, realClock, exempted, maxRealClockSleeps)
	} else {
		t.Logf("%d real-clock sleeps in tests (%d marked, %d in exempt directories), budget %d", total, realClock, exempted, maxRealClockSleeps)
	}
}

func TestSleepRulesCatchWhatTheyAreMeantTo(t *testing.T) {
	t.Parallel()
	const header = "package p\nimport (\n\t\"testing\"\n\t\"testing/synctest\"\n\t\"time\"\n)\n"
	for name, test := range map[string]struct {
		body       string
		exempt     bool
		wantIssues int
		wantReal   int
		wantFree   int
	}{
		"bare sleep":                   {"func T(t *testing.T) { time.Sleep(time.Second) }", false, 1, 0, 0},
		"bare sleep in exempt dir":     {"func T(t *testing.T) { time.Sleep(time.Second) }", true, 0, 0, 1},
		"trailing real-clock":          {"func T(t *testing.T) { time.Sleep(time.Second) // real-clock: the database stamps the expiry\n}", false, 0, 1, 0},
		"real-clock on the line above": {"func T(t *testing.T) {\n\t// real-clock: the database stamps the expiry\n\ttime.Sleep(time.Second)\n}", false, 0, 1, 0},
		"real-clock without reason":    {"func T(t *testing.T) { time.Sleep(time.Second) // real-clock:\n}", false, 1, 0, 0},
		"real-clock with one word":     {"func T(t *testing.T) { time.Sleep(time.Second) // real-clock: expiry\n}", false, 1, 0, 0},
		"plain comment":                {"func T(t *testing.T) { time.Sleep(time.Second) // wait a bit\n}", false, 1, 0, 0},
		"fake-clock outside bubble":    {"func T(t *testing.T) { time.Sleep(time.Second) // fake-clock: runs in a bubble\n}", false, 1, 0, 0},
		"fake-clock inside bubble":     {"func T(t *testing.T) { synctest.Test(t, func(t *testing.T) {\n\ttime.Sleep(time.Second) // fake-clock: runs in a bubble\n}) }", false, 0, 0, 0},
		"real-clock inside bubble":     {"func T(t *testing.T) { synctest.Test(t, func(t *testing.T) {\n\ttime.Sleep(time.Second) // real-clock: the database stamps it\n}) }", false, 1, 1, 0},
		"bare sleep inside bubble":     {"func T(t *testing.T) { synctest.Test(t, func(t *testing.T) {\n\ttime.Sleep(time.Second)\n}) }", false, 1, 0, 0},
		"sleep after the bubble":       {"func T(t *testing.T) { synctest.Test(t, func(t *testing.T) {}); time.Sleep(time.Second) // fake-clock: wrongly claims a bubble\n}", false, 1, 0, 0},
		"no sleep":                     {"func T(t *testing.T) { _ = time.Now() }", false, 0, 0, 0},
	} {
		problems, real, free := sleepFindings("fixture_test.go", []byte(header+test.body), test.exempt)
		if len(problems) != test.wantIssues || real != test.wantReal || free != test.wantFree {
			t.Errorf("%s: problems %v, real-clock %d, exempt %d; want %d, %d, %d", name, problems, real, free, test.wantIssues, test.wantReal, test.wantFree)
		}
	}
}
