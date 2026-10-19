// Package storagetest is the conformance suite every storage.Storage must
// pass.
//
//	func TestStorage(t *testing.T) {
//	    storagetest.Run(t, func(t testing.TB) storage.Storage { ... an empty store the test cleans up ... })
//	}
package storagetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/storage"
)

// NewStorage returns an empty store that the test cleans up.
type NewStorage func(t testing.TB) storage.Storage

var ctx = context.Background()

// Run executes the suite.
func Run(t *testing.T, newStorage NewStorage) {
	t.Helper()
	tests := []struct {
		name string
		run  func(*testing.T, storage.Storage)
	}{
		{"PutOpenRoundTrip", roundTrip},
		{"StatDescribesWithoutReading", statDescribes},
		{"NestedKeys", nestedKeys},
		{"OverwriteReplacesContentAndMetadata", overwrite},
		{"DeleteIsIdempotent", deleteIdempotent},
		{"MissingKeyIsNotFound", missingKey},
		{"SizeLimitIsEnforcedAndStoresNothing", sizeLimit},
		{"SizeLimitBoundary", sizeBoundary},
		{"RejectsUnsafeKeys", unsafeKeys},
		{"RejectsInvalidOptions", invalidOptions},
		{"EmptyBodyAndBinaryContent", emptyAndBinary},
		{"FailedReadStoresNothing", failedRead},
		{"CancelledContext", cancelled},
		{"ConcurrentOverwritesNeverTear", concurrentOverwrites},
		{"ConcurrentDistinctKeys", concurrentDistinct},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { test.run(t, newStorage(t)) })
	}
}

func put(t *testing.T, s storage.Storage, key string, content []byte, opts storage.PutOptions) storage.Info {
	t.Helper()
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 1 << 20
	}
	info, err := s.Put(ctx, key, bytes.NewReader(content), opts)
	if err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
	return info
}

