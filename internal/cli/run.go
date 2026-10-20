package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

// appShutdownGrace bounds how long forge waits for the application to drain
// after an interrupt; it exceeds the default HTTP shutdown timeout.
const appShutdownGrace = 35 * time.Second

// runMigrate and runRollback delegate to the application binary's own
// commands, so development and production share one implementation.
func runMigrate(args []string, stdout, stderr io.Writer) error {
	if len(args) != 0 && !slices.Equal(args, []string{"status"}) {
		return errors.New("usage: forge migrate [status]")
	}
	return runAppCommand(nil, append([]string{"migrate"}, args...), stdout, stderr)
}

func runRollback(args []string, stdout, stderr io.Writer) error {
	return runAppCommand(nil, append([]string{"rollback"}, args...), stdout, stderr)
}

// runOpenAPI delegates to the application's built-in openapi command, which
// runs Configure without serving and prints or writes the document of the
// described routes (ADR 0043). It needs what Configure needs, such as the
// development database.
func runOpenAPI(args []string, stdout, stderr io.Writer) error {
	flags := 0
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--output" || arg == "--check":
			if i+1 == len(args) {
				return errors.New(openAPIUsage)
			}
			i++
		case strings.HasPrefix(arg, "--output=") || strings.HasPrefix(arg, "--check="):
		default:
			return errors.New(openAPIUsage)
		}
		flags++
	}
	if flags > 1 {
		return errors.New(openAPIUsage)
	}
	return runAppCommand(nil, append([]string{"openapi"}, args...), stdout, stderr)
}

const openAPIUsage = "usage: forge openapi [--output FILE | --check FILE]"

// runSetup delegates to the application's setup command, which creates the
// first tenant and owner. Standard input is passed through so the password
// can be typed at the terminal (without echo) or piped with
// --password-stdin; it is never an argument.
func runSetup(args []string, stdout, stderr io.Writer) error {
	return runAppCommandWithInput(nil, append([]string{"setup"}, args...), os.Stdin, stdout, stderr)
}

// runSeed delegates to the application's seed command.
func runSeed(args []string, stdout, stderr io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: forge seed")
	}
	return runAppCommand(nil, []string{"seed"}, stdout, stderr)
}

func runAppCommand(nameArgs, appArgs []string, stdout, stderr io.Writer) error {
	return runAppCommandWithInput(nameArgs, appArgs, nil, stdout, stderr)
}

func runAppCommandWithInput(nameArgs, appArgs []string, stdin io.Reader, stdout, stderr io.Writer) error {
	name, err := resolveCommand(".", nameArgs)
	if err != nil {
		return err
	}
	env, err := developmentEnvironment(".")
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runAppWithInput(ctx, ".", name, appArgs, env, stdin, stdout, stderr)
}

// runTest runs go test with the development environment, so database tests
// find FORGE_TEST_DATABASE_URL. Arguments pass through; the default is ./...
func runTest(args []string, stdout, stderr io.Writer) error {
	if _, err := os.Stat("go.mod"); err != nil {
		return errors.New("no go.mod here: run this inside a Forge application")
	}
	env, err := developmentEnvironment(".")
	if err != nil {
		return err
	}
	if len(args) == 0 {
		args = []string{"./..."}
	}
	return goCommand(context.Background(), ".", env, stdout, stderr, append([]string{"test"}, args...)...).Run()
}

func runBuild(args []string, stdout, stderr io.Writer) error {
	name, err := resolveCommand(".", args)
	if err != nil {
		return err
	}
	output := filepath.Join("bin", name)
	if err := goCommand(context.Background(), ".", nil, stdout, stderr, "build", "-trimpath", "-o", output, "./cmd/"+name).Run(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "built %s\n", output)
	return err
}

func developmentEnvironment(root string) ([]string, error) {
	env, err := environment(root, os.Environ())
	if err != nil {
		return nil, err
	}
	if _, ok := os.LookupEnv("FORGE_ENV"); !ok && !hasKey(env, "FORGE_ENV") {
		env = append(env, "FORGE_ENV=development")
	}
	return env, nil
}

func hasKey(env []string, key string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			return true
		}
	}
	return false
}

// runApp compiles cmd/<name> and runs the binary as a direct child. It does
// not use `go run`, which does not forward signals: an interrupt delivered
// only to forge would otherwise leave the application running. Cancelling ctx
// sends the application an interrupt so it shuts down gracefully.
func runApp(ctx context.Context, root, name string, args, env []string, stdout, stderr io.Writer) error {
	return runAppWithInput(ctx, root, name, args, env, nil, stdout, stderr)
}

// runAppWithInput is runApp with standard input for the application; nil
// means none.
func runAppWithInput(ctx context.Context, root, name string, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	buildDir, err := os.MkdirTemp("", "forge-app-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(buildDir) }()
	binary := filepath.Join(buildDir, name)
	if err := goCommand(ctx, root, nil, stdout, stderr, "build", "-o", binary, "./cmd/"+name).Run(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("build: %w", err)
	}

	command := exec.CommandContext(ctx, binary, args...) // #nosec G204 -- the binary built above, with forge-chosen arguments
	command.Dir = root
	command.Env = env
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
	command.WaitDelay = appShutdownGrace
	err = command.Run()
	if ctx.Err() != nil {
		if state := command.ProcessState; state != nil && !state.Success() {
			return fmt.Errorf("application did not shut down cleanly: %s", state)
		}
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// The application already reported the failure on stderr.
		return fmt.Errorf("%s exited with status %d", name, exitErr.ExitCode())
	}
	return err
}

// resolveCommand picks the application binary under cmd/: the one named in
// args, or the only one present.
func resolveCommand(root string, args []string) (string, error) {
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return "", errors.New("no go.mod here: run this inside a Forge application")
	}
	if len(args) > 1 {
		return "", errors.New("expected at most one command name")
	}
	if len(args) == 1 {
		if _, err := scaffold.Normalize(scaffold.Options{Name: args[0]}); err != nil {
			return "", fmt.Errorf("invalid command name: %w", err)
		}
		if _, err := os.Stat(filepath.Join(root, "cmd", args[0], "main.go")); err != nil {
			return "", fmt.Errorf("cmd/%s/main.go not found", args[0])
		}
		return args[0], nil
	}
	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		return "", errors.New("no cmd/ directory: run this inside a Forge application")
	}
	var names []string
	for _, entry := range entries {
		if _, err := os.Stat(filepath.Join(root, "cmd", entry.Name(), "main.go")); entry.IsDir() && err == nil {
			names = append(names, entry.Name())
		}
	}
	switch len(names) {
	case 1:
		return names[0], nil
	case 0:
		return "", errors.New("no cmd/<name>/main.go found")
	default:
		return "", fmt.Errorf("several commands found (%s): pass one name", strings.Join(names, ", "))
	}
}
