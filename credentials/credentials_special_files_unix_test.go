//go:build unix

package credentials_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/credentials"
)

// A socket cannot be opened for reading: the open fails after the lstat saw
// it, and the failure is reported as it is, neither as a missing file (which
// development would tolerate) nor as content.
func TestSocketsAtCredentialsPathsAreErrors(t *testing.T) {
	// A unix socket path is limited to about a hundred bytes: use a short directory.
	dir, err := os.MkdirTemp("", "cr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	for _, name := range []string{"production.key", "production.json.enc"} {
		listener, err := net.Listen("unix", filepath.Join(dir, name))
		if err != nil {
			t.Skipf("cannot create a unix socket here: %v", err)
		}
		t.Cleanup(func() { _ = listener.Close() })
	}
	if _, _, err := credentials.LoadKeys(dir, "production", noEnvironment); err == nil || errors.Is(err, credentials.ErrKeyMissing) || !strings.Contains(err.Error(), "open key file") {
		t.Errorf("a socket as the key file: %v", err)
	}
	if _, err := credentials.ReadFile(dir, "production"); err == nil || errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("a socket as the credentials file: %v", err)
	}
	if _, err := credentials.Open("development", credentials.Options{Dir: dir, Lookup: noEnvironment}); err != nil {
		// development has no credentials file of its own: only production's name is a socket.
		t.Errorf("an unrelated environment: %v", err)
	}
}

// A dangling symbolic link in the path of the key directory cannot be
// created through, and Init reports it without creating a key or a file.
func TestInitRefusesToCreateTheDirectoryThroughADanglingLink(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(base, "nowhere"), link); err != nil {
		t.Skipf("cannot create a symbolic link here: %v", err)
	}
	created, err := credentials.Init(filepath.Join(link, "credentials"), "production")
	if err == nil || len(created) != 0 {
		t.Fatalf("Init through a dangling link = %v, %v", created, err)
	}
	if _, err := os.Lstat(filepath.Join(base, "nowhere")); err == nil {
		t.Error("Init created the link's target")
	}
}
