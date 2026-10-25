//go:build unix

package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/acme/autocert"
)

func TestDurableCachePutIsConfinedAndKeepsTheEntryWhenCanceled(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "certs")
	cache := durableCache{}
	cache.DirCache = autocert.DirCache(dir)
	ctx := context.Background()
	if err := cache.Put(ctx, "example.org", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if got, err := cache.Get(ctx, "example.org"); err != nil || string(got) != "first" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	// A canceled Put leaves the entry as it was and no temporary file behind.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := cache.Put(canceled, "example.org", []byte("second")); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled Put = %v", err)
	}
	if got, _ := cache.Get(ctx, "example.org"); string(got) != "first" {
		t.Errorf("a canceled Put overwrote the entry: %q", got)
	}
	// A name cannot climb out of the cache directory.
	if err := cache.Put(ctx, "../escape", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); err == nil {
		t.Error("the entry was written outside the cache directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}
	// A cache directory that cannot exist is an error.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := durableCache{}
	broken.DirCache = autocert.DirCache(filepath.Join(blocker, "certs"))
	if err := broken.Put(ctx, "example.org", []byte("x")); err == nil {
		t.Error("Put succeeded below a regular file")
	}
	if err := syncDir(filepath.Join(dir, "missing")); err == nil {
		t.Error("syncDir of a missing directory succeeded")
	}
}
