package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
	"github.com/rocketinsidecorp/forge-go/internal/version"
)

var releaseVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// readBuildInfo is replaced by tests.
var readBuildInfo = debug.ReadBuildInfo

// builtVersion is the framework version recorded in the CLI's build info, or
// "" when there is none worth trusting. Only a CLI fetched from the module
// proxy or cache (`go install ...forge-go/cmd/forge@VERSION`) qualifies: Go
// records a checksum (Main.Sum) for it, and the version is one `go get` can
// resolve. A build of a checkout has no Sum even when Go stamps a VCS
// pseudo-version, which may name a commit that was never published; a working
// tree is "(devel)" and a dirty one ends in "+dirty".
func builtVersion(build func() (*debug.BuildInfo, bool)) string {
	info, ok := build()
	if !ok || info == nil || info.Main.Path != scaffold.FrameworkModule || info.Main.Sum == "" || !releaseVersion.MatchString(info.Main.Version) {
		return ""
	}
	return info.Main.Version
}

// effectiveVersion is the version this CLI reports: the release stamped
// through ldflags or, when those left "dev", builtVersion.
func effectiveVersion(ldflagsVersion string, build func() (*debug.BuildInfo, bool)) string {
	if ldflagsVersion == "dev" {
		if built := builtVersion(build); built != "" {
			return built
		}
	}
	return ldflagsVersion
}

// frameworkTarget is the framework version `forge new` pins. A release build
// stamps version.Version through ldflags. A CLI installed from the module
// proxy carries no ldflags ("dev") but records the module version it was built
// from (builtVersion), which is then pinned so the generated application uses
// the framework its templates came from. Anything else resolves to "latest".
func frameworkTarget(ldflagsVersion string, build func() (*debug.BuildInfo, bool)) string {
	if version := effectiveVersion(ldflagsVersion, build); releaseVersion.MatchString(version) {
		return version
	}
	return "latest"
}

func runNew(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("new", flag.ContinueOnError)
	flags.SetOutput(stderr)
	module := flags.String("module", "", "Go module path (default: NAME)")
	dir := flags.String("dir", "", "target directory (default: NAME)")
	forgePath := flags.String("forge-path", "", "use a local Forge checkout and vendor it (framework development)")
	deprecatedPath := flags.String("framework-path", "", "deprecated alias of --forge-path; removed in the next minor version")
	skipDeps := flags.Bool("skip-deps", false, "generate files without resolving Go dependencies")
	flags.BoolVar(skipDeps, "skip-lockfile", false, "alias of --skip-deps (Forge-Rust's name: go.sum is the lockfile)")
	database := flags.String("database", "postgresql", "database: "+strings.Join(scaffold.Databases, ", ")+" or none")
	flags.StringVar(database, "d", "postgresql", "shorthand for --database")
	skipDatabase := flags.Bool("skip-database", false, "same as --database none")
	locale := flags.String("locale", "en", "default locale: "+strings.Join(scaffold.Locales, ", ")+" (en.json is always generated)")
	apiAuth := flags.Bool("api-auth", false, "also mount the JSON authentication APIs (bearer and cookie login, MFA completion, JSON password recovery, GET /v1/auth/session); PostgreSQL only")
	apiOnly := flags.Bool("api", false, "generate an API-only application (like rails new --api): JSON handlers without views, assets, JavaScript, translations, sign-in pages or browser middleware")
	name, err := parseSinglePositional(flags, args, "NAME")
	if err != nil {
		return err
	}
	if err := checkAPIConflicts(flags, *apiOnly); err != nil {
		return err
	}
	frameworkPath, err := resolveForgePath(flags, *forgePath, *deprecatedPath, stderr)
	if err != nil {
		return err
	}
	if *skipDatabase {
		explicit := false
		flags.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "database" || f.Name == "d" })
		if explicit {
			return errors.New("--skip-database conflicts with --database")
		}
		*database = "none"
	}
	opts, err := scaffold.Normalize(scaffold.Options{Name: name, Module: *module, Dir: *dir, Database: *database, Locale: *locale, APIAuth: *apiAuth, API: *apiOnly})
	if err != nil {
		return err
	}
	localFramework := ""
	if frameworkPath != "" {
		if localFramework, err = checkFrameworkPath(frameworkPath); err != nil {
			return err
		}
	}

	paths, err := scaffold.Generate(opts)
	if err != nil {
		return err
	}
	for _, path := range paths {
		fmt.Fprintf(stdout, "  create  %s\n", filepath.Join(opts.Dir, path))
	}
	if !*skipDeps {
		if err := resolveDependencies(context.Background(), opts.Dir, localFramework, stdout, stderr); err != nil {
			return fmt.Errorf("resolve dependencies (files were generated; fix the cause and run `go mod tidy` in %s): %w", opts.Dir, err)
		}
	}
	visit := "visit http://127.0.0.1:8080/ for the welcome screen."
	if opts.API {
		visit = "curl http://127.0.0.1:8080/ and http://127.0.0.1:8080/health (JSON)."
	}
	_, err = fmt.Fprintf(stdout, "\nNext steps:\n  cd %s\n  docker compose up --build   # or: forge dev\n\n  After it starts, %s\n\n  Traces, metrics and logs in Grafana (Prometheus, Tempo, Loki): see \"Observability stack\" in README.md.\n", opts.Dir, visit)
	return err
}

