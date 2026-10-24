//go:build linux || darwin || freebsd || netbsd || openbsd

package mail

import (
	"os"
	"syscall"
)

// openDirectory opens dir read-only without following a symbolic link in its
// last component and only if it is a directory: a link, or a FIFO swapped in
// after the caller's Lstat, fails to open instead of being followed or
// blocking.
func openDirectory(dir string) (*os.File, error) {
	return os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- operator-configured development directory
}
