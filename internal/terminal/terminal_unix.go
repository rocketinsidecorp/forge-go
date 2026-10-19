//go:build linux || darwin

package terminal

import "golang.org/x/sys/unix"

func getState(fd int) (*unix.Termios, error) {
	return unix.IoctlGetTermios(fd, ioctlGetTermios)
}

func disableEcho(fd int, state *unix.Termios) error {
	silent := *state
	silent.Lflag &^= unix.ECHO
	silent.Lflag |= unix.ICANON | unix.ISIG
	return unix.IoctlSetTermios(fd, ioctlSetTermios, &silent)
}

func restore(fd int, state *unix.Termios) error {
	return unix.IoctlSetTermios(fd, ioctlSetTermios, state)
}
