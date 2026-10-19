// Package disk is the local-disk adapter of Forge's file storage (ADR 0028).
//
// Files live under one root directory, opened once as an os.Root: the
// operating system then refuses any path that leaves the root, whether through
// "..", an absolute path or a symbolic link, on top of storage.ValidateKey
// which rejects such keys before they reach the file system. Writes are atomic:
// the content goes to a temporary file in the root, is flushed to disk and
// renamed over the final name, so readers see the old file or the new one, never
// a partial one, and a crash leaves at most an orphaned temporary file (see
// PruneTemp). Files are created 0600 and directories 0750.
//
// Layout below the root:
//
//	blobs/<key>        the content
//	meta/<key>.json    content type, file name, size and SHA-256 digest
//	tmp/               uploads in progress
//
// The metadata is renamed into place before the content, so a blob is never
// visible with metadata that belongs to a different upload except in the
// instant between two overwrites of one key; keys of user content are unique
// (storage.Receive uses UUIDv7), so overwrites are the exception. A blob whose
// metadata is missing or does not match is served as
// application/octet-stream, which the handler downloads.
//
// Disk storage suits one application host (or hosts sharing a volume with
// working rename and fsync semantics, not NFS). Use a different adapter for
// object storage.
package disk

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/storage"
)

const (
	dirMode  = 0o750
	fileMode = 0o600
	// maxMetaBytes bounds the metadata read for one blob.
	maxMetaBytes = 8 << 10
)

// Store is a storage.Storage on a local directory. It is safe for concurrent
// use; Close it when the application shuts down.
type Store struct {
	root *os.Root
}

var _ storage.Storage = (*Store)(nil)

// New opens (creating it if needed) the storage directory dir.
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("storage root directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root: %w", err)
	}
	if err := os.MkdirAll(abs, dirMode); err != nil {
		return nil, fmt.Errorf("create storage root: %w", err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("open storage root: %w", err)
	}
	for _, sub := range []string{"blobs", "meta", "tmp"} {
		if err := root.MkdirAll(sub, dirMode); err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("create storage directory %s: %w", sub, err)
		}
	}
	return &Store{root: root}, nil
}

// NewFromConfig opens the directory of FORGE_STORAGE_ROOT.
func NewFromConfig(cfg config.Storage) (*Store, error) {
	if cfg.Root == "" {
		return nil, errors.New("FORGE_STORAGE_ROOT is required for disk storage")
	}
	return New(cfg.Root)
}

// Close releases the root directory.
func (s *Store) Close() error { return s.root.Close() }

