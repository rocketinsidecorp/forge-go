package ptytest

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// Open returns a pseudo-terminal: the master a test types into and reads the
// terminal's output from, and the slave a program under test reads from and
// writes to. Both are closed when the test ends. The test is skipped where
// /dev/ptmx cannot be opened or the pseudo-terminal cannot be set up.
func Open(t testing.TB) (master, slave *os.File) {
	t.Helper()
	fd, err := syscall.Open("/dev/ptmx", syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	master = os.NewFile(uintptr(fd), "/dev/ptmx") // #nosec G115 -- a descriptor syscall.Open returned
	t.Cleanup(func() { _ = master.Close() })
	var unlock int32
	if err := ioctl(fd, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil { // #nosec G103 -- an ioctl argument
		t.Skipf("cannot unlock the pseudo-terminal: %v", err)
	}
	var number uint32
	if err := ioctl(fd, syscall.TIOCGPTN, unsafe.Pointer(&number)); err != nil { // #nosec G103 -- an ioctl argument
		t.Skipf("cannot name the pseudo-terminal: %v", err)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("cannot open the pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

// ioctl runs ioctl(fd, request, arg); arg is converted in the call
// expression, which keeps what it points to alive during the system call.
func ioctl(fd int, request uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), request, uintptr(arg)); errno != 0 { // #nosec G115 -- file descriptors are small non-negative integers
		return errno
	}
	return nil
}