// checkAPIConflicts refuses the flags Forge-Rust declares conflicts_with =
// "api" for `forge new`: --api-auth (the JSON authentication APIs sit beside
// the sign-in pages an API-only application lacks) and --locale (it has no
// pages to translate). Naming either with --api is a usage error, as in
// Forge-Rust, even with the default value.
func checkAPIConflicts(flags *flag.FlagSet, apiOnly bool) error {
	if !apiOnly {
		return nil
	}
	var conflicts []string
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "api-auth" || f.Name == "locale" {
			conflicts = append(conflicts, f.Name)
		}
	})
	if len(conflicts) == 0 {
		return nil
	}
	return usageError{fmt.Sprintf("--%s cannot be used with --api", conflicts[0])}
}

// resolveForgePath returns the local checkout named by --forge-path or by its
// deprecated alias --framework-path. The alias still works and prints exactly
// one deprecation line to stderr; naming both with different values is a usage
// error.
func resolveForgePath(flags *flag.FlagSet, forgePath, deprecatedPath string, stderr io.Writer) (string, error) {
	var canonicalSet, aliasSet bool
	flags.Visit(func(f *flag.Flag) {
		canonicalSet = canonicalSet || f.Name == "forge-path"
		aliasSet = aliasSet || f.Name == "framework-path"
	})
	switch {
	case !aliasSet:
		return forgePath, nil
	case canonicalSet && forgePath != deprecatedPath:
		return "", errors.New("--forge-path and --framework-path name different directories; use only --forge-path")
	}
	fmt.Fprintln(stderr, "forge: --framework-path is deprecated and will be removed in the next minor version; use --forge-path")
	return deprecatedPath, nil
}

// runGoStep runs one go command of resolveDependencies in dir; tests replace
// it to observe the arguments without running the go tool.
var runGoStep = func(ctx context.Context, dir string, stdout, stderr io.Writer, args ...string) error {
	return goCommand(ctx, dir, nil, stdout, stderr, args...).Run()
}

// resolveDependencies adds the framework requirement. With a local checkout
// the dependency is replaced and vendored, so the Docker build context stays
// self-contained; otherwise the matching release is fetched.
func resolveDependencies(ctx context.Context, dir, localFramework string, stdout, stderr io.Writer) error {
	var steps [][]string
	if localFramework != "" {
		steps = [][]string{
			{"mod", "edit", "-require=" + scaffold.FrameworkModule + "@v0.0.0", "-replace=" + scaffold.FrameworkModule + "=" + localFramework},
			{"mod", "tidy"},
			{"mod", "vendor"},
		}
	} else {
		target := frameworkTarget(version.Version, readBuildInfo)
		steps = [][]string{{"get", scaffold.FrameworkModule + "@" + target}, {"mod", "tidy"}}
	}
	for _, step := range steps {
		fmt.Fprintf(stdout, "  run     go %s\n", strings.Join(step, " "))
		if err := runGoStep(ctx, dir, stdout, stderr, step...); err != nil {
			return fmt.Errorf("go %s: %w", step[0], err)
		}
	}
	return nil
}

func checkFrameworkPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return "", fmt.Errorf("framework path: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open("go.mod")
	if err != nil {
		return "", fmt.Errorf("framework path: %w", err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if fields := strings.Fields(scanner.Text()); len(fields) == 2 && fields[0] == "module" {
			if fields[1] != scaffold.FrameworkModule {
				return "", fmt.Errorf("framework path contains module %s, want %s", fields[1], scaffold.FrameworkModule)
			}
			return absolute, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", errors.New("framework path go.mod has no module directive")
}

// goCommand runs the go tool directly, never through a shell, with arguments
// built by Forge from validated input.
func goCommand(ctx context.Context, dir string, env []string, stdout, stderr io.Writer, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, "go", args...) // #nosec G204 -- fixed executable, no shell, validated arguments
	command.Dir = dir
	command.Env = env
	command.Stdout = stdout
	command.Stderr = stderr
	return command
}

// parseSinglePositional accepts the positional argument before or after flags.
func parseSinglePositional(flags *flag.FlagSet, args []string, label string) (string, error) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		if err := parseFlags(flags, args[1:]); err != nil {
			return "", err
		}
		if flags.NArg() != 0 {
			return "", usageError{fmt.Sprintf("unexpected arguments: %s", strings.Join(flags.Args(), " "))}
		}
		return args[0], nil
	}
	if err := parseFlags(flags, args); err != nil {
		return "", err
	}
	if flags.NArg() != 1 {
		return "", usageError{fmt.Sprintf("expected exactly one %s", label)}
	}
	return flags.Arg(0), nil
}
