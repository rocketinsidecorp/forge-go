package cli

import (
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckFrameworkPathAcceptsOnlyTheFrameworkModule(t *testing.T) {
	t.Parallel()
	write := func(content string) string {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "go.mod"), content)
		return dir
	}
	good := write("// the framework\nmodule github.com/rocketinsidecorp/forge-go\n\ngo 1.27.1\n")
	if got, err := checkFrameworkPath(good); err != nil || got != good {
		t.Errorf("the framework checkout: %q, %v", got, err)
	}
	for name, tc := range map[string]struct{ path, want string }{
		"another module":        {write("module example.com/other\n"), "contains module example.com/other"},
		"no module directive":   {write("go 1.27.1\n"), "has no module directive"},
		"no go.mod":             {t.TempDir(), "framework path:"},
		"no such directory":     {filepath.Join(t.TempDir(), "gone"), "framework path:"},
		"go.mod is a directory": {func() string { d := t.TempDir(); _ = os.Mkdir(filepath.Join(d, "go.mod"), 0o750); return d }(), ""},
	} {
		got, err := checkFrameworkPath(tc.path)
		if err == nil || got != "" || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %q, %v, want %q", name, got, err, tc.want)
		}
	}
}

func TestResolveDependenciesNamesTheStepThatFailed(t *testing.T) {
	t.Parallel()
	// A directory that is no module: the first step of either flavor fails
	// without touching the network.
	var out strings.Builder
	err := resolveDependencies(context.Background(), t.TempDir(), "", &out, io.Discard)
	if err == nil || !strings.HasPrefix(err.Error(), "go get: ") || !strings.Contains(out.String(), "run     go get github.com/rocketinsidecorp/forge-go@") {
		t.Errorf("a release: err %v, output %q", err, out.String())
	}
	out.Reset()
	err = resolveDependencies(context.Background(), t.TempDir(), "/checkout/forge-go", &out, io.Discard)
	if err == nil || !strings.HasPrefix(err.Error(), "go mod: ") || !strings.Contains(out.String(), "-replace=github.com/rocketinsidecorp/forge-go=/checkout/forge-go") {
		t.Errorf("a local checkout: err %v, output %q", err, out.String())
	}
}

func TestParseSinglePositionalAcceptsTheNameBeforeOrAfterFlags(t *testing.T) {
	t.Parallel()
	newFlags := func() *flag.FlagSet {
		flags := flag.NewFlagSet("x", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		flags.Bool("v", false, "")
		return flags
	}
	for name, tc := range map[string]struct {
		args []string
		want string
		bad  bool
	}{
		"name first":          {[]string{"app", "-v"}, "app", false},
		"name last":           {[]string{"-v", "app"}, "app", false},
		"name only":           {[]string{"app"}, "app", false},
		"two names":           {[]string{"app", "other"}, "", true},
		"flag after the name": {[]string{"app", "-wat"}, "", true},
		"flag before":         {[]string{"-wat", "app"}, "", true},
		"no name":             {[]string{"-v"}, "", true},
		"nothing":             {nil, "", true},
	} {
		got, err := parseSinglePositional(newFlags(), tc.args, "NAME")
		if (err != nil) != tc.bad || got != tc.want {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
}

func TestRunNewRefusesConflictingOrUnusableArguments(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"skip-database and database": {[]string{"shop", "--skip-database", "--database", "mysql", "--skip-deps", "--dir", filepath.Join(dir, "a")}, "conflicts"},
		"skip-database and -d":       {[]string{"shop", "--skip-database", "-d", "sqlite", "--skip-deps", "--dir", filepath.Join(dir, "b")}, "conflicts"},
		"framework path is missing":  {[]string{"shop", "--framework-path", filepath.Join(dir, "none"), "--skip-deps", "--dir", filepath.Join(dir, "c")}, "framework path:"},
		"target directory in use": {func() []string {
			used := filepath.Join(dir, "used")
			writeFile(t, filepath.Join(used, "keep.txt"), "x")
			return []string{"shop", "--skip-deps", "--dir", used}
		}(), "not empty"},
		"unknown database": {[]string{"shop", "-d", "oracle", "--skip-deps", "--dir", filepath.Join(dir, "d")}, "oracle"},
		"unknown locale":   {[]string{"shop", "--locale", "fr", "--skip-deps", "--dir", filepath.Join(dir, "e")}, "unsupported locale"},
	} {
		var out strings.Builder
		if err := runNew(tc.args, &out, io.Discard); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	// --skip-database generates an application without a database.
	target := filepath.Join(dir, "plain")
	var out strings.Builder
	if err := runNew([]string{"plain", "--skip-database", "--skip-deps", "--dir", target}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "db")); !os.IsNotExist(err) {
		t.Errorf("an application without a database has a db directory: %v", err)
	}
	if !strings.Contains(out.String(), "Next steps:") {
		t.Errorf("output %q", out.String())
	}
}
