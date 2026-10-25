package sqlite_test

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
)

func TestOpenReportsAFileThatIsNotADatabase(t *testing.T) {
	t.Parallel()
	cfg := sqlitetest.Config(t)
	path := cfg.SQLitePath()
	// O_EXCL leaves an existing file alone, so garbage stays garbage.
	if err := os.WriteFile(path, []byte(strings.Repeat("this is not a SQLite database. ", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(context.Background(), cfg)
	if err == nil {
		_ = db.Close()
		t.Fatal("a file that is not a database opened")
	}
	if !strings.Contains(err.Error(), "open database") {
		t.Errorf("error = %v", err)
	}
	if strings.Contains(err.Error(), "this is not") {
		t.Errorf("the file's contents leaked into the error: %v", err)
	}
}

func TestOpenRefusesADatabaseFileNameTheSystemCannotCreate(t *testing.T) {
	t.Parallel()
	cfg := config.Default().Database
	tooLong := filepath.ToSlash(filepath.Join(t.TempDir(), strings.Repeat("n", 300)+".db"))
	cfg.URL = config.NewSecret((&url.URL{Scheme: "sqlite", Path: "/" + strings.TrimPrefix(tooLong, "/")}).String())
	_, err := sqlite.Open(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "create database file") {
		t.Fatalf("Open = %v, want the file creation failure", err)
	}
}
