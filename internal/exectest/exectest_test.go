//go:build unix

package exectest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestWriteCreatesAnExecutableTheCallerCanRunAtOnce(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "tool")
	if err := Write(path, "#!/bin/sh\necho \"hello $1\"\n"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v, want 0700", info.Mode().Perm())
	}
	output, err := exec.Command(path, "world").Output() // #nosec G204 -- the file this test wrote.
	if err != nil || string(output) != "hello world\n" {
		t.Fatalf("output = %q, err = %v", output, err)
	}
}

func TestWriteReplacesAnExistingFileAndKeepsOddPathsLiteral(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "it's a $(tool) `x`")
	if err := os.WriteFile(path, []byte("old contents that are longer than the new ones"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, "#!/bin/sh\nexit 0\n"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "#!/bin/sh\nexit 0\n" {
		t.Fatalf("contents = %q", got)
	}
	if err := exec.Command(path).Run(); err != nil { // #nosec G204 -- the file this test wrote.
		t.Fatal(err)
	}
}

func TestWriteReportsAFailure(t *testing.T) {
	t.Parallel()
	err := Write(filepath.Join(t.TempDir(), "missing", "tool"), "#!/bin/sh\n")
	if err == nil || !strings.Contains(err.Error(), "write executable") {
		t.Fatalf("err = %v", err)
	}
}

// Many goroutines write and exec concurrently, the pattern that fails with
// "text file busy" when the file is written in-process.
func TestWriteThenExecIsSafeUnderConcurrentForks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 10 {
				path := filepath.Join(dir, fmt.Sprintf("tool-%d-%d", i, j))
				if err := Write(path, "#!/bin/sh\nexit 0\n"); err != nil {
					t.Error(err)
					return
				}
				if err := exec.Command(path).Run(); err != nil { // #nosec G204 -- the file this test wrote.
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
