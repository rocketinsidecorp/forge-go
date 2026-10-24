//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package mail

import "os"

// openDirectory opens dir read-only. Without O_NOFOLLOW and O_DIRECTORY the
// caller's Lstat and SameFile checks are the only guard.
func openDirectory(dir string) (*os.File, error) {
	return os.Open(dir) // #nosec G304 -- operator-configured development directory
}
