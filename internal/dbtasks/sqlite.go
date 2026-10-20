package dbtasks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	sqliteDirPermissions  = 0o750
	sqliteFilePermissions = 0o600
)

// sqliteCompanions are the files SQLite and Forge keep next to a database:
// the write-ahead log, its shared memory, the rollback journal and the
// migration lock.
var sqliteCompanions = []string{"-wal", "-shm", "-journal", "-migrate.lock"}

func (t *Target) sqliteExists() (bool, error) {
	info, err := os.Stat(t.Name)
	switch {
	case err == nil && info.Mode().IsRegular():
		return true, nil
	case err == nil:
		return false, fmt.Errorf("%s exists and is not a regular file", t.Name)
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// sqliteCreate creates an empty database file (a valid empty database) with
// its directory, leaving an existing database untouched.
func (t *Target) sqliteCreate() (bool, error) {
	if err := os.MkdirAll(filepath.Dir(t.Name), sqliteDirPermissions); err != nil {
		return false, fmt.Errorf("create database directory: %w", err)
	}
	file, err := os.OpenFile(t.Name, os.O_RDWR|os.O_CREATE|os.O_EXCL, sqliteFilePermissions) // #nosec G304 -- the operator-configured database file
	switch {
	case err == nil:
		return true, file.Close()
	case errors.Is(err, os.ErrExist):
		return false, nil
	default:
		return false, fmt.Errorf("create database file: %w", err)
	}
}

// sqliteDrop removes the database file and its companions. It refuses
// anything that is not a regular file.
func (t *Target) sqliteDrop() (bool, error) {
	exists, err := t.sqliteExists()
	if err != nil || !exists {
		return false, err
	}
	if err := os.Remove(t.Name); err != nil {
		return false, fmt.Errorf("remove database file: %w", err)
	}
	for _, suffix := range sqliteCompanions {
		if err := os.Remove(t.Name + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return true, fmt.Errorf("remove %s: %w", filepath.Base(t.Name+suffix), err)
		}
	}
	return true, nil
}
