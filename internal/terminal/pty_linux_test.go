//go:build linux

package terminal

import (
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openPTY returns a pseudo-terminal: the master a test types into and reads
// the terminal's output from, and the slave a program reads its secret from.
// The test is skipped where /dev/ptmx cannot be opened.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	master = os.NewFile(uintptr(fd), "/dev/ptmx")
	t.Cleanup(func() { _ = master.Close() })
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("cannot unlock the pseudo-terminal: %v", err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Skipf("cannot name the pseudo-terminal: %v", err)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("cannot open the pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

func TestReadSecretOnATerminalHidesAndRestoresEcho(t *testing.T) {
	t.Parallel()
	master, slave := openPTY(t)
	if !IsTerminal(slave) {
		t.Fatal("the slave of a pseudo-terminal is a terminal")
	}
	before, err := unix.IoctlGetTermios(int(slave.Fd()), ioctlGetTermios)
	if err != nil {
		t.Fatal(err)
	}
	if before.Lflag&unix.ECHO == 0 {
		t.Fatal("echo should start enabled")
	}

	type result struct {
		secret []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		secret, err := ReadSecret(slave)
		done <- result{secret, err}
	}()

	// Type only once echo is off: whatever is typed while it is on would be
	// echoed back to the master, and the point is that nothing is.
	deadline := time.Now().Add(10 * time.Second)
	for {
		state, err := unix.IoctlGetTermios(int(slave.Fd()), ioctlGetTermios)
		if err != nil {
			t.Fatal(err)
		}
		if state.Lflag&unix.ECHO == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ReadSecret did not switch echo off")
		}
		select {
		case r := <-done:
			t.Fatalf("ReadSecret returned before any input: %q %v", r.secret, r.err)
		case <-time.After(2 * time.Millisecond):
		}
	}
	if _, err := master.WriteString("hunter2 hunter2\n"); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || string(r.secret) != "hunter2 hunter2" {
		t.Fatalf("secret=%q err=%v", r.secret, r.err)
	}
	after, err := unix.IoctlGetTermios(int(slave.Fd()), ioctlGetTermios)
	if err != nil {
		t.Fatal(err)
	}
	if after.Lflag != before.Lflag {
		t.Errorf("the terminal state was not restored: lflag %#x, was %#x", after.Lflag, before.Lflag)
	}

	// Nothing the user typed was echoed to the terminal's output: switch the
	// master to non-blocking reads and drain it.
	if err := unix.SetNonblock(int(master.Fd()), true); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 256)
	n, err := unix.Read(int(master.Fd()), buffer)
	if n > 0 {
		t.Errorf("the terminal echoed %q", buffer[:n])
	} else if err != nil && err != unix.EAGAIN {
		t.Errorf("reading the terminal output: %v", err)
	}
}

func TestReadSecretReportsALineThatEndsWithoutInput(t *testing.T) {
	t.Parallel()
	master, slave := openPTY(t)
	// Closing the master hangs the terminal up: the read fails or ends.
	if err := master.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecret(slave); err == nil {
		t.Fatal("ReadSecret succeeded on a hung-up terminal")
	}
}
