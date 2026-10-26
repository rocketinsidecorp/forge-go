//go:build unix

package credentials_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/credentials"
	"github.com/rocketinsidecorp/forge-go/encryption"
)

func TestWritableCredentialsDirectoriesAreRefused(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "credentials")
	key := newKey(t, 3)
	writeCredentials(t, dir, "production", document, key)
	if err := credentials.WriteKeyFile(dir, "production", []encryption.Key{key}, false); err != nil {
		t.Fatal(err)
	}
	noEnv := func(string) (string, bool) { return "", false }
	// Group or others could replace the 0600 key file in such a directory.
	for _, mode := range []os.FileMode{0o770, 0o775, 0o707, 0o777, 0o777 | os.ModeSticky} {
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		_, _, err := credentials.LoadKeys(dir, "production", noEnv)
		if !errors.Is(err, credentials.ErrDirectoryPermissions) || !strings.Contains(err.Error(), "chmod 700") {
			t.Fatalf("mode %v: %v", mode, err)
		}
		err = credentials.WriteKeyFile(dir, "staging", []encryption.Key{key}, false)
		if !errors.Is(err, credentials.ErrDirectoryPermissions) {
			t.Fatalf("mode %v: write: %v", mode, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "staging.key")); err == nil {
			t.Fatalf("mode %v: a key was written", mode)
		}
	}
	// Readable by others is not writable: a checkout made with umask 022.
	for _, mode := range []os.FileMode{0o755, 0o750, 0o700, 0o500} {
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		if _, _, err := credentials.LoadKeys(dir, "production", noEnv); err != nil {
			t.Fatalf("mode %v: %v", mode, err)
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestFIFOsAtCredentialsPathsAreRefusedWithoutBlocking: a FIFO would block
// open until a writer appears; it must be refused at once. The calls run on a
// goroutine so that a regression fails this test instead of hanging the run.
func TestFIFOsAtCredentialsPathsAreRefusedWithoutBlocking(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "credentials")
	key := newKey(t, 4)
	writeCredentials(t, dir, "production", document, key)
	if err := credentials.WriteKeyFile(dir, "production", []encryption.Key{key}, false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"production.key", "production.json.enc"} {
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	type result struct{ keys, file error }
	done := make(chan result, 1)
	go func() {
		_, _, keysErr := credentials.LoadKeys(dir, "production", func(string) (string, bool) { return "", false })
		_, fileErr := credentials.ReadFile(dir, "production")
		done <- result{keysErr, fileErr}
	}()
	select {
	case got := <-done:
		if got.keys == nil || !strings.Contains(got.keys.Error(), "is not a regular file") {
			t.Fatalf("key FIFO: %v", got.keys)
		}
		if got.file == nil || !strings.Contains(got.file.Error(), "not a regular file") {
			t.Fatalf("credentials FIFO: %v", got.file)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("opening a FIFO blocked")
	}
}

// TestSymlinkedCredentialsFilesAreRefused: a key or credentials file that is
// a symbolic link is refused even when it points inside the directory, as in
// forge-rust (O_NOFOLLOW): os.Root alone would follow it.
func TestSymlinkedCredentialsFilesAreRefused(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "credentials")
	key := newKey(t, 6)
	writeCredentials(t, dir, "production", document, key)
	if err := credentials.WriteKeyFile(dir, "production", []encryption.Key{key}, false); err != nil {
		t.Fatal(err)
	}
	noEnv := func(string) (string, bool) { return "", false }
	for _, name := range []string{"production.key", "production.json.enc"} {
		path := filepath.Join(dir, name)
		if err := os.Rename(path, path+".real"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(name+".real", path); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := credentials.LoadKeys(dir, "production", noEnv); !errors.Is(err, credentials.ErrSymlink) {
		t.Fatalf("symlinked key file: %v", err)
	}
	if _, err := credentials.ReadFile(dir, "production"); !errors.Is(err, credentials.ErrSymlink) {
		t.Fatalf("symlinked credentials file: %v", err)
	}
	if _, err := credentials.Open("production", credentials.Options{Dir: dir, Lookup: noEnv}); !errors.Is(err, credentials.ErrSymlink) {
		t.Fatalf("open with a symlinked key file: %v", err)
	}
	// A dangling link is a link, not a missing key.
	if err := os.Remove(filepath.Join(dir, "production.key.real")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := credentials.LoadKeys(dir, "production", noEnv); !errors.Is(err, credentials.ErrSymlink) {
		t.Fatalf("dangling key link: %v", err)
	}
	// The real files are accepted again once the links are gone.
	for _, name := range []string{"production.json.enc"} {
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".real", path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := credentials.ReadFile(dir, "production"); err != nil {
		t.Fatalf("regular credentials file: %v", err)
	}
}
