package dbtasks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// maxToolOutput bounds the tool diagnostics shown in an error.
const maxToolOutput = 4096

// tool runs a vendor tool, found on PATH, with an explicit environment.
type tool struct {
	name  string
	args  []string
	env   []string
	dir   string
	stdin io.Reader
}

// run executes the tool and returns its stdout. A failure carries the tool's
// own diagnostics (stderr), which connection tools keep free of passwords.
func (t *Target) run(ctx context.Context, spec tool) ([]byte, error) {
	path, err := t.lookPath(spec.name)
	if err != nil {
		return nil, missingTool(spec.name, t.Adapter)
	}
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, path, spec.args...) // #nosec G204 -- a vendor tool found on PATH, no shell, forge-chosen arguments
	command.Env = spec.env
	command.Dir = spec.dir
	command.Stdin = spec.stdin
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > maxToolOutput {
			message = message[:maxToolOutput] + "..."
		}
		if message == "" {
			return nil, fmt.Errorf("%s failed: %w", spec.name, err)
		}
		return nil, fmt.Errorf("%s failed: %w\n%s", spec.name, err, message)
	}
	return stdout.Bytes(), nil
}

// environ is the process environment the tools start from.
func environ() []string { return os.Environ() }

// writeFile writes data to a new file with owner-only permissions.
func writeFile(name string, data []byte) error {
	return os.WriteFile(name, data, 0o600)
}
