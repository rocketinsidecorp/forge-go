//go:build unix

package credentials_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/credentials"
	"github.com/rocketinsidecorp/forge-go/encryption"
)

// The tests below need a file system that enforces permissions against the
// caller, which root is exempt from: they run on CI's non-root runner and are
// skipped for root.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not restrict root: run as another user to exercise this")
	}
}

// restrict sets the mode of path and restores a removable one at the end, so
// t.TempDir can clean up.
func restrict(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestUnreadableFilesAreErrorsNotMissingFiles(t *testing.T) {
	skipIfRoot(t)
	t.Parallel()
	dir := t.TempDir()
	key := newKey(t, 1)
	writeCredentials(t, dir, "production", document, key)
	if err := credentials.WriteKeyFile(dir, "production", []encryption.Key{key}, false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"production.key", "production.json.enc"} {
		if err := os.Chmod(filepath.Join(dir, name), 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := credentials.LoadKeys(dir, "production", noEnvironment); err == nil || errors.Is(err, credentials.ErrKeyMissing) || !strings.Contains(err.Error(), "open key file") {
		t.Errorf("an unreadable key file: %v", err)
	}
	if _, err := credentials.ReadFile(dir, "production"); err == nil || errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("an unreadable credentials file: %v", err)
	}
	// Development does not fall back to an empty store for it either.
	if _, err := credentials.Open("production", credentials.Options{Dir: dir, Lookup: noEnvironment}); err == nil || errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("Open with an unreadable file: %v", err)
	}
}

func TestDirectoriesThatCannotBeOpenedOrWrittenAreErrors(t *testing.T) {
	skipIfRoot(t)
	t.Parallel()
	key := newKey(t, 2)

	// No read permission: the directory cannot be opened.
	closed := filepath.Join(t.TempDir(), "closed")
	if err := os.Mkdir(closed, 0o700); err != nil {
		t.Fatal(err)
	}
	restrict(t, closed, 0o300)
	if err := credentials.PrepareKeyDir(closed); err == nil {
		t.Error("PrepareKeyDir of an unreadable directory succeeded")
	}
	if err := credentials.WriteFile(closed, "production", []byte("x"), false); err == nil {
		t.Error("WriteFile into an unreadable directory succeeded")
	}

	// No write permission: the temporary file cannot be created, and Init
	// reports it after reusing the key that is already there.
	sealed := filepath.Join(t.TempDir(), "readonly")
	if err := credentials.WriteKeyFile(sealed, "production", []encryption.Key{key}, false); err != nil {
		t.Fatal(err)
	}
	restrict(t, sealed, 0o500)
	if err := credentials.WriteFile(sealed, "production", []byte("x"), false); err == nil {
		t.Error("WriteFile into a read-only directory succeeded")
	}
	created, err := credentials.Init(sealed, "production")
	if err == nil || strings.Contains(err.Error(), "already exists") || len(created) != 0 {
		t.Errorf("Init in a read-only directory = %v, %v", created, err)
	}
	if _, err := os.Stat(filepath.Join(sealed, "production.json.enc")); err == nil {
		t.Error("a credentials file appeared in a read-only directory")
	}

	// A parent that cannot be written: the key directory cannot be created,
	// so Init creates nothing.
	parent := filepath.Join(t.TempDir(), "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	restrict(t, parent, 0o500)
	created, err = credentials.Init(filepath.Join(parent, "credentials"), "production")
	if err == nil || len(created) != 0 {
		t.Errorf("Init below a read-only parent = %v, %v", created, err)
	}
	if _, err := os.Stat(filepath.Join(parent, "credentials")); err == nil {
		t.Error("a directory was created below a read-only parent")
	}
}
