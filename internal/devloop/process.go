package devloop

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// processWaitDelay bounds how long Wait lingers for output copying after the
// process exits.
const processWaitDelay = 2 * time.Second

// Instance is one running copy of the application.
type Instance interface {
	// Done is closed when the process has exited.
	Done() <-chan struct{}
	// Stop asks the process to shut down gracefully (SIGTERM), waits up to
	// grace, then kills it. It returns once the process is gone.
	Stop(grace time.Duration) error
}

// Process is an Instance backed by an operating system process.
type Process struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

// StartProcess runs binary in dir with env and args, wiring its output.
// The goroutine waiting for it ends when the process exits, which Stop and
// the caller's shutdown guarantee.
func StartProcess(binary string, args []string, dir string, env []string, stdout, stderr io.Writer) (*Process, error) {
	cmd := exec.Command(binary, args...) // #nosec G204 -- the binary forge built from the application's own source
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = processWaitDelay
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start application: %w", err)
	}
	p := &Process{cmd: cmd, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// Done is closed when the process has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// Err is the process's exit error; it is valid after Done is closed.
func (p *Process) Err() error { return p.err }

// Stop sends SIGTERM, waits up to grace for a clean exit, then SIGKILL.
func (p *Process) Stop(grace time.Duration) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("signal application: %w", err)
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-timer.C:
	}
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill application: %w", err)
	}
	<-p.done
	return fmt.Errorf("application did not stop within %s and was killed", grace)
}
