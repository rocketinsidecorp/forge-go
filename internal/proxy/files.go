package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/crypto/acme/autocert"
)

// privateDir makes path a directory only its owner can use (0700). A
// missing directory is created with that mode; an existing one is
// restricted to it, because os.MkdirAll applies the mode only to
// directories it creates. A symbolic link or any other kind of file at path
// is refused, and so is a directory the proxy cannot restrict (owned by
// someone else).
func privateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0o700 {
		if err := os.Chmod(path, 0o700); err != nil { // #nosec G302 -- a directory needs its search bit; 0700 is owner-only
			return fmt.Errorf("restrict %s to 0700: %w", path, err)
		}
	}
	return nil
}

// syncDir flushes a directory's entries (a rename in it) to disk. File
// systems that cannot sync a directory (EINVAL) are accepted, as the rename
// is all they offer.
func syncDir(dir string) error {
	handle, err := os.Open(dir) // #nosec G304 -- dir is the proxy's own state or cache directory
	if err != nil {
		return err
	}
	err = handle.Sync()
	if closeErr := handle.Close(); err == nil {
		err = closeErr
	}
	if errors.Is(err, syscall.EINVAL) {
		return nil
	}
	return err
}

// durableCache is autocert's directory cache with what it leaves out: the
// file written by Put and the rename in the directory are synced to disk
// before Put returns, so an account key or certificate survives a crash.
type durableCache struct {
	autocert.DirCache
}

func (c durableCache) Put(ctx context.Context, name string, data []byte) error {
	if err := c.DirCache.Put(ctx, name, data); err != nil {
		return err
	}
	dir := string(c.DirCache)
	// DirCache.Put stores name at this path.
	file, err := os.Open(filepath.Join(dir, filepath.Clean("/"+name))) // #nosec G304 -- the cache file DirCache.Put just wrote
	if err != nil {
		return err
	}
	err = file.Sync()
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return syncDir(dir)
}
