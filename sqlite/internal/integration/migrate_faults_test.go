package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/migrate"
	_ "modernc.org/sqlite" // the driver the dialect below runs on
)

var errMigrateInjected = errors.New("injected failure")

// logLines records log messages so a test can wait for or count them.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) Enabled(context.Context, slog.Level) bool { return true }
func (l *logLines) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, r.Message)
	return nil
}
func (l *logLines) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logLines) WithGroup(string) slog.Handler      { return l }
func (l *logLines) count(message string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if line == message {
			n++
		}
	}
	return n
}

// scriptedDialect runs the engine on SQLite with every dialect operation
// scriptable. SQLite takes the cross-process lock from an in-process flag.
type scriptedDialect struct {
	prepare, unlock, tableExists func() error
	tryLock                      func() (bool, error)
	locks                        atomic.Int32
}

func (d *scriptedDialect) Placeholder(int) string { return "?" }
func (d *scriptedDialect) PrepareSession(context.Context, *sql.Conn) error {
	if d.prepare != nil {
		return d.prepare()
	}
	return nil
}
func (d *scriptedDialect) TryLock(context.Context, *sql.Conn) (bool, error) {
	d.locks.Add(1)
	if d.tryLock != nil {
		return d.tryLock()
	}
	return true, nil
}
func (d *scriptedDialect) Unlock(context.Context, *sql.Conn) error {
	if d.unlock != nil {
		return d.unlock()
	}
	return nil
}
func (d *scriptedDialect) TableExists(ctx context.Context, conn *sql.Conn, table string) (bool, error) {
	if d.tableExists != nil {
		if err := d.tableExists(); err != nil {
			return false, err
		}
	}
	var n int
	err := conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&n)
	return n > 0, err
}

// identifyingDialect also reports a session identity that the test controls.
type identifyingDialect struct {
	scriptedDialect
	identity func() (string, error)
}

func (d *identifyingDialect) SessionIdentity(context.Context, migrate.Queryer) (string, error) {
	return d.identity()
}

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "migrate.db"))+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newMigrator(t *testing.T, dialect migrate.Dialect, migrations ...migrate.Migration) (*migrate.Migrator, *sql.DB, *logLines) {
	t.Helper()
	db := openSQLite(t)
	logs := &logLines{}
	migrator, err := migrate.New(db, dialect, migrations, slog.New(logs))
	if err != nil {
		t.Fatal(err)
	}
	return migrator, db, logs
}

func migration(version int64, name, up, down string) migrate.Migration {
	return migrate.Migration{
		Version: version, Name: name,
		Up: migrate.Script{SQL: up}, Down: migrate.Script{SQL: down},
		Checksum: strings.Repeat(string(rune('a'+version%26)), 64),
	}
}

func TestNewRequiresItsDependencies(t *testing.T) {
	t.Parallel()
	db := openSQLite(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, call := range map[string]func() (*migrate.Migrator, error){
		"database": func() (*migrate.Migrator, error) { return migrate.New(nil, &scriptedDialect{}, nil, logger) },
		"dialect":  func() (*migrate.Migrator, error) { return migrate.New(db, nil, nil, logger) },
		"logger":   func() (*migrate.Migrator, error) { return migrate.New(db, &scriptedDialect{}, nil, nil) },
	} {
		if m, err := call(); err == nil || m != nil {
			t.Errorf("a missing %s was accepted", name)
		}
	}
}

func TestMigrationSessionFailuresAreReportedAndLeaveNothingApplied(t *testing.T) {
	t.Parallel()
	first := migration(1, "first", "CREATE TABLE first (id INTEGER)", "DROP TABLE first")
	cases := map[string]struct {
		dialect func() migrate.Dialect
		want    string
	}{
		"session setup": {func() migrate.Dialect {
			return &scriptedDialect{prepare: func() error { return errMigrateInjected }}
		}, "configure migration session"},
		"session identity": {func() migrate.Dialect {
			return &identifyingDialect{identity: func() (string, error) { return "", errMigrateInjected }}
		}, "identify migration session"},
		"lock": {func() migrate.Dialect {
			return &scriptedDialect{tryLock: func() (bool, error) { return false, errMigrateInjected }}
		}, "acquire migration lock"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			migrator, db, _ := newMigrator(t, tc.dialect(), first)
			for operation, run := range map[string]func() error{
				"Up":   func() error { _, err := migrator.Up(context.Background()); return err },
				"Down": func() error { _, err := migrator.Down(context.Background(), 1); return err },
			} {
				if err := run(); !errors.Is(err, errMigrateInjected) || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%s = %v, want %q wrapping the failure", operation, err, tc.want)
				}
			}
			var tables int
			if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('first', ?)", migrate.Table).Scan(&tables); err != nil || tables != 0 {
				t.Errorf("a failed session left %d tables behind (err=%v)", tables, err)
			}
		})
	}
}

