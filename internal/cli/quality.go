package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// golangciLintInstall is the install hint `forge lint` prints when
// golangci-lint is missing. The version is the one this repository's CI runs.
const golangciLintInstall = "go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 (or see https://golangci-lint.run/welcome/install/)"

// gofmtBatch bounds how many files one gofmt invocation names, so a large
// application never exceeds the operating system's argument limit.
const gofmtBatch = 200

// requireApplication refuses to run outside a Go module: check, format and
// lint, like test and build, work on the application in the current directory.
func requireApplication() error {
	if _, err := os.Stat("go.mod"); err != nil {
		return errors.New("no go.mod here: run this inside a Forge application")
	}
	return nil
}

// toolError reports a failed tool. The tool already wrote its diagnostics, and
// its own exit status (gofmt exits 2 on a syntax error) is not forge's: forge
// exits 1, "the command failed", and keeps 2 for a command line that is wrong.
func toolError(name string, err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Errorf("%s exited with status %d", name, exit.ExitCode())
	}
	return fmt.Errorf("%s: %w", name, err)
}

// runCheck type-checks the application without producing a binary: go build
// compiles every package, go vet type-checks the tests too and reports
// suspicious constructs. It is the counterpart of Forge-Rust's `forge check`
// (cargo check --all-targets).
func runCheck(args []string, stdout, stderr io.Writer) error {
	if len(args) != 0 {
		return usageError{"usage: forge check"}
	}
	if err := requireApplication(); err != nil {
		return err
	}
	ctx := context.Background()
	for _, step := range [][]string{{"build", "./..."}, {"vet", "./..."}} {
		fmt.Fprintf(stdout, "  run     go %s\n", strings.Join(step, " "))
		if err := goCommand(ctx, ".", nil, stdout, stderr, step...).Run(); err != nil {
			return toolError("go "+step[0], err)
		}
	}
	_, err := fmt.Fprintln(stdout, "check: ok")
	return err
}

// runFormat formats the application's Go files with gofmt. With --check it
// lists the files that need formatting, writes nothing and fails when there
// are any: the form CI runs. It is the counterpart of Forge-Rust's
// `forge format` (cargo fmt --all).
func runFormat(args []string, stdout, stderr io.Writer) error {
	check := false
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "--check":
		check = true
	default:
		return usageError{"usage: forge format [--check]"}
	}
	if err := requireApplication(); err != nil {
		return err
	}
	gofmt, err := exec.LookPath("gofmt")
	if err != nil {
		return errors.New("gofmt not found on PATH: it ships with the Go toolchain (install Go from https://go.dev/dl/)")
	}
	files, err := goFiles(".")
	if err != nil {
		return err
	}
	flags := []string{"-l", "-w"}
	if check {
		flags = []string{"-l"}
	}
	var changed []string
	for batch := range slices.Chunk(files, gofmtBatch) {
		var listed bytes.Buffer
		command := exec.CommandContext(context.Background(), gofmt, append(slices.Clone(flags), batch...)...) // #nosec G204 -- the gofmt found on PATH, no shell, file paths found by forge itself
		command.Stdout = &listed
		command.Stderr = stderr
		if err := command.Run(); err != nil {
			return toolError("gofmt", err)
		}
		for line := range strings.SplitSeq(listed.String(), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				changed = append(changed, line)
			}
		}
	}
	for _, path := range changed {
		if _, err := fmt.Fprintln(stdout, path); err != nil {
			return err
		}
	}
	if check && len(changed) > 0 {
		return fmt.Errorf("%d file(s) need formatting: run `forge format`", len(changed))
	}
	return nil
}

// goFiles lists the .go files under root that the application owns, the way
// the go tool selects packages: it skips vendor, testdata, directories whose
// name starts with "." or "_" and nested modules.
func goFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if entry.IsDir() {
			if path == root {
				return nil
			}
			if name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type().IsRegular() && strings.HasSuffix(name, ".go") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// runLint runs golangci-lint, the Go counterpart of `cargo clippy`, with the
// application's own configuration. It is not part of the Go toolchain, so a
// missing binary is an explained error rather than a silent pass.
func runLint(args []string, stdout, stderr io.Writer) error {
	if len(args) != 0 {
		return usageError{"usage: forge lint"}
	}
	if err := requireApplication(); err != nil {
		return err
	}
	lint, err := exec.LookPath("golangci-lint")
	if err != nil {
		return fmt.Errorf("golangci-lint not found on PATH: install it with %s", golangciLintInstall)
	}
	command := exec.CommandContext(context.Background(), lint, "run", "./...") // #nosec G204 -- the golangci-lint found on PATH, fixed arguments, no shell
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return toolError("golangci-lint", err)
	}
	_, err = fmt.Fprintln(stdout, "lint: ok")
	return err
}
