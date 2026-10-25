package cli

import (
	"bytes"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoutesNeedAnApplicationAndAWritableOutput(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := runRoutesAt([]string{"--json"}, &out, t.TempDir()); err == nil || !strings.Contains(err.Error(), "usage: forge routes") {
		t.Errorf("arguments: %v", err)
	}
	if err := runRoutesAt(nil, &out, filepath.Join(t.TempDir(), "gone")); err == nil || !strings.Contains(err.Error(), "go.mod not found") {
		t.Errorf("a missing directory: %v", err)
	}
	if err := runRoutesAt(nil, &out, t.TempDir()); err == nil || !strings.Contains(err.Error(), "go.mod not found") {
		t.Errorf("no go.mod: %v", err)
	}
	if err := runRoutesAt(nil, failingWriter{}, writeRoutesProject(t)); err != errBrokenPipe {
		t.Errorf("a failing output: %v", err)
	}
}

func TestDiscoverRoutesReadsOnlyLiteralRegistrations(t *testing.T) {
	t.Parallel()
	source := `package app

func routes(app interface{ HandleFunc(string, any); Handle(any, any); Resources(string, any) }, name string) {
	plain()
	app.Handle(name, nil)
	app.Handle(42, nil)
	app.HandleFunc("   ", nil)
	app.HandleFunc("/any", nil)
	app.HandleFunc("GET /posts/{id}", nil)
	app.Resources("/posts", nil)
	app.Resources("/no-handler")
	app.Resources("posts-without-slash", nil)
	app.Other("GET /ignored", nil)
}
`
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "routes.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	routes := discoverRoutes(fileSet, file)
	var patterns []string
	for _, route := range routes {
		patterns = append(patterns, route.pattern)
	}
	joined := strings.Join(patterns, "|")
	if !strings.HasPrefix(joined, "/any|GET /posts/{id}|GET /posts") {
		t.Errorf("patterns = %s", joined)
	}
	for _, unwanted := range []string{"ignored", "no-handler", "without-slash"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("%q must not be discovered: %s", unwanted, joined)
		}
	}
	for _, route := range routes {
		if route.line == 0 {
			t.Errorf("%s has no line", route.pattern)
		}
	}
	// Without a file set the lines are unknown.
	for _, route := range discoverRoutes(nil, file) {
		if route.line != 0 {
			t.Errorf("%s has line %d without a file set", route.pattern, route.line)
		}
	}
}

func TestSplitRoutePatternTreatsAMethodlessPatternAsAnyMethod(t *testing.T) {
	t.Parallel()
	for pattern, want := range map[string][2]string{
		"GET /posts":  {"GET", "/posts"},
		"/health":     {"ANY", "/health"},
		"  /padded  ": {"ANY", "/padded"},
		"POST   /a":   {"POST", "/a"},
		"a b c":       {"ANY", "a b c"},
	} {
		if method, path := splitRoutePattern(pattern); method != want[0] || path != want[1] {
			t.Errorf("splitRoutePattern(%q) = %q %q, want %q %q", pattern, method, path, want[0], want[1])
		}
	}
}

func TestRunHelpPrintsUsageWithoutArguments(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := runHelp(nil, &out); err != nil || !strings.Contains(out.String(), "usage: forge") {
		t.Errorf("%v\n%s", err, out.String())
	}
}
