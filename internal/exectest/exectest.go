// Package exectest creates the executable stand-ins (fake ssh, fake database
// clients, hook scripts) that tests run as separate processes.
//
// Writing a file in-process and executing it at once is racy on Linux: while
// any test is forking, the child may inherit the write descriptor until it
// execs, and executing a file somebody still holds open for writing fails
// with ETXTBSY ("text file busy", golang/go#22315). Write therefore creates
// the file in a child process, so this process never holds a writable
// descriptor to it, and returns only after that child has exited.
package exectest

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Write creates path as an executable (mode 0700) file holding body, replacing
// any file already there, and returns once no process holds it open for
// writing. The file is safe to exec immediately.
func Write(path, body string) error {
	// The path is an argument, never part of the script, so no quoting is
	// needed. The mode is set by chmod, not the umask.
	command := exec.CommandContext(context.Background(), "/bin/sh", "-c", `cat > "$1" && chmod 700 "$1"`, "sh", path) // #nosec G204 -- fixed script; the path is a positional parameter.
	command.Stdin = bytes.NewReader([]byte(body))
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("write executable %s: %w: %s", path, err, bytes.TrimSpace(output))
	}
	return nil
}
