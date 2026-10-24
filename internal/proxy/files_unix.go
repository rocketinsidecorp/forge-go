//go:build linux || darwin || freebsd || netbsd || openbsd

package proxy

import (
	"io/fs"
	"os"
	"syscall"
)

// openDirectory opens dir read-only without following a symbolic link in
// its last component and only if it is a directory: a link or file swapped
// in after the caller's Lstat fails to open instead of being followed.
func openDirectory(dir string) (*os.File, error) {
	return os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- the proxy's own control or cache directory
}

// ownerOf returns the uid that owns info's file.
func ownerOf(info fs.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}
