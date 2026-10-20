//go:build !unix

package devloop

import (
	"os"
	"os/exec"
	"syscall"
)

// Process groups are a Unix feature. Elsewhere forge signals only the
// process it started, so processes the application itself launches can
// outlive a restart; stop them from the application, or use a Unix host.
func isolate(*exec.Cmd) {}

func signalGroup(p *os.Process, sig syscall.Signal) error {
	if sig == syscall.SIGKILL {
		return p.Kill()
	}
	return p.Signal(sig)
}