type metadata struct {
	ContentType string `json:"content_type"`
	Filename    string `json:"filename,omitempty"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
}

func blobPath(key string) string { return "blobs/" + key }
func metaPath(key string) string { return "meta/" + key + ".json" }

// Put implements storage.Storage.
func (s *Store) Put(ctx context.Context, key string, body io.Reader, opts storage.PutOptions) (storage.Info, error) {
	if err := ctx.Err(); err != nil {
		return storage.Info{}, err
	}
	if err := storage.ValidateKey(key); err != nil {
		return storage.Info{}, err
	}
	if opts.MaxBytes < 1 {
		return storage.Info{}, errors.New("storage put needs a positive MaxBytes")
	}
	contentType, err := storage.NormalizeContentType(opts.ContentType)
	if err != nil {
		return storage.Info{}, err
	}
	if body == nil {
		return storage.Info{}, errors.New("storage put needs a body")
	}

	tmpData, err := tempName()
	if err != nil {
		return storage.Info{}, err
	}
	tmpMeta := tmpData + ".meta"
	defer func() { // whatever a failure left behind
		_ = s.root.Remove(tmpData)
		_ = s.root.Remove(tmpMeta)
	}()

	file, err := s.root.OpenFile(tmpData, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return storage.Info{}, fmt.Errorf("create temporary file: %w", err)
	}
	digest := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, digest), &limitedReader{ctx: ctx, r: body, remaining: opts.MaxBytes + 1})
	if copyErr == nil && written > opts.MaxBytes {
		copyErr = storage.ErrTooLarge
	}
	if copyErr == nil {
		copyErr = file.Sync()
	}
	if closeErr := file.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return storage.Info{}, copyErr
	}

	meta := metadata{ContentType: contentType, Filename: storage.SanitizeFilename(opts.Filename), SHA256: hex.EncodeToString(digest.Sum(nil)), Size: written}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return storage.Info{}, err
	}
	if err := s.root.WriteFile(tmpMeta, encoded, fileMode); err != nil {
		return storage.Info{}, fmt.Errorf("write metadata: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return storage.Info{}, err
	}
	if dir := path.Dir(key); dir != "." {
		for _, tree := range []string{"blobs/", "meta/"} {
			if err := s.root.MkdirAll(tree+dir, dirMode); err != nil {
				return storage.Info{}, fmt.Errorf("create storage directory: %w", err)
			}
		}
	}
	// Metadata first: the blob is the commit point.
	if err := s.root.Rename(tmpMeta, metaPath(key)); err != nil {
		return storage.Info{}, fmt.Errorf("store metadata: %w", err)
	}
	if err := s.root.Rename(tmpData, blobPath(key)); err != nil {
		_ = s.root.Remove(metaPath(key))
		return storage.Info{}, fmt.Errorf("store file: %w", err)
	}
	return storage.Info{
		Key: key, Size: written, ContentType: contentType, Filename: meta.Filename,
		SHA256: meta.SHA256, ModTime: time.Now().UTC(),
	}, nil
}

// Open implements storage.Storage. The reader is an *os.File, so it is
// seekable.
func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, storage.Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, storage.Info{}, err
	}
	if err := storage.ValidateKey(key); err != nil {
		return nil, storage.Info{}, err
	}
	file, err := s.root.Open(blobPath(key))
	if err != nil {
		return nil, storage.Info{}, notFound(err)
	}
	stat, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, storage.Info{}, err
	}
	if !stat.Mode().IsRegular() {
		_ = file.Close()
		return nil, storage.Info{}, storage.ErrNotFound
	}
	return file, s.info(key, stat), nil
}

// Stat implements storage.Storage.
func (s *Store) Stat(ctx context.Context, key string) (storage.Info, error) {
	if err := ctx.Err(); err != nil {
		return storage.Info{}, err
	}
	if err := storage.ValidateKey(key); err != nil {
		return storage.Info{}, err
	}
	stat, err := s.root.Stat(blobPath(key))
	if err != nil {
		return storage.Info{}, notFound(err)
	}
	if !stat.Mode().IsRegular() {
		return storage.Info{}, storage.ErrNotFound
	}
	return s.info(key, stat), nil
}

// info combines the file's own facts with its metadata, which is trusted only
// while its recorded size matches the file.
func (s *Store) info(key string, stat fs.FileInfo) storage.Info {
	info := storage.Info{Key: key, Size: stat.Size(), ContentType: storage.OctetStream, ModTime: stat.ModTime().UTC()}
	file, err := s.root.Open(metaPath(key))
	if err != nil {
		return info
	}
	defer func() { _ = file.Close() }()
	var meta metadata
	if err := json.NewDecoder(io.LimitReader(file, maxMetaBytes)).Decode(&meta); err != nil || meta.Size != stat.Size() {
		return info
	}
	if contentType, err := storage.NormalizeContentType(meta.ContentType); err == nil {
		info.ContentType = contentType
	}
	info.Filename = storage.SanitizeFilename(meta.Filename)
	info.SHA256 = meta.SHA256
	return info
}

// Delete implements storage.Storage.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := storage.ValidateKey(key); err != nil {
		return err
	}
	// The blob first: once it is gone the key is gone, whatever happens to the
	// metadata after.
	if err := s.root.Remove(blobPath(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete file: %w", err)
	}
	if err := s.root.Remove(metaPath(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete metadata: %w", err)
	}
	return nil
}

// PruneTemp removes temporary files older than olderThan, left by uploads
// that crashed. It returns how many it removed. Call it periodically (for
// example from jobs.Scheduler) with a generous age, such as an hour.
func (s *Store) PruneTemp(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan < time.Minute {
		return 0, errors.New("storage temporary file age must be at least one minute")
	}
	dir, err := s.root.Open("tmp")
	if err != nil {
		return 0, fmt.Errorf("open temporary directory: %w", err)
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return 0, fmt.Errorf("list temporary files: %w", err)
	}
	removed := 0
	cutoff := time.Now().Add(-olderThan)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			continue
		}
		if err := s.root.Remove("tmp/" + entry.Name()); err == nil {
			removed++
		}
	}
	return removed, nil
}

func tempName() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate temporary file name: %w", err)
	}
	return "tmp/" + hex.EncodeToString(b[:]), nil
}

// notFound maps "there is no such file" to storage.ErrNotFound: a missing
// path, a key below an existing blob (not a directory) and a path that tries to
// leave the root through a symbolic link. Real I/O failures stay errors.
func notFound(err error) error {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return err
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR), strings.Contains(err.Error(), "path escapes from parent"):
		return storage.ErrNotFound.WithCause(err)
	default:
		return err
	}
}

// limitedReader reads at most remaining bytes and stops at the first sign of
// a cancelled context, so a slow or endless body cannot outlive its request.
type limitedReader struct {
	ctx       context.Context
	r         io.Reader
	remaining int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if err := l.ctx.Err(); err != nil {
		return 0, err
	}
	if l.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	return n, err
}
