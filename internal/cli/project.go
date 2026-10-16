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
	"strings"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
	"github.com/rocketinsidecorp/forge-go/internal/version"
)

var releaseVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

func runNew(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("new", flag.ContinueOnError)
	flags.SetOutput(stderr)
	module := flags.String("module", "", "Go module path (default: NAME)")
	dir := flags.String("dir", "", "target directory (default: NAME)")
	frameworkPath := flags.String("framework-path", "", "use a local Forge checkout and vendor it (framework development)")
	skipDeps := flags.Bool("skip-deps", false, "generate files without resolving Go dependencies")
	database := flags.String("database", "postgresql", "database: "+strings.Join(scaffold.Databases, ", ")+" or none")
	flags.StringVar(database, "d", "postgresql", "shorthand for --database")
	skipDatabase := flags.Bool("skip-database", false, "same as --database none")
	name, err := parseSinglePositional(flags, args, "NAME")
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
	opts, err := scaffold.Normalize(scaffold.Options{Name: name, Module: *module, Dir: *dir, Database: *database})
	if err != nil {
		return err
	}
	localFramework := ""
	if *frameworkPath != "" {
		if localFramework, err = checkFrameworkPath(*frameworkPath); err != nil {
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
	_, err = fmt.Fprintf(stdout, "\nNext steps:\n  cd %s\n  docker compose up --build   # or: forge dev\n\n  After it starts, visit http://127.0.0.1:8080/ for the welcome screen.\n", opts.Dir)
	return err
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
		target := "latest"
		if releaseVersion.MatchString(version.Version) {
			target = version.Version
		}
		steps = [][]string{{"get", scaffold.FrameworkModule + "@" + target}, {"mod", "tidy"}}
	}
	for _, step := range steps {
		fmt.Fprintf(stdout, "  run     go %s\n", strings.Join(step, " "))
		if err := goCommand(ctx, dir, nil, stdout, stderr, step...).Run(); err != nil {
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
		if err := flags.Parse(args[1:]); err != nil {
			return "", err
		}
		if flags.NArg() != 0 {
			return "", fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
		}
		return args[0], nil
	}
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if flags.NArg() != 1 {
		return "", fmt.Errorf("expected exactly one %s", label)
	}
	return flags.Arg(0), nil
}
