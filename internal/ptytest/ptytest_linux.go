package ptytest

import (
	"fmt"
	"syscall"
	"unsafe"
)

// slaveName unlocks the pseudo-terminal whose master is fd and returns its
// slave's path: TIOCSPTLCK and TIOCGPTN, as glibc's unlockpt and ptsname do.
func slaveName(fd int) (string, error) {
	var unlock int32
	if err := ioctl(fd, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil { // #nosec G103 -- an ioctl argument
		return "", fmt.Errorf("unlock: %w", err)
	}
	var number uint32
	if err := ioctl(fd, syscall.TIOCGPTN, unsafe.Pointer(&number)); err != nil { // #nosec G103 -- an ioctl argument
		return "", fmt.Errorf("name: %w", err)
	}
	return fmt.Sprintf("/dev/pts/%d", number), nil
}
