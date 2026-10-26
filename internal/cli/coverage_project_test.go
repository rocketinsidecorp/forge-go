package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
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

// recordGoSteps replaces runGoStep so no go command runs, and returns what it
// was asked to run. fail names the first argument of a step that returns an
// error. Not for parallel tests: runGoStep is a package variable.
func recordGoSteps(t *testing.T, fail string) *[][]string {
	t.Helper()
	previous := runGoStep
	t.Cleanup(func() { runGoStep = previous })
	var steps [][]string
	runGoStep = func(_ context.Context, _ string, _, _ io.Writer, args ...string) error {
		steps = append(steps, append([]string(nil), args...))
		if args[0] == fail {
			return errors.New("simulated failure")
		}
		return nil
	}
	return &steps
}

func TestResolveDependenciesNamesTheStepThatFailed(t *testing.T) {
	// Not parallel: runGoStep is replaced.
	steps := recordGoSteps(t, "get")
	var out strings.Builder
	err := resolveDependencies(context.Background(), t.TempDir(), "", &out, io.Discard)
	if err == nil || !strings.HasPrefix(err.Error(), "go get: ") || !strings.Contains(out.String(), "run     go get github.com/rocketinsidecorp/forge-go@") || len(*steps) != 1 {
		t.Errorf("a release: err %v, output %q, steps %v", err, out.String(), *steps)
	}
	steps = recordGoSteps(t, "mod")
	out.Reset()
	err = resolveDependencies(context.Background(), t.TempDir(), "/checkout/forge-go", &out, io.Discard)
	if err == nil || !strings.HasPrefix(err.Error(), "go mod: ") || !strings.Contains(out.String(), "-replace=github.com/rocketinsidecorp/forge-go=/checkout/forge-go") || len(*steps) != 1 {
		t.Errorf("a local checkout: err %v, output %q, steps %v", err, out.String(), *steps)
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

func buildInfoOf(path, version, sum string) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Path: path, Version: version, Sum: sum}}, true
	}
}

func TestFrameworkTargetPinsTheVersionTheCLIWasBuiltFrom(t *testing.T) {
	t.Parallel()
	const module = "github.com/rocketinsidecorp/forge-go"
	const pseudo = "v0.0.0-20261024123456-0123456789ab"
	const sum = "h1:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG="
	for name, tc := range map[string]struct {
		ldflags string
		build   func() (*debug.BuildInfo, bool)
		want    string
	}{
		"ldflags release wins":              {"v1.2.3", buildInfoOf(module, pseudo, sum), "v1.2.3"},
		"ldflags prerelease":                {"v1.2.3-rc.1", buildInfoOf(module, "", ""), "v1.2.3-rc.1"},
		"proxy install of a pseudo":         {"dev", buildInfoOf(module, pseudo, sum), pseudo},
		"proxy install of a tag":            {"dev", buildInfoOf(module, "v0.4.0", sum), "v0.4.0"},
		"checkout build, VCS pseudo":        {"dev", buildInfoOf(module, pseudo, ""), "latest"},
		"checkout build, VCS tag":           {"dev", buildInfoOf(module, "v0.4.0", ""), "latest"},
		"build of a working tree":           {"dev", buildInfoOf(module, "(devel)", ""), "latest"},
		"devel even with a sum":             {"dev", buildInfoOf(module, "(devel)", sum), "latest"},
		"dirty vcs stamp":                   {"dev", buildInfoOf(module, pseudo+"+dirty", sum), "latest"},
		"empty module version":              {"dev", buildInfoOf(module, "", sum), "latest"},
		"another main module":               {"dev", buildInfoOf("example.com/fork", pseudo, sum), "latest"},
		"no build info":                     {"dev", func() (*debug.BuildInfo, bool) { return nil, false }, "latest"},
		"nil build info":                    {"dev", func() (*debug.BuildInfo, bool) { return nil, true }, "latest"},
		"unknown ldflags ignore build info": {"nightly", buildInfoOf(module, pseudo, sum), "latest"},
	} {
		if got := frameworkTarget(tc.ldflags, tc.build); got != tc.want {
			t.Errorf("%s: frameworkTarget(%q) = %q, want %q", name, tc.ldflags, got, tc.want)
		}
	}
}

