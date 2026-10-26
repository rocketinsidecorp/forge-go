// Package sqlite is Forge's SQLite adapter: a database/sql pool on the pure-Go
// modernc.org/sqlite driver (so binaries keep CGO_ENABLED=0 and the static
// distroless image), error translation into fault errors and SQL-file
// migrations.
//
// Every connection gets the same settings, from Forge rather than the URL
// (FORGE_DATABASE_URL takes no SQLite parameters):
//
//   - foreign_keys=ON: SQLite ships with foreign keys disabled.
//   - journal_mode=WAL: readers do not block the writer and the writer does
//     not block readers.
//   - synchronous=NORMAL: safe with WAL. A committed transaction survives an
//     application crash; only a power loss or OS crash can lose the most
//     recent commits, never corrupt the database.
//   - busy_timeout from FORGE_DATABASE_CONNECT_TIMEOUT: a connection waits
//     that long for the write lock instead of failing at once.
//   - immediate transactions: BEGIN takes the write lock up front, so two
//     transactions that read then write never deadlock on the lock upgrade
//     (which busy_timeout cannot resolve).
//   - read-only transactions (sqldb.TxOptions.ReadOnly) begin deferred, so
//     they never wait for the writer, and run under PRAGMA query_only: a
//     write fails with SQLITE_READONLY, translated to internal_error (500)
//     like a write in a read-only transaction on PostgreSQL. Isolation must
//     be sql.LevelDefault or sql.LevelSerializable (what SQLite provides);
//     other levels fail with ErrIsolationLevel.
//   - defensive mode and no double-quoted string literals: statements cannot
//     corrupt the file through writable_schema, and "name" is always an
//     identifier, as in standard SQL.
//
// SQLite has no server-side statement timeout, so FORGE_DATABASE_STATEMENT_TIMEOUT
// does not apply: a statement runs until it finishes or its context ends, in
// which case the driver interrupts it. Bound request work with contexts.
//
// Types: store UUIDv7 identifiers as TEXT (uuid.UUID binds as its canonical
// lowercase string, which sorts in generation order). time.Time parameters
// are written as TEXT in UTC, "2006-01-02 15:04:05.999999999+00:00", which
// sorts chronologically and which SQLite's date functions understand. Columns
// declared TIMESTAMP, DATETIME or DATE scan back into time.Time in UTC; STRICT
// tables only allow TEXT for them, which scans as string.
//
// SQLite serves one application instance on one host: the database is a file,
// and its locks do not work reliably over network file systems.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	driver "modernc.org/sqlite"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/sqldb"
)

const (
	dirPermissions  = 0o750
	filePermissions = 0o600
)

// Open creates a connection pool for the database file cfg points at,
// creating the file and its directory when missing, and verifies it within
// cfg.ConnectTimeout, so misconfiguration fails at startup rather than on the
// first request.
func Open(ctx context.Context, cfg config.Database, opts ...Option) (*sqldb.DB, error) {
	obs, poolName, err := observer(opts)
	if err != nil {
		return nil, err
	}
	path, err := prepareFile(cfg)
	if err != nil {
		return nil, err
	}
	db, err := openDB(path, cfg.ConnectTimeout, obs)
	if err != nil {
		return nil, err
	}
	pool, err := sqldb.New(db, cfg, classify)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := obs.ObservePool(poolName, pool); err != nil {
		_ = db.Close()
		return nil, err
	}
	return pool, nil
}

// prepareFile validates cfg and returns the absolute path of its database
// file, creating the directory (0750) and an empty database file (0600) when
// they do not exist, with symlinks resolved. SQLite gives its -wal and -shm files the database file's
// permissions, so they stay private too.
func prepareFile(cfg config.Database) (string, error) {
	if cfg.URL.IsZero() {
		return "", errors.New("database is not configured: set FORGE_DATABASE_URL")
	}
	if adapter := cfg.Adapter(); adapter != config.AdapterSQLite {
		return "", fmt.Errorf("FORGE_DATABASE_URL selects the %s adapter, but this application uses sqlite", adapter)
	}
	return createFile(cfg.SQLitePath())
}

func createFile(path string) (string, error) {
	if path == "" || strings.Contains(path, ":memory:") {
		return "", errors.New("FORGE_DATABASE_URL must be sqlite:relative/path.db or sqlite:///absolute/path.db")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	// The path is the operator-configured database file (FORGE_DATABASE_URL),
	// never request input.
	if err := os.MkdirAll(filepath.Dir(path), dirPermissions); err != nil { // #nosec G703 -- operator-configured path, see above
		return "", fmt.Errorf("create database directory: %w", err)
	}
	// An empty file is a valid empty database. O_EXCL leaves an existing
	// database untouched.
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, filePermissions) // #nosec G304 G703 -- the path is the operator-configured database file
	switch {
	case err == nil:
		if err := file.Close(); err != nil {
			return "", fmt.Errorf("create database file: %w", err)
		}
	case !errors.Is(err, os.ErrExist):
		return "", fmt.Errorf("create database file: %w", err)
	}
	// One database, one path: through a symlink the file would otherwise
	// get a second migration lock file.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	return resolved, nil
}

// openDB returns a pool whose every connection carries Forge's settings.
func openDB(path string, busyTimeout time.Duration, obs *sqldb.Observer) (*sql.DB, error) {
	params := url.Values{}
	params.Set("_busy_timeout", strconv.FormatInt(max(busyTimeout.Milliseconds(), 1), 10))
	params.Set("_foreign_keys", "1")
	params.Set("_journal_mode", "WAL")
	params.Set("_synchronous", "NORMAL")
	params.Set("_txlock", "immediate")
	params.Set("_time_format", "sqlite")
	params.Set("_timezone", "UTC")
	params.Set("_defensive", "1")
	params.Set("_dqs", "0")
	return connect(path, params, obs)
}

// connect opens path through a file: URI. The path is percent-encoded, so
// characters such as '?' and '#' in a file name stay part of the name and can
// never inject connection parameters.
func connect(path string, params url.Values, obs *sqldb.Observer) (*sql.DB, error) {
	base, err := driver.NewConnector(fileURI(path, params))
	if err != nil {
		return nil, fmt.Errorf("configure database connection: %w", err)
	}
	return sql.OpenDB(sqldb.InstrumentConnector(connector{base: base}, obs)), nil
}

func fileURI(path string, params url.Values) string {
	slashed := filepath.ToSlash(path)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed // on Windows the URI is file:///C:/dir/app.db
	}
	uri := url.URL{Scheme: "file", Path: slashed, RawQuery: params.Encode()}
	return uri.String()
}
