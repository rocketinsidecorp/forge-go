//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package proxy

import (
	"io/fs"
	"os"
)

// openDirectory opens dir read-only. Without O_NOFOLLOW and O_DIRECTORY the
// caller's Lstat and SameFile checks are the only guard.
func openDirectory(dir string) (*os.File, error) {
	return os.Open(dir) // #nosec G304 -- the proxy's own control or cache directory
}

// ownerOf is unknown on this platform; the owner check is skipped.
func ownerOf(fs.FileInfo) (int, bool) { return 0, false }