func TestAMigrationThatCannotReleaseItsLockReportsIt(t *testing.T) {
	t.Parallel()
	migrator, db, _ := newMigrator(t, &scriptedDialect{unlock: func() error { return errMigrateInjected }},
		migration(1, "first", "CREATE TABLE first (id INTEGER)", "DROP TABLE first"))
	applied, err := migrator.Up(context.Background())
	if !errors.Is(err, errMigrateInjected) || !strings.Contains(err.Error(), "release migration lock") {
		t.Fatalf("Up = %v", err)
	}
	if len(applied) != 1 {
		t.Errorf("the migration that did apply is not reported: %v", applied)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM " + migrate.Table).Scan(&n); err != nil || n != 1 {
		t.Errorf("recorded %d migrations (err=%v), want 1", n, err)
	}
}

func TestAMigratorWaitsForTheLockAndStopsWaitingWithItsContext(t *testing.T) {
	t.Parallel()
	first := migration(1, "first", "CREATE TABLE first (id INTEGER)", "DROP TABLE first")

	// The lock frees up after two refusals.
	var refusals atomic.Int32
	dialect := &scriptedDialect{tryLock: func() (bool, error) { return refusals.Add(1) > 2, nil }}
	migrator, _, logs := newMigrator(t, dialect, first)
	applied, err := migrator.Up(context.Background())
	if err != nil || len(applied) != 1 {
		t.Fatalf("Up = %v, %v", applied, err)
	}
	if got := dialect.locks.Load(); got != 3 {
		t.Errorf("TryLock ran %d times, want 3", got)
	}
	if got := logs.count("waiting for another process to finish migrating"); got != 1 {
		t.Errorf("announced the wait %d times, want once", got)
	}

	// The lock never frees up: the caller's deadline ends the wait.
	stuck := &scriptedDialect{tryLock: func() (bool, error) { return false, nil }}
	migrator, _, _ = newMigrator(t, stuck, first)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := migrator.Up(ctx); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "acquire migration lock") {
		t.Errorf("Up = %v, want the deadline while waiting for the lock", err)
	}
}

func TestMigratorsOverAClosedDatabaseReportThatNoSessionOpened(t *testing.T) {
	t.Parallel()
	migrator, _, _ := newMigrator(t, &scriptedDialect{}, migration(1, "first", "SELECT 1", "SELECT 1"))
	if err := migrator.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := migrator.Up(ctx); err == nil || !strings.Contains(err.Error(), "open migration session") {
		t.Errorf("Up = %v", err)
	}
	if _, err := migrator.Status(ctx); err == nil || !strings.Contains(err.Error(), "open migration session") {
		t.Errorf("Status = %v", err)
	}
}