func read(t *testing.T, s storage.Storage, key string) ([]byte, storage.Info) {
	t.Helper()
	rc, info, err := s.Open(ctx, key)
	if err != nil {
		t.Fatalf("Open(%q): %v", key, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	return data, info
}

func roundTrip(t *testing.T, s storage.Storage) {
	content := []byte("hello, storage")
	sum := sha256.Sum256(content)
	stored := put(t, s, "docs/hello.txt", content, storage.PutOptions{ContentType: "Text/Plain; charset=UTF-8", Filename: "../../etc/Hello World.txt"})
	if stored.Key != "docs/hello.txt" || stored.Size != int64(len(content)) || stored.ContentType != "text/plain" {
		t.Fatalf("Put info = %+v", stored)
	}
	data, info := read(t, s, "docs/hello.txt")
	if !bytes.Equal(data, content) {
		t.Fatalf("content = %q", data)
	}
	if info.Size != int64(len(content)) || info.ContentType != "text/plain" || info.Filename != "Hello World.txt" {
		t.Fatalf("Open info = %+v, want normalized type and a sanitized file name", info)
	}
	if info.SHA256 != "" && info.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("SHA256 = %q, want %x", info.SHA256, sum)
	}
	if info.ModTime.IsZero() || time.Since(info.ModTime) > time.Minute {
		t.Fatalf("ModTime = %v", info.ModTime)
	}
}

func statDescribes(t *testing.T, s storage.Storage) {
	put(t, s, "a", []byte("12345"), storage.PutOptions{ContentType: "image/png"})
	info, err := s.Stat(ctx, "a")
	if err != nil || info.Size != 5 || info.ContentType != "image/png" || info.Key != "a" {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	put(t, s, "b", []byte("x"), storage.PutOptions{})
	if info, _ := s.Stat(ctx, "b"); info.ContentType != storage.OctetStream {
		t.Fatalf("default content type = %q, want %s", info.ContentType, storage.OctetStream)
	}
}

func nestedKeys(t *testing.T, s storage.Storage) {
	keys := []string{"a/b/c/d.txt", "a/b/other", "a/x", "v1.2/file-name_3.tar.gz"}
	for _, key := range keys {
		put(t, s, key, []byte(key), storage.PutOptions{})
	}
	for _, key := range keys {
		if data, _ := read(t, s, key); string(data) != key {
			t.Errorf("%s = %q", key, data)
		}
	}
	if _, err := s.Stat(ctx, "a/b"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat of a prefix = %v, want not found", err)
	}
	if _, _, err := s.Open(ctx, "a/x/y"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Open below a blob = %v, want not found", err)
	}
}

func overwrite(t *testing.T, s storage.Storage) {
	put(t, s, "k", []byte("first version"), storage.PutOptions{ContentType: "text/plain", Filename: "one.txt"})
	put(t, s, "k", []byte("2nd"), storage.PutOptions{ContentType: "image/png", Filename: "two.png"})
	data, info := read(t, s, "k")
	if string(data) != "2nd" || info.Size != 3 || info.ContentType != "image/png" || info.Filename != "two.png" {
		t.Fatalf("after overwrite: %q %+v", data, info)
	}
}

func deleteIdempotent(t *testing.T, s storage.Storage) {
	put(t, s, "dir/k", []byte("v"), storage.PutOptions{})
	if err := s.Delete(ctx, "dir/k"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Open(ctx, "dir/k"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Open after Delete = %v", err)
	}
	if err := s.Delete(ctx, "dir/k"); err != nil {
		t.Fatalf("second Delete = %v", err)
	}
	if err := s.Delete(ctx, "never/existed"); err != nil {
		t.Fatalf("Delete of a missing key = %v", err)
	}
}

func missingKey(t *testing.T, s storage.Storage) {
	if _, _, err := s.Open(ctx, "missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Open = %v", err)
	}
	if _, err := s.Stat(ctx, "missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat = %v", err)
	}
}

func sizeLimit(t *testing.T, s storage.Storage) {
	put(t, s, "old", []byte("keep me"), storage.PutOptions{})
	_, err := s.Put(ctx, "big", strings.NewReader(strings.Repeat("x", 5000)), storage.PutOptions{MaxBytes: 1000})
	if !errors.Is(err, storage.ErrTooLarge) {
		t.Fatalf("Put over the limit = %v, want ErrTooLarge", err)
	}
	if _, err := s.Stat(ctx, "big"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("an oversized upload left a blob behind: %v", err)
	}
	// Overwriting with an oversized body must keep the old content.
	_, err = s.Put(ctx, "old", strings.NewReader(strings.Repeat("y", 5000)), storage.PutOptions{MaxBytes: 1000})
	if !errors.Is(err, storage.ErrTooLarge) {
		t.Fatalf("overwrite over the limit = %v", err)
	}
	if data, _ := read(t, s, "old"); string(data) != "keep me" {
		t.Fatalf("a failed overwrite changed the blob: %q", data)
	}
}

func sizeBoundary(t *testing.T, s storage.Storage) {
	put(t, s, "exact", bytes.Repeat([]byte("a"), 100), storage.PutOptions{MaxBytes: 100})
	if _, err := s.Put(ctx, "plus-one", bytes.NewReader(bytes.Repeat([]byte("a"), 101)), storage.PutOptions{MaxBytes: 100}); !errors.Is(err, storage.ErrTooLarge) {
		t.Fatalf("limit+1 bytes = %v, want ErrTooLarge", err)
	}
}

func unsafeKeys(t *testing.T, s storage.Storage) {
	keys := []string{
		"", "/", "/abs", "../x", "a/../b", "a/..", "..", ".", "./a", "a/./b", ".hidden", "a/.hidden",
		"a//b", "a/", "a\\b", "..\\x", "a/b\x00", "a b", "a%2e%2e/b", "a\n", "C:/x", "~root", "a:b", "ключ",
		strings.Repeat("a", storage.MaxKeyLength+1), strings.Repeat("a/", 9) + "a", strings.Repeat("a", 129),
	}
	for _, key := range keys {
		label := fmt.Sprintf("%q", key)
		if len(label) > 40 {
			label = label[:40]
		}
		if _, err := s.Put(ctx, key, strings.NewReader("x"), storage.PutOptions{MaxBytes: 10}); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("Put(%s) = %v, want ErrInvalidKey", label, err)
		}
		if _, _, err := s.Open(ctx, key); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("Open(%s) = %v, want ErrInvalidKey", label, err)
		}
		if _, err := s.Stat(ctx, key); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("Stat(%s) = %v, want ErrInvalidKey", label, err)
		}
		if err := s.Delete(ctx, key); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("Delete(%s) = %v, want ErrInvalidKey", label, err)
		}
	}
}

