package disk_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/storage"
)

// cancelingBody serves data and cancels ctx when it is read: with eof set,
// at the very end of the body, otherwise before the first byte is returned.
type cancelingBody struct {
	cancel context.CancelFunc
	data   string
	eof    bool
	read   bool
}

func (b *cancelingBody) Read(p []byte) (int, error) {
	if !b.read {
		b.read = true
		n := copy(p, b.data)
		if !b.eof {
			b.cancel()
		}
		return n, nil
	}
	if b.eof {
		b.cancel()
	}
	return 0, io.EOF
}

func TestPutStopsWhenTheRequestIsCanceledAndLeavesNothing(t *testing.T) {
	t.Parallel()
	for name, eof := range map[string]bool{"while the body is read": false, "right after the body ended": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, dir := newStore(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := s.Put(ctx, "doc.txt", &cancelingBody{cancel: cancel, data: "abc", eof: eof}, storage.PutOptions{MaxBytes: 1 << 20, ContentType: "text/plain"})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Put = %v, want context.Canceled", err)
			}
			if _, _, err := s.Open(context.Background(), "doc.txt"); !errors.Is(err, storage.ErrNotFound) {
				t.Errorf("a canceled upload left a blob: %v", err)
			}
			for _, tree := range []string{"tmp", "blobs", "meta"} {
				entries, err := os.ReadDir(filepath.Join(dir, tree))
				if err != nil || len(entries) != 0 {
					t.Errorf("%s holds %d entries after a canceled upload (err=%v)", tree, len(entries), err)
				}
			}
		})
	}
}

func TestPruneTempReportsAnUnreadableTemporaryDirectory(t *testing.T) {
	t.Parallel()
	s, dir := newStore(t)
	tmp := filepath.Join(dir, "tmp")
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneTemp(context.Background(), time.Hour); err == nil || !strings.Contains(err.Error(), "list temporary files") {
		t.Errorf("PruneTemp = %v", err)
	}
}

// A failure that is not "there is no such file" must not be reported as a
// missing file: the caller would treat a broken disk as an empty one.
func TestRealIOFailuresAreNotReportedAsMissingFiles(t *testing.T) {
	t.Parallel()
	s, dir := newStore(t)
	if err := os.Symlink("loop", filepath.Join(dir, "blobs", "loop")); err != nil {
		t.Skipf("symbolic links are not available: %v", err)
	}
	ctx := context.Background()
	if _, err := s.Stat(ctx, "loop"); err == nil || errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat through a symlink loop = %v, want an I/O error", err)
	}
	if _, _, err := s.Open(ctx, "loop"); err == nil || errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Open through a symlink loop = %v, want an I/O error", err)
	}
}
