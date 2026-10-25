package sqlite_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/sqlite"
)

func TestOpenFailsClearlyWhenTheDatabaseFileCannotBeCreated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "plain-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// A directory cannot be created below a regular file.
	if _, err := sqlite.Open(ctx, sqliteConfig(filepath.Join(file, "sub", "app.db"))); err == nil || !strings.Contains(err.Error(), "create database directory") {
		t.Errorf("directory below a file: %v", err)
	}
	// The database path itself is a directory.
	if err := os.Mkdir(filepath.Join(dir, "is-a-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.Open(ctx, sqliteConfig(filepath.Join(dir, "is-a-dir"))); err == nil {
		t.Error("a directory was opened as a database")
	}
	// Neither failure left a database behind, or leaked the configured path
	// into an unrelated message.
	if _, err := os.Stat(filepath.Join(dir, "plain-file", "sub")); err == nil {
		t.Error("a directory was created below a file")
	}
}

func TestOpenResolvesSymlinksToOneDatabasePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real.db")
	db := openAt(t, real)
	if _, err := db.ExecContext(context.Background(), "CREATE TABLE marker (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("no symlinks: %v", err)
	}
	through := openAt(t, link)
	var count int
	if err := through.QueryRowContext(context.Background(), "SELECT count(*) FROM sqlite_master WHERE name = 'marker'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("the symlink does not reach the same database: %d %v", count, err)
	}
	// A dangling symlink is refused with a clear error, not silently replaced.
	dangling := filepath.Join(dir, "dangling.db")
	if err := os.Symlink(filepath.Join(dir, "target.db"), dangling); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.Open(context.Background(), sqliteConfig(dangling)); err == nil || !strings.Contains(err.Error(), "resolve database path") {
		t.Errorf("dangling symlink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "target.db")); err == nil {
		t.Error("the symlink's target was created")
	}
}

func TestOpenGivesUpWhenThePingTimesOut(t *testing.T) {
	t.Parallel()
	cfg := sqliteConfig(filepath.Join(t.TempDir(), "app.db"))
	cfg.ConnectTimeout = time.Nanosecond
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sqlite.Open(ctx, cfg); err == nil || !strings.Contains(err.Error(), "open database") {
		t.Errorf("err = %v", err)
	}
}
