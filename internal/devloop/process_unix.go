//go:build unix

package devloop

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// isolate puts the command in a process group of its own, so that signals
// sent to the group reach everything the command starts, and the terminal's
// Ctrl-C reaches forge, which stops the group in order.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends sig to the whole process group led by p. If there is no
// such group (p is not a group leader) it signals p alone; a process that is
// already gone is os.ErrProcessDone.
func signalGroup(p *os.Process, sig syscall.Signal) error {
	err := syscall.Kill(-p.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return p.Signal(sig)
	}
	return err
}