func TestStatusReportsAnUnreadableTrackingTable(t *testing.T) {
	t.Parallel()
	migrator, db, _ := newMigrator(t, &scriptedDialect{tableExists: func() error { return errMigrateInjected }},
		migration(1, "first", "SELECT 1", "SELECT 1"))
	if _, err := migrator.Status(context.Background()); !errors.Is(err, errMigrateInjected) || !strings.Contains(err.Error(), "read applied migrations") {
		t.Errorf("Status with a failing TableExists = %v", err)
	}

	// A tracking table in another layout (a pre-release one) is explained.
	if _, err := db.Exec("CREATE TABLE " + migrate.Table + " (version BIGINT, name TEXT, checksum TEXT, applied_at TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO " + migrate.Table + " VALUES (1, 'first', 'x', 'not a number')"); err != nil {
		t.Fatal(err)
	}
	other, err := migrate.New(db, &scriptedDialect{}, []migrate.Migration{migration(1, "first", "SELECT 1", "SELECT 1")}, slog.New(&logLines{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "does not have the expected layout") {
		t.Errorf("Status over a foreign layout = %v", err)
	}
}

func TestStatusFlagsAMigrationEditedAfterItWasApplied(t *testing.T) {
	t.Parallel()
	original := migration(1, "first", "CREATE TABLE first (id INTEGER)", "DROP TABLE first")
	migrator, db, _ := newMigrator(t, &scriptedDialect{}, original)
	if _, err := migrator.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	edited := original
	edited.Checksum = strings.Repeat("f", 64)
	other, err := migrate.New(db, &scriptedDialect{}, []migrate.Migration{edited}, slog.New(&logLines{}))
	if err != nil {
		t.Fatal(err)
	}
	statuses, err := other.Status(context.Background())
	if err != nil || len(statuses) != 1 || statuses[0].State != migrate.StateModified {
		t.Fatalf("Status = %+v, %v; want the migration modified", statuses, err)
	}
	if _, err := other.Up(context.Background()); err == nil || !strings.Contains(err.Error(), "was modified after it was applied") {
		t.Errorf("Up over an edited migration = %v", err)
	}
}

func TestDownRefusesAnUnknownAppliedMigrationAndOrdersTiesByVersion(t *testing.T) {
	t.Parallel()
	one := migration(1, "one", "CREATE TABLE one (id INTEGER)", "DROP TABLE one")
	two := migration(2, "two", "CREATE TABLE two (id INTEGER)", "DROP TABLE two")
	migrator, db, _ := newMigrator(t, &scriptedDialect{}, one, two)
	ctx := context.Background()
	if _, err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	// Applied in the same instant, the higher version is the newer one.
	if _, err := db.Exec("UPDATE " + migrate.Table + " SET applied_at = 1000"); err != nil {
		t.Fatal(err)
	}
	reverted, err := migrator.Down(ctx, 1)
	if err != nil || len(reverted) != 1 || reverted[0].Version != 2 {
		t.Fatalf("Down = %+v, %v; want version 2 reverted", reverted, err)
	}

	// A build that lacks version 1 must not revert anything.
	older, err := migrate.New(db, &scriptedDialect{}, []migrate.Migration{two}, slog.New(&logLines{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := older.Down(ctx, 1); err == nil || !strings.Contains(err.Error(), "does not contain") {
		t.Errorf("Down with an unknown applied migration = %v", err)
	}
}

func TestAScriptThatSwitchesTheSessionIsRefusedAndRolledBack(t *testing.T) {
	t.Parallel()
	for name, noTx := range map[string]bool{"transactional": false, "no transaction": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			dialect := &identifyingDialect{identity: func() (string, error) {
				// The identity before the script, then after it.
				if calls.Add(1) == 1 {
					return "main", nil
				}
				return "elsewhere", nil
			}}
			m := migration(1, "first", "CREATE TABLE first (id INTEGER)", "DROP TABLE first")
			m.Up.NoTx = noTx
			migrator, db, _ := newMigrator(t, dialect, m)
			_, err := migrator.Up(context.Background())
			if err == nil || !strings.Contains(err.Error(), `switched the session from "main" to "elsewhere"`) {
				t.Fatalf("Up = %v", err)
			}
			var recorded int
			if err := db.QueryRow("SELECT count(*) FROM " + migrate.Table).Scan(&recorded); err != nil || recorded != 0 {
				t.Errorf("recorded %d migrations (err=%v) after a refused script", recorded, err)
			}
			if !noTx {
				var tables int
				if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'first'").Scan(&tables); err != nil || tables != 0 {
					t.Errorf("the refused transactional script was not rolled back (tables=%d err=%v)", tables, err)
				}
			}
		})
	}
}

func TestAScriptThatRecordsItselfMakesTheBookkeepingFailAndRollBack(t *testing.T) {
	t.Parallel()
	script := "CREATE TABLE first (id INTEGER); INSERT INTO " + migrate.Table + " VALUES (1, 'first', '" + strings.Repeat("b", 64) + "', 1)"
	migrator, db, _ := newMigrator(t, &scriptedDialect{}, migration(1, "first", script, "DROP TABLE first"))
	if _, err := migrator.Up(context.Background()); err == nil || !strings.Contains(err.Error(), "apply migration 1_first") {
		t.Fatalf("Up = %v", err)
	}
	var tables int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'first'").Scan(&tables); err != nil || tables != 0 {
		t.Errorf("the script survived a failed record (tables=%d err=%v)", tables, err)
	}
}
