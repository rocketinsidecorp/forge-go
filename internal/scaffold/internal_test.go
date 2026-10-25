package scaffold

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupRemovesOnlyWhatGenerateCreated(t *testing.T) {
	t.Parallel()
	cause := errors.New("disk full")

	created := filepath.Join(t.TempDir(), "new-app")
	if err := os.MkdirAll(filepath.Join(created, "cmd"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(created, true, cause); !errors.Is(err, cause) || err != cause {
		t.Errorf("cleanup returned %v, want the cause unchanged", err)
	}
	if _, err := os.Stat(created); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the partially generated directory is still there: %v", err)
	}

	// A directory that existed before (empty, given by the user) stays.
	existing := t.TempDir()
	if err := cleanup(existing, false, cause); err != cause {
		t.Errorf("cleanup returned %v", err)
	}
	if _, err := os.Stat(existing); err != nil {
		t.Errorf("a directory Generate did not create was removed: %v", err)
	}

	// When even the removal fails both errors are reported.
	err := cleanup("bad\x00path", true, cause)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "remove partially generated") {
		t.Errorf("cleanup with a failing removal returned %v", err)
	}
}
