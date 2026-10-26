package devloop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/waittest"
)

const (
	testPoll     = 10 * time.Millisecond
	testDebounce = 60 * time.Millisecond
	testWait     = 5 * time.Second
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	name := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func startWatch(t *testing.T, root string) <-chan Change {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	changes, err := Watch(ctx, Options{Root: root, PollInterval: testPoll, Debounce: testDebounce})
	if err != nil {
		t.Fatal(err)
	}
	return changes
}

func expectChange(t *testing.T, changes <-chan Change) Change {
	t.Helper()
	select {
	case change, ok := <-changes:
		if !ok {
			t.Fatal("watcher closed unexpectedly")
		}
		return change
	case <-time.After(testWait):
		t.Fatal("no change reported")
		return Change{}
	}
}

func expectQuiet(t *testing.T, changes <-chan Change, wait time.Duration) {
	t.Helper()
	select {
	case change := <-changes:
		t.Fatalf("unexpected change %+v", change)
	case <-time.After(wait):
	}
}

func TestWatchDebouncesBurstsIntoOneBatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, "app/a.go", "package app\n")
	write(t, root, "app/views/index.gohtml", "<p>1</p>\n")
	changes := startWatch(t, root)

	write(t, root, "app/a.go", "package app // 1\n")
	write(t, root, "app/views/index.gohtml", "<p>22</p>\n")
	write(t, root, "db/migrations/20260101000000_x.up.sql", "SELECT 1;\n")

	change := expectChange(t, changes)
	if change.Count != 3 || change.EnvOnly {
		t.Fatalf("change = %+v, want one batch of 3 files", change)
	}
	want := []string{"app/a.go", "app/views/index.gohtml", "db/migrations/20260101000000_x.up.sql"}
	for i, name := range want {
		if change.Paths[i] != name {
			t.Fatalf("paths = %v, want %v", change.Paths, want)
		}
	}
	expectQuiet(t, changes, 5*testDebounce)
}

func TestWatchKeepsExtendingTheBatchWhileFilesKeepChanging(t *testing.T) {
	t.Parallel()
	// Fake time: the writes are 60 ms apart against a 300 ms debounce, a
	// margin that a loaded runner could eat in real time and split the
	// batch in two.
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "main.go", "package main\n")
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		changes, err := Watch(ctx, Options{Root: root, PollInterval: testPoll, Debounce: 300 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}

		var lastWrite time.Time
		for i := 1; i <= 6; i++ {
			write(t, root, "main.go", "package main\n// "+strings.Repeat("x", i)+"\n")
			lastWrite = time.Now()
			time.Sleep(60 * time.Millisecond) // fake-clock: the 60ms write cadence stays inside the 300ms debounce on the bubble clock
		}
		change := expectChange(t, changes)
		if change.Count != 1 {
			t.Fatalf("change = %+v", change)
		}
		if quiet := time.Since(lastWrite); quiet < 300*time.Millisecond {
			t.Fatalf("batch was reported %s after the last write; it must wait for the debounce", quiet)
		}
		expectQuiet(t, changes, 600*time.Millisecond)
	})
}

func TestWatchIgnoresUnwatchedFilesAndDirectories(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, "main.go", "package main\n")
	changes := startWatch(t, root)

	for _, name := range []string{
		".git/HEAD.go", "tmp/mail/a.html", "vendor/x/x.go", "node_modules/x/index.js", "bin/app.json",
		"storage/dev.db", "main_test.go", "README.md", "main.go~", ".#main.go", "db/schema.sql", ".hidden/x.go", "storage/uploads/a.png",
	} {
		write(t, root, name, "ignored\n")
	}
	expectQuiet(t, changes, 6*testDebounce)

	write(t, root, "app/new.go", "package app\n")
	if change := expectChange(t, changes); change.Count != 1 || change.Paths[0] != "app/new.go" {
		t.Fatalf("change = %+v", change)
	}
}

func TestWatchReportsEnvFilesAsRestartOnly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, ".env.development", "A=1\n")
	write(t, root, "main.go", "package main\n")
	changes := startWatch(t, root)

	write(t, root, ".env.development", "A=22\n")
	write(t, root, ".env.local", "B=1\n")
	change := expectChange(t, changes)
	if !change.EnvOnly || change.Count != 2 {
		t.Fatalf("change = %+v, want env-only batch of 2", change)
	}

	write(t, root, ".env.development", "A=333\n")
	write(t, root, "main.go", "package main // changed\n")
	if change := expectChange(t, changes); change.EnvOnly {
		t.Fatalf("a source change in the batch must force a rebuild: %+v", change)
	}

	// Env files below the root are not the application's.
	write(t, root, "sub/.env.local", "C=1\n")
	expectQuiet(t, changes, 6*testDebounce)
}

func TestWatchSeesDeletions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, "gone.go", "package x\n")
	changes := startWatch(t, root)
	if err := os.Remove(filepath.Join(root, "gone.go")); err != nil {
		t.Fatal(err)
	}
	if change := expectChange(t, changes); change.Paths[0] != "gone.go" {
		t.Fatalf("change = %+v", change)
	}
}

func TestWatchStopsAndClosesWithContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	changes, err := Watch(ctx, Options{Root: t.TempDir(), PollInterval: testPoll})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case _, ok := <-changes:
		if ok {
			t.Fatal("unexpected change")
		}
	case <-time.After(testWait):
		t.Fatal("watcher goroutine did not stop")
	}
}

func TestWatchRefusesTreesBeyondTheBound(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		write(t, root, name, "package x\n")
	}
	_, err := Watch(context.Background(), Options{Root: root, MaxFiles: 2})
	if err == nil {
		t.Fatal("want ErrTooManyFiles")
	}
}

func TestWatchRequiresAnExistingRoot(t *testing.T) {
	t.Parallel()
	if _, err := Watch(context.Background(), Options{Root: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("want an error for a missing root")
	}
}

func TestWatchReportsScanFailuresOncePerRun(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, "a.go", "package x\n")
	write(t, root, "b.go", "package x\n")
	var mu sync.Mutex
	var messages []string
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_, err := Watch(ctx, Options{Root: root, PollInterval: testPoll, MaxFiles: 2, Logf: func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		messages = append(messages, fmt.Sprintf(format, args...))
	}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), messages...) }
	count := func() int { return len(snapshot()) }

	write(t, root, "c.go", "package x\n") // now beyond the bound: every poll fails
	waittest.Poll(testWait, func() bool { return count() > 0 })
	time.Sleep(20 * testPoll) // real-clock: absence check, many more failing polls of the watcher's own real poll interval must not repeat the message
	if got := snapshot(); len(got) != 1 || !strings.Contains(got[0], "cannot scan") {
		t.Fatalf("want exactly one \"cannot scan\" message for one run of failures, got %v", got)
	}

	if err := os.Remove(filepath.Join(root, "c.go")); err != nil {
		t.Fatal(err)
	}
	waittest.Poll(testWait, func() bool { return count() >= 2 })
	if got := snapshot(); len(got) != 2 || !strings.Contains(got[1], "again") {
		t.Fatalf("recovery must be reported once: %v", got)
	}
}
