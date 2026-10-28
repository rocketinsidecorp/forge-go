//go:build linux || darwin

package ptytest

import (
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// RequireVariable names the environment variable that, set to "true",
// turns Open's skips into failures, as CI does: a pseudo-terminal that cannot
// be set up (a broken ioctl, say) must not leave the terminal tests green.
const RequireVariable = "FORGE_TEST_REQUIRE_PTY"

// Open returns a pseudo-terminal: the master a test types into and reads the
// terminal's output from, and the slave a program under test reads from and
// writes to. Both are closed when the test ends. The test is skipped where
// /dev/ptmx cannot be opened or the pseudo-terminal cannot be set up, and
// fails instead when RequireVariable is "true".
func Open(t testing.TB) (master, slave *os.File) {
	t.Helper()
	fd, err := syscall.Open("/dev/ptmx", syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		unavailable(t, "no pseudo-terminal available: %v", err)
	}
	master = os.NewFile(uintptr(fd), "/dev/ptmx") // #nosec G115 -- a descriptor syscall.Open returned
	t.Cleanup(func() { _ = master.Close() })
	name, err := slaveName(fd)
	if err != nil {
		unavailable(t, "cannot set up the pseudo-terminal: %v", err)
	}
	slave, err = os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0) // #nosec G304 -- the slave path the kernel named for this master
	if err != nil {
		unavailable(t, "cannot open the pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

// unavailable skips t, or fails it when RequireVariable is "true".
func unavailable(t testing.TB, format string, args ...any) {
	t.Helper()
	if os.Getenv(RequireVariable) == "true" {
		t.Fatalf(format+" ("+RequireVariable+"=true)", args...)
		return
	}
	t.Skipf(format, args...)
}

// ioctl runs ioctl(fd, request, arg); arg is converted in the call
// expression, which keeps what it points to alive during the system call.
func ioctl(fd int, request uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), request, uintptr(arg)); errno != 0 { // #nosec G115 -- file descriptors are small non-negative integers
		return errno
	}
	return nil
}