func TestEffectiveVersionReportsTheProxyVersionOnlyForUnstampedBuilds(t *testing.T) {
	t.Parallel()
	const module = "github.com/rocketinsidecorp/forge-go"
	const pseudo = "v0.0.0-20261024123456-0123456789ab"
	for name, tc := range map[string]struct {
		ldflags string
		build   func() (*debug.BuildInfo, bool)
		want    string
	}{
		"stamped release":       {"v1.2.3", buildInfoOf(module, pseudo, "h1:x="), "v1.2.3"},
		"proxy install":         {"dev", buildInfoOf(module, pseudo, "h1:x="), pseudo},
		"checkout build":        {"dev", buildInfoOf(module, pseudo, ""), "dev"},
		"working tree":          {"dev", buildInfoOf(module, "(devel)", ""), "dev"},
		"no build info":         {"dev", func() (*debug.BuildInfo, bool) { return nil, false }, "dev"},
		"custom stamp is shown": {"nightly", buildInfoOf(module, pseudo, "h1:x="), "nightly"},
	} {
		if got := effectiveVersion(tc.ldflags, tc.build); got != tc.want {
			t.Errorf("%s: effectiveVersion(%q) = %q, want %q", name, tc.ldflags, got, tc.want)
		}
	}
}

func TestVersionCommandPrintsTheBuildInfoVersion(t *testing.T) {
	// Not parallel: readBuildInfo is replaced.
	previous := readBuildInfo
	t.Cleanup(func() { readBuildInfo = previous })
	readBuildInfo = buildInfoOf("github.com/rocketinsidecorp/forge-go", "v0.3.1", "h1:x=")
	var stdout bytes.Buffer
	if err := runVersion(nil, &stdout); err != nil || !strings.HasPrefix(stdout.String(), "forge v0.3.1 (") {
		t.Errorf("runVersion = %q, %v", stdout.String(), err)
	}
}

func TestResolveDependenciesPinsTheBuildInfoVersionButNotALocalCheckout(t *testing.T) {
	// Not parallel: readBuildInfo and runGoStep are replaced.
	previous := readBuildInfo
	t.Cleanup(func() { readBuildInfo = previous })
	readBuildInfo = buildInfoOf("github.com/rocketinsidecorp/forge-go", "v0.0.0-20261024123456-0123456789ab", "h1:x=")
	steps := recordGoSteps(t, "")
	if err := resolveDependencies(context.Background(), t.TempDir(), "", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"get", "github.com/rocketinsidecorp/forge-go@v0.0.0-20261024123456-0123456789ab"}, {"mod", "tidy"}}
	if !reflect.DeepEqual(*steps, want) {
		t.Errorf("steps = %v, want %v", *steps, want)
	}
	steps = recordGoSteps(t, "")
	if err := resolveDependencies(context.Background(), t.TempDir(), "/checkout/forge-go", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want = [][]string{
		{"mod", "edit", "-require=github.com/rocketinsidecorp/forge-go@v0.0.0", "-replace=github.com/rocketinsidecorp/forge-go=/checkout/forge-go"},
		{"mod", "tidy"}, {"mod", "vendor"},
	}
	if !reflect.DeepEqual(*steps, want) {
		t.Errorf("a local checkout: steps = %v, want %v (and no go get)", *steps, want)
	}
	// A CLI built from a checkout (no Sum) resolves latest.
	readBuildInfo = buildInfoOf("github.com/rocketinsidecorp/forge-go", "v0.0.0-20261024123456-0123456789ab", "")
	steps = recordGoSteps(t, "")
	if err := resolveDependencies(context.Background(), t.TempDir(), "", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if (*steps)[0][1] != "github.com/rocketinsidecorp/forge-go@latest" {
		t.Errorf("checkout build: steps = %v, want @latest", *steps)
	}
}