func invalidOptions(t *testing.T, s storage.Storage) {
	for name, opts := range map[string]storage.PutOptions{
		"no limit":        {},
		"negative limit":  {MaxBytes: -1},
		"bad type":        {MaxBytes: 10, ContentType: "not a media type"},
		"no slash":        {MaxBytes: 10, ContentType: "text"},
		"header injected": {MaxBytes: 10, ContentType: "text/plain\r\nX-Evil: 1"},
	} {
		if _, err := s.Put(ctx, "k", strings.NewReader("x"), opts); err == nil {
			t.Errorf("%s: Put succeeded", name)
		}
	}
	if _, err := s.Stat(ctx, "k"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("rejected Put left a blob behind: %v", err)
	}
}

func emptyAndBinary(t *testing.T, s storage.Storage) {
	put(t, s, "empty", nil, storage.PutOptions{})
	if data, info := read(t, s, "empty"); len(data) != 0 || info.Size != 0 {
		t.Fatalf("empty blob = %q %+v", data, info)
	}
	bin := make([]byte, 70_000)
	for i := range bin {
		bin[i] = byte(i * 7)
	}
	put(t, s, "bin", bin, storage.PutOptions{})
	if data, _ := read(t, s, "bin"); !bytes.Equal(data, bin) {
		t.Fatal("binary content did not round-trip")
	}
}

type failingReader struct{ after int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.after <= 0 {
		return 0, errors.New("connection reset")
	}
	n := min(len(p), f.after)
	f.after -= n
	return n, nil
}

func failedRead(t *testing.T, s storage.Storage) {
	if _, err := s.Put(ctx, "partial", &failingReader{after: 1000}, storage.PutOptions{MaxBytes: 1 << 20}); err == nil {
		t.Fatal("a body that fails mid-stream was stored")
	}
	if _, err := s.Stat(ctx, "partial"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a failed upload left a blob behind: %v", err)
	}
}

func cancelled(t *testing.T, s storage.Storage) {
	put(t, s, "k", []byte("v"), storage.PutOptions{})
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Put(c, "k2", strings.NewReader("x"), storage.PutOptions{MaxBytes: 10}); !errors.Is(err, context.Canceled) {
		t.Errorf("Put = %v", err)
	}
	if _, _, err := s.Open(c, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Open = %v", err)
	}
	if _, err := s.Stat(c, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Stat = %v", err)
	}
	if err := s.Delete(c, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete = %v", err)
	}
	if _, err := s.Stat(ctx, "k2"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a cancelled Put stored something: %v", err)
	}
}

func concurrentOverwrites(t *testing.T, s storage.Storage) {
	const writers, rounds, size = 8, 20, 64 << 10
	contents := make([][]byte, writers)
	for i := range contents {
		contents[i] = bytes.Repeat([]byte{byte('A' + i)}, size)
	}
	put(t, s, "shared", contents[0], storage.PutOptions{})
	stop := make(chan struct{})
	var readers sync.WaitGroup
	torn := make(chan string, 1)
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rc, _, err := s.Open(ctx, "shared")
				if err != nil {
					continue
				}
				data, err := io.ReadAll(rc)
				_ = rc.Close()
				if err != nil || len(data) != size || len(bytes.Trim(data, string(data[:1]))) != 0 {
					select {
					case torn <- fmt.Sprintf("read %d bytes, err %v: not one writer's content", len(data), err):
					default:
					}
					return
				}
			}
		}()
	}
	var writersWG sync.WaitGroup
	for i := range writers {
		writersWG.Add(1)
		go func() {
			defer writersWG.Done()
			for range rounds {
				if _, err := s.Put(ctx, "shared", bytes.NewReader(contents[i]), storage.PutOptions{MaxBytes: size}); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
			}
		}()
	}
	writersWG.Wait()
	close(stop)
	readers.Wait()
	select {
	case msg := <-torn:
		t.Fatal("a reader saw a torn write: " + msg)
	default:
	}
}

func concurrentDistinct(t *testing.T, s storage.Storage) {
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("many/k%d", i)
			content := []byte(strings.Repeat(key, 100))
			if _, err := s.Put(ctx, key, bytes.NewReader(content), storage.PutOptions{MaxBytes: 1 << 20}); err != nil {
				t.Errorf("Put %s: %v", key, err)
				return
			}
			rc, _, err := s.Open(ctx, key)
			if err != nil {
				t.Errorf("Open %s: %v", key, err)
				return
			}
			defer func() { _ = rc.Close() }()
			if data, _ := io.ReadAll(rc); !bytes.Equal(data, content) {
				t.Errorf("%s content mismatch", key)
			}
		}()
	}
	wg.Wait()
}
