//go:build unix

package frontend

import (
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
)

// TestWriteAtomicReportsAFailedWriteAndRemovesTheTemporaryFile makes the write
// fail the way a full disk does, with a file size limit (EFBIG; SIGXFSZ is
// ignored meanwhile). It is not parallel on purpose: the limit belongs to the
// process, and parallel tests only start after the sequential ones are done.
func TestWriteAtomicReportsAFailedWriteAndRemovesTheTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	project, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = project.Close() }()

	var previous syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
		t.Skipf("no file size limit: %v", err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	limited := previous
	limited.Cur = 10
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited); err != nil {
		t.Skipf("cannot lower the file size limit: %v", err)
	}
	failure := writeAtomic(project, "big.js", make([]byte, 4096))
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
		t.Fatalf("cannot restore the file size limit: %v", err)
	}
	if failure == nil {
		t.Fatal("a write past the file size limit succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Errorf("a temporary or partial file was left: %v %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "big.js")); !os.IsNotExist(err) {
		t.Errorf("the target exists: %v", err)
	}
}
