package ptytest

import (
	"bytes"
	"fmt"
	"syscall"
	"unsafe"
)

// slaveName grants and unlocks the pseudo-terminal whose master is fd and
// returns its slave's path: TIOCPTYGRANT, TIOCPTYUNLK and TIOCPTYGNAME, as
// macOS's libc grantpt, unlockpt and ptsname do.
func slaveName(fd int) (string, error) {
	if err := ioctl(fd, syscall.TIOCPTYGRANT, nil); err != nil {
		return "", fmt.Errorf("grant: %w", err)
	}
	if err := ioctl(fd, syscall.TIOCPTYUNLK, nil); err != nil {
		return "", fmt.Errorf("unlock: %w", err)
	}
	var name [128]byte                                                                // TIOCPTYGNAME writes a NUL-terminated path of at most 128 bytes
	if err := ioctl(fd, syscall.TIOCPTYGNAME, unsafe.Pointer(&name[0])); err != nil { // #nosec G103 -- an ioctl argument
		return "", fmt.Errorf("name: %w", err)
	}
	path, _, found := bytes.Cut(name[:], []byte{0})
	if !found || len(path) == 0 {
		return "", fmt.Errorf("name: TIOCPTYGNAME returned no path")
	}
	return string(path), nil
}
