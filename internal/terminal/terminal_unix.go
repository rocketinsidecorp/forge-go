//go:build linux || darwin

package terminal

import (
	"syscall"
	"unsafe"
)

// The terminal state is read and written with the termios ioctls of the
// standard library's syscall package, so the core links no x/sys module.

func getState(fd int) (*syscall.Termios, error) {
	state := new(syscall.Termios)
	if err := ioctlTermios(fd, ioctlGetTermios, state); err != nil {
		return nil, err
	}
	return state, nil
}

func disableEcho(fd int, state *syscall.Termios) error {
	silent := *state
	silent.Lflag &^= syscall.ECHO
	silent.Lflag |= syscall.ICANON | syscall.ISIG
	return ioctlTermios(fd, ioctlSetTermios, &silent)
}

func restore(fd int, state *syscall.Termios) error {
	return ioctlTermios(fd, ioctlSetTermios, state)
}

// ioctlTermios runs ioctl(fd, request, state). The pointer is converted in
// the call expression itself, which keeps state alive and unmoved for the
// duration of the system call (unsafe.Pointer rule 4).
func ioctlTermios(fd int, request uintptr, state *syscall.Termios) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), request, uintptr(unsafe.Pointer(state))); errno != 0 { // #nosec G103 G115 -- the syscall.Syscall pointer-argument pattern; descriptors are small non-negative integers
		return errno
	}
	return nil
}
