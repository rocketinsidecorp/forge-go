package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sync"

	sqlite3 "modernc.org/sqlite/lib"
)

// lockFileSuffix names the migration lock file next to the database:
// storage/app.db is locked through storage/app.db-migrate.lock.
const lockFileSuffix = "-migrate.lock"

// migrationLock is the cross-process migration lock. SQLite has no advisory
// locks, so the lock is a write transaction held open on a separate, always
// empty SQLite database next to the application's.
//
// SQLite implements that transaction with an operating-system file lock
// (POSIX fcntl locks on Unix, LockFileEx on Windows), and the kernel releases
// such locks when the holding process exits for any reason. A crashed,
// killed or OOM-terminated migrator therefore never leaves a stale lock
// behind, and no time bound is needed: unlike a lock row with an expiry,
// a migration that legitimately runs for hours cannot lose its lock to a
// second migrator. SQLite also tracks these locks across connections within
// one process, so migrators in the same process exclude each other too.
//
// Taking the lock never waits inside SQLite: the lock database has no busy
// timeout, so BEGIN IMMEDIATE either takes it or fails at once with
// SQLITE_BUSY, and the migration engine polls.
//
// The lock works for every process on the host that shares the file,
// including containers sharing a volume. Like SQLite itself, it is not
// reliable on network file systems.
type migrationLock struct {
	path string
	mu   sync.Mutex
	held map[*sql.Conn]heldLock
}

type heldLock struct {
	db *sql.DB
	tx *sql.Tx
}

func newMigrationLock(databasePath string) *migrationLock {
	return &migrationLock{path: databasePath + lockFileSuffix, held: make(map[*sql.Conn]heldLock)}
}

// tryLock takes the lock for session without waiting.
func (l *migrationLock) tryLock(ctx context.Context, session *sql.Conn) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.held[session]; ok {
		return false, errors.New("migration lock is already held by this session")
	}
	if _, err := createFile(l.path); err != nil {
		return false, err
	}
	params := url.Values{}
	params.Set("_busy_timeout", "0")
	params.Set("_txlock", "immediate")
	db, err := connect(l.path, params, nil)
	if err != nil {
		return false, err
	}
	db.SetMaxOpenConns(1)
	// The transaction lives until unlock, not until ctx ends: database/sql
	// would otherwise roll it back, releasing the lock mid-migration.
	tx, err := db.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		_ = db.Close()
		if isBusy(err) {
			return false, nil
		}
		return false, fmt.Errorf("lock %s: %w", l.path, err)
	}
	l.held[session] = heldLock{db: db, tx: tx}
	return true, nil
}

// unlock releases the lock tryLock took for session.
func (l *migrationLock) unlock(session *sql.Conn) error {
	l.mu.Lock()
	lock, ok := l.held[session]
	delete(l.held, session)
	l.mu.Unlock()
	if !ok {
		return errors.New("migration lock is not held by this session")
	}
	// Closing the connection releases the file lock even if the rollback
	// fails.
	return errors.Join(lock.tx.Rollback(), lock.db.Close())
}

func isBusy(err error) bool {
	primary := ErrorCode(err) & 0xff
	return primary == sqlite3.SQLITE_BUSY || primary == sqlite3.SQLITE_LOCKED
}
