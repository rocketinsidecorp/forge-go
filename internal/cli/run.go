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

// runDev serves the application with development settings.
func runDev(args []string, stdout, stderr io.Writer) error {
	return runAppCommand(args, nil, stdout, stderr)
}

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

func runAppCommand(nameArgs, appArgs []string, stdout, stderr io.Writer) error {
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
	return runApp(ctx, ".", name, appArgs, env, stdout, stderr)
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
