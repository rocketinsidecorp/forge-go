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

// privateDir makes path a directory only the proxy's user can use (0700).
// A missing directory is created with that mode; an existing one is
// restricted to it, because os.MkdirAll applies the mode only to
// directories it creates. The directory is checked and changed through one
// handle opened without following a link (fstat, fchmod), so the path
// cannot be swapped between the check and the change. A symbolic link, any
// other kind of file, and a directory owned by another user are refused.
func privateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if !before.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	handle, err := openDirectory(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = handle.Close() }()
	info, err := handle.Stat()
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.IsDir() || !os.SameFile(before, info) {
		return fmt.Errorf("%s changed while it was checked", path)
	}
	if owner, ok := ownerOf(info); ok && owner != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, not by the proxy's uid %d", path, owner, os.Geteuid())
	}
	if info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0o700 {
		if err := handle.Chmod(0o700); err != nil { // #nosec G302 -- a directory needs its search bit; 0700 is owner-only
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

// durableCache is autocert's directory cache with durable writes: Put
// writes a 0600 temporary file, syncs it, renames it over the entry and
// syncs the directory before it returns, so an account key or certificate
// survives a crash. DirCache.Put syncs neither. Get and Delete are
// DirCache's.
type durableCache struct {
	autocert.DirCache
}

func (c durableCache) Put(ctx context.Context, name string, data []byte) error {
	dir := string(c.DirCache)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// The path DirCache.Get reads name from.
	target := filepath.Join(dir, filepath.Clean("/"+name))
	temp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	// Like DirCache.Put: a cancelled Put does not overwrite the entry.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tempName, target); err != nil {
		return err
	}
	return syncDir(dir)
}
