package disk_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/storage"
	"github.com/rocketinsidecorp/forge-go/storage/disk"
	"github.com/rocketinsidecorp/forge-go/storage/storagetest"
)

var bg = context.Background()

func newStore(t testing.TB) (*disk.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "storage")
	s, err := disk.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t testing.TB) storage.Storage {
		s, _ := newStore(t)
		return s
	})
}

func TestTraversalKeysNeverTouchTheFileSystemOutsideTheRoot(t *testing.T) {
	outer := t.TempDir()
	secret := filepath.Join(outer, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := disk.New(filepath.Join(outer, "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, key := range []string{"../secret.txt", "..", "a/../../secret.txt", "/etc/passwd", secret, "..%2fsecret.txt", `..\secret.txt`, "blobs/../../secret.txt"} {
		if _, _, err := s.Open(bg, key); err == nil {
			t.Errorf("Open(%q) succeeded", key)
		}
		if _, err := s.Put(bg, key, strings.NewReader("pwned"), storage.PutOptions{MaxBytes: 10}); err == nil {
			t.Errorf("Put(%q) succeeded", key)
		}
		if err := s.Delete(bg, key); err == nil {
			t.Errorf("Delete(%q) succeeded", key)
		}
	}
	if data, _ := os.ReadFile(secret); string(data) != "top secret" {
		t.Fatalf("the file outside the root was modified: %q", data)
	}
	entries, _ := os.ReadDir(outer)
	for _, e := range entries {
		if e.Name() != "root" && e.Name() != "secret.txt" {
			t.Errorf("unexpected file outside the root: %s", e.Name())
		}
	}
}

func TestSymlinkEscapeIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	outer := t.TempDir()
	if err := os.WriteFile(filepath.Join(outer, "secret.txt"), []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, dir := newStore(t)
	// Someone with write access to the volume plants links that point outside.
	if err := os.Symlink(outer, filepath.Join(dir, "blobs", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outer, "secret.txt"), filepath.Join(dir, "blobs", "link")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Open(bg, "escape/secret.txt"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Open through a directory symlink = %v, want not found", err)
	}
	if _, _, err := s.Open(bg, "link"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Open of an escaping file symlink = %v, want not found", err)
	}
	if _, err := s.Put(bg, "escape/new.txt", strings.NewReader("pwned"), storage.PutOptions{MaxBytes: 10}); err == nil {
		t.Error("Put through a directory symlink succeeded")
	}
	if _, err := os.Stat(filepath.Join(outer, "new.txt")); err == nil {
		t.Fatal("a file was written outside the root")
	}
}

func TestPutLeavesNoTemporaryFilesBehind(t *testing.T) {
	s, dir := newStore(t)
	_, _ = s.Put(bg, "ok", strings.NewReader("x"), storage.PutOptions{MaxBytes: 10})
	_, _ = s.Put(bg, "toolarge", strings.NewReader(strings.Repeat("x", 100)), storage.PutOptions{MaxBytes: 10})
	_, _ = s.Put(bg, "badtype", strings.NewReader("x"), storage.PutOptions{MaxBytes: 10, ContentType: "nope"})
	entries, err := os.ReadDir(filepath.Join(dir, "tmp"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("tmp = %v, %v; want empty", entries, err)
	}
}

func TestFilePermissionsArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	s, dir := newStore(t)
	if _, err := s.Put(bg, "a/b.txt", strings.NewReader("x"), storage.PutOptions{MaxBytes: 10}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]fs.FileMode{
		"blobs/a/b.txt":     0o600,
		"meta/a/b.txt.json": 0o600,
		"blobs/a":           0o750,
	} {
		info, err := os.Stat(filepath.Join(dir, path))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got&^want != 0 {
			t.Errorf("%s mode = %v, want no more than %v", path, got, want)
		}
	}
}

func TestMissingOrStaleMetadataFallsBackToOctetStream(t *testing.T) {
	s, dir := newStore(t)
	if _, err := s.Put(bg, "f", strings.NewReader("hello"), storage.PutOptions{MaxBytes: 10, ContentType: "text/html", Filename: "x.html"}); err != nil {
		t.Fatal(err)
	}
	metaFile := filepath.Join(dir, "meta", "f.json")

	if err := os.WriteFile(metaFile, []byte(`{"content_type":"text/html","size":999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, _ := s.Stat(bg, "f"); info.ContentType != storage.OctetStream || info.Filename != "" {
		t.Fatalf("metadata with a wrong size was trusted: %+v", info)
	}
	if err := os.WriteFile(metaFile, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, _ := s.Stat(bg, "f"); info.ContentType != storage.OctetStream {
		t.Fatalf("corrupt metadata was trusted: %+v", info)
	}
	if err := os.Remove(metaFile); err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat(bg, "f")
	if err != nil || info.ContentType != storage.OctetStream || info.Size != 5 {
		t.Fatalf("Stat without metadata = %+v, %v", info, err)
	}
}

func TestOpenReturnsASeekableReader(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Put(bg, "f", strings.NewReader("0123456789"), storage.PutOptions{MaxBytes: 100}); err != nil {
		t.Fatal(err)
	}
	rc, _, err := s.Open(bg, "f")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	seeker, ok := rc.(io.ReadSeeker)
	if !ok {
		t.Fatal("the disk adapter's reader must be seekable so that ranges work")
	}
	if _, err := seeker.Seek(7, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if rest, _ := io.ReadAll(rc); string(rest) != "789" {
		t.Fatalf("rest = %q", rest)
	}
}

func TestPruneTempRemovesOnlyOldTemporaryFiles(t *testing.T) {
	s, dir := newStore(t)
	old := filepath.Join(dir, "tmp", "old")
	fresh := filepath.Join(dir, "tmp", "fresh")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneTemp(bg, time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("PruneTemp = %d, %v; want 1", n, err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("old temporary file survived")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh temporary file was removed")
	}
	if _, err := s.PruneTemp(bg, time.Second); err == nil {
		t.Error("a tiny age must be refused: it could delete uploads in progress")
	}
}

func TestNewValidationAndConfig(t *testing.T) {
	if _, err := disk.New(""); err == nil {
		t.Error("empty root accepted")
	}
	if _, err := disk.NewFromConfig(config.Storage{}); err == nil {
		t.Error("unset FORGE_STORAGE_ROOT accepted")
	}
	dir := filepath.Join(t.TempDir(), "from", "config")
	s, err := disk.NewFromConfig(config.Storage{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := os.Stat(filepath.Join(dir, "blobs")); err != nil {
		t.Errorf("root was not prepared: %v", err)
	}
	// A root that is a file is refused.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.New(file); err == nil {
		t.Error("a file as root accepted")
	}
}

func TestLargeStreamedUploadRoundTrips(t *testing.T) {
	s, _ := newStore(t)
	const size = 32 << 20
	info, err := s.Put(bg, "big", io.LimitReader(zeroReader{}, size), storage.PutOptions{MaxBytes: size})
	if err != nil || info.Size != size {
		t.Fatalf("Put = %+v, %v", info, err)
	}
	rc, _, err := s.Open(bg, "big")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if n, _ := io.Copy(io.Discard, rc); n != size {
		t.Fatalf("read %d bytes", n)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
