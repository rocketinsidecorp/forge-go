package disk_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/storage"
	"github.com/rocketinsidecorp/forge-go/storage/disk"
)

func put(t *testing.T, s *disk.Store, key, body string) error {
	t.Helper()
	_, err := s.Put(bg, key, strings.NewReader(body), storage.PutOptions{MaxBytes: 1 << 20, ContentType: "text/plain", Filename: "f.txt"})
	return err
}

func TestNewRefusesUnusableRoots(t *testing.T) {
	t.Parallel()
	if _, err := disk.New(""); err == nil {
		t.Error("an empty directory was accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.New(filepath.Join(file, "root")); err == nil || !strings.Contains(err.Error(), "create storage root") {
		t.Errorf("root below a file: %v", err)
	}
	// A subdirectory that cannot be created (a file is in its place).
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "blobs"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.New(root); err == nil || !strings.Contains(err.Error(), "create storage directory blobs") {
		t.Errorf("blobs is a file: %v", err)
	}
}

func TestPutRefusesBadInputBeforeWriting(t *testing.T) {
	t.Parallel()
	s, dir := newStore(t)
	ctx := bg
	if _, err := s.Put(ctx, "k", strings.NewReader("x"), storage.PutOptions{}); err == nil {
		t.Error("a missing MaxBytes was accepted")
	}
	if _, err := s.Put(ctx, "k", strings.NewReader("x"), storage.PutOptions{MaxBytes: 10, ContentType: "not a type"}); err == nil {
		t.Error("an invalid content type was accepted")
	}
	if _, err := s.Put(ctx, "k", nil, storage.PutOptions{MaxBytes: 10}); err == nil || !strings.Contains(err.Error(), "body") {
		t.Errorf("a nil body: %v", err)
	}
	if _, err := s.Put(ctx, "k", strings.NewReader(strings.Repeat("x", 11)), storage.PutOptions{MaxBytes: 10}); !errors.Is(err, storage.ErrTooLarge) {
		t.Errorf("an oversized body: %v", err)
	}
	if _, err := s.Put(ctx, "k", failingReader{}, storage.PutOptions{MaxBytes: 10}); err == nil {
		t.Error("a failing body was accepted")
	}
	// None of that left anything behind.
	for _, sub := range []string{"tmp", "blobs", "meta"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil || len(entries) != 0 {
			t.Errorf("%s holds %d entries (%v)", sub, len(entries), err)
		}
	}
	if _, _, err := s.Open(ctx, "k"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a refused Put stored something: %v", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("body failed") }

func TestPutFailsCleanlyWhenTheFileSystemRefuses(t *testing.T) {
	t.Parallel()
	t.Run("no temporary directory", func(t *testing.T) {
		t.Parallel()
		s, dir := newStore(t)
		if err := os.RemoveAll(filepath.Join(dir, "tmp")); err != nil {
			t.Fatal(err)
		}
		if err := put(t, s, "a.txt", "x"); err == nil || !strings.Contains(err.Error(), "temporary file") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a nested key blocked by a file", func(t *testing.T) {
		t.Parallel()
		s, dir := newStore(t)
		if err := os.WriteFile(filepath.Join(dir, "meta", "docs"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := put(t, s, "docs/a.txt", "x"); err == nil || !strings.Contains(err.Error(), "create storage directory") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("metadata cannot be stored", func(t *testing.T) {
		t.Parallel()
		s, dir := newStore(t)
		blocker := filepath.Join(dir, "meta", "a.txt.json")
		if err := os.MkdirAll(filepath.Join(blocker, "inside"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := put(t, s, "a.txt", "x"); err == nil || !strings.Contains(err.Error(), "store metadata") {
			t.Errorf("err = %v", err)
		}
		if _, _, err := s.Open(bg, "a.txt"); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("a failed Put left a blob: %v", err)
		}
	})
	t.Run("the blob cannot be stored", func(t *testing.T) {
		t.Parallel()
		s, dir := newStore(t)
		if err := os.MkdirAll(filepath.Join(dir, "blobs", "a.txt", "inside"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := put(t, s, "a.txt", "x"); err == nil || !strings.Contains(err.Error(), "store file") {
			t.Errorf("err = %v", err)
		}
		// The metadata written first is taken back.
		if _, err := os.Stat(filepath.Join(dir, "meta", "a.txt.json")); err == nil {
			t.Error("orphan metadata left behind")
		}
		entries, _ := os.ReadDir(filepath.Join(dir, "tmp"))
		if len(entries) != 0 {
			t.Errorf("temporary files left behind: %d", len(entries))
		}
	})
}

func TestOpenAndStatTreatNonFilesAsMissingAndHonourCancellation(t *testing.T) {
	t.Parallel()
	s, dir := newStore(t)
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Open(bg, "folder"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Open of a directory: %v", err)
	}
	if _, err := s.Stat(bg, "folder"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat of a directory: %v", err)
	}
	canceled, cancel := testContext(t)
	cancel()
	if _, _, err := s.Open(canceled, "k"); err == nil {
		t.Error("Open ignored a canceled context")
	}
	if _, err := s.Stat(canceled, "k"); err == nil {
		t.Error("Stat ignored a canceled context")
	}
	if err := s.Delete(canceled, "k"); err == nil {
		t.Error("Delete ignored a canceled context")
	}
	if _, err := s.Put(canceled, "k", strings.NewReader("x"), storage.PutOptions{MaxBytes: 1}); err == nil {
		t.Error("Put ignored a canceled context")
	}
	for _, call := range []func() error{
		func() error { _, _, err := s.Open(bg, "../x"); return err },
		func() error { _, err := s.Stat(bg, "../x"); return err },
	} {
		if err := call(); err == nil {
			t.Error("an invalid key was accepted")
		}
	}
}

func TestMetadataIsTrustedOnlyWhileItMatchesTheFile(t *testing.T) {
	t.Parallel()
	s, dir := newStore(t)
	if err := put(t, s, "a.txt", "hello"); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(dir, "meta", "a.txt.json")
	check := func(name, content string, wantType string, untrusted bool) {
		t.Helper()
		if err := os.WriteFile(meta, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := s.Stat(bg, "a.txt")
		if err != nil {
			t.Fatal(err)
		}
		if info.ContentType != wantType || info.Size != 5 {
			t.Errorf("%s: info = %+v, want content type %q", name, info, wantType)
		}
		if untrusted && (info.SHA256 != "" || info.Filename != "") {
			t.Errorf("%s: untrusted metadata leaked: %+v", name, info)
		}
	}
	check("corrupt", `{not json`, storage.OctetStream, true)
	check("wrong size", `{"content_type":"text/plain","size":99,"sha256":"abc","filename":"x"}`, storage.OctetStream, true)
	check("invalid type", `{"content_type":"not a type","size":5,"sha256":"abc"}`, storage.OctetStream, false)
	check("matching", `{"content_type":"text/plain","size":5,"sha256":"abc","filename":"f.txt"}`, "text/plain", false)
	if err := os.Remove(meta); err != nil {
		t.Fatal(err)
	}
	if info, err := s.Stat(bg, "a.txt"); err != nil || info.ContentType != storage.OctetStream {
		t.Errorf("without metadata: %+v %v", info, err)
	}
}

func TestDeleteReportsWhatItCannotRemove(t *testing.T) {
	t.Parallel()
	s, dir := newStore(t)
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "stuck", "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(bg, "stuck"); err == nil || !strings.Contains(err.Error(), "delete file") {
		t.Errorf("blob that is a non-empty directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "meta", "meta-stuck.json", "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := put(t, s, "free", "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(bg, "meta-stuck"); err == nil || !strings.Contains(err.Error(), "delete metadata") {
		t.Errorf("metadata that is a non-empty directory: %v", err)
	}
	if err := s.Delete(bg, "never-existed"); err != nil {
		t.Errorf("deleting a missing key must succeed: %v", err)
	}
}

func TestPruneTempRemovesOnlyOldFilesAndReportsFailures(t *testing.T) {
	t.Parallel()
	s, dir := newStore(t)
	if _, err := s.PruneTemp(bg, time.Second); err == nil {
		t.Error("a sub-minute age was accepted")
	}
	tmp := filepath.Join(dir, "tmp")
	old, fresh := filepath.Join(tmp, "old"), filepath.Join(tmp, "fresh")
	for _, path := range []string{old, fresh} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(tmp, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	for _, path := range []string{old, filepath.Join(tmp, "subdir")} {
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := s.PruneTemp(bg, time.Hour)
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("the old file survived")
	}
	for _, path := range []string{fresh, filepath.Join(tmp, "subdir")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed: %v", path, err)
		}
	}
	// A canceled context stops the sweep.
	canceled, cancel := testContext(t)
	cancel()
	if _, err := s.PruneTemp(canceled, time.Hour); err == nil {
		t.Error("PruneTemp ignored a canceled context")
	}
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneTemp(bg, time.Hour); err == nil || !strings.Contains(err.Error(), "temporary directory") {
		t.Errorf("without the directory: %v", err)
	}
}

func TestNewFromConfigNeedsARoot(t *testing.T) {
	t.Parallel()
	if _, err := disk.NewFromConfig(configStorage("")); err == nil {
		t.Error("an empty root was accepted")
	}
	s, err := disk.NewFromConfig(configStorage(filepath.Join(t.TempDir(), "root")))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func testContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithCancel(t.Context())
}

func configStorage(root string) config.Storage { return config.Storage{Root: root} }
