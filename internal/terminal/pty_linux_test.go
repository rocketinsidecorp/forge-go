//go:build linux

package terminal

import (
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/ptytest"
)

func TestReadSecretOnATerminalHidesAndRestoresEcho(t *testing.T) {
	t.Parallel()
	master, slave := ptytest.Open(t)
	if !IsTerminal(slave) {
		t.Fatal("the slave of a pseudo-terminal is a terminal")
	}
	before, err := getState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if before.Lflag&syscall.ECHO == 0 {
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
		state, err := getState(int(slave.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		if state.Lflag&syscall.ECHO == 0 {
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
	after, err := getState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if after.Lflag != before.Lflag {
		t.Errorf("the terminal state was not restored: lflag %#x, was %#x", after.Lflag, before.Lflag)
	}

	// Nothing the user typed was echoed to the terminal's output: switch the
	// master to non-blocking reads and drain it.
	if err := syscall.SetNonblock(int(master.Fd()), true); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 256)
	n, err := syscall.Read(int(master.Fd()), buffer)
	if n > 0 {
		t.Errorf("the terminal echoed %q", buffer[:n])
	} else if err != nil && !errors.Is(err, syscall.EAGAIN) {
		t.Errorf("reading the terminal output: %v", err)
	}
}

func TestReadSecretReportsALineThatEndsWithoutInput(t *testing.T) {
	t.Parallel()
	master, slave := ptytest.Open(t)
	// Closing the master hangs the terminal up: the read fails or ends.
	if err := master.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecret(slave); err == nil {
		t.Fatal("ReadSecret succeeded on a hung-up terminal")
	}
}
