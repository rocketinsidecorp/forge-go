package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func sqliteConfig(path string) config.Database {
	cfg := config.Default().Database
	cfg.URL = config.NewSecret((&url.URL{Scheme: "sqlite", Path: filepath.ToSlash(path)}).String())
	return cfg
}

func openAt(t *testing.T, path string) *sqldb.DB {
	t.Helper()
	db, err := sqlite.Open(context.Background(), sqliteConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenRequiresSQLiteConfiguration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, err := sqlite.Open(ctx, config.Default().Database); err == nil || !strings.Contains(err.Error(), "FORGE_DATABASE_URL") {
		t.Fatalf("expected an error without FORGE_DATABASE_URL, got %v", err)
	}
	cfg := config.Default().Database
	cfg.URL = config.NewSecret("postgres://app:hunter2@127.0.0.1:5432/app")
	_, err := sqlite.Open(ctx, cfg)
	if err == nil || !strings.Contains(err.Error(), "postgres adapter") {
		t.Fatalf("expected a wrong-adapter error, got %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaked the URL: %v", err)
	}
	cfg.URL = config.NewSecret("sqlite::memory:")
	if _, err := sqlite.Open(ctx, cfg); err == nil {
		t.Fatal("an in-memory database must be refused: every pooled connection would get its own")
	}
	if _, err := sqlite.NewMigrator(config.Default().Database, nil, discard); err == nil {
		t.Fatal("NewMigrator must require FORGE_DATABASE_URL")
	}
}

func TestOpenCreatesMissingDirectoriesAndPrivateFiles(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "storage", "nested")
	path := filepath.Join(dir, "app.db")
	db := openAt(t, path)
	if _, err := db.ExecContext(context.Background(), "CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, check := range []struct {
		path string
		max  os.FileMode
	}{{dir, 0o750}, {path, 0o600}, {path + "-wal", 0o600}} {
		info, err := os.Stat(check.path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&^check.max != 0 {
			t.Errorf("%s has permissions %o, want at most %o", check.path, perm, check.max)
		}
	}
}

func TestPathIsNeverParsedAsParameters(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "we?ird#dir %41", "app.db?_pragma=foreign_keys(0)&_txlock=deferred")
	db := openAt(t, path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not created at the literal path: %v", err)
	}
	var foreignKeys int
	if err := db.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("parameters in the file name took effect: foreign_keys=%d err=%v", foreignKeys, err)
	}
}

func TestSettingsApplyToEveryPooledConnection(t *testing.T) {
	t.Parallel()
	db := sqlitetest.New(t)
	ctx := context.Background()
	const connections = 5
	var held []*sql.Conn
	defer func() {
		for _, conn := range held {
			_ = conn.Close()
		}
	}()
	for range connections {
		conn, err := db.SQL().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
	}
	if open := db.SQL().Stats().OpenConnections; open < connections {
		t.Fatalf("only %d physical connections open", open)
	}
	for i, conn := range held {
		var foreignKeys, busyTimeout, synchronous int
		var journalMode string
		for pragma, target := range map[string]any{
			"foreign_keys": &foreignKeys, "busy_timeout": &busyTimeout, "synchronous": &synchronous, "journal_mode": &journalMode,
		} {
			if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(target); err != nil {
				t.Fatalf("PRAGMA %s: %v", pragma, err)
			}
		}
		// synchronous 1 is NORMAL; busy_timeout is FORGE_DATABASE_CONNECT_TIMEOUT.
		if foreignKeys != 1 || busyTimeout != 5000 || synchronous != 1 || journalMode != "wal" {
			t.Errorf("connection %d: foreign_keys=%d busy_timeout=%d synchronous=%d journal_mode=%s",
				i, foreignKeys, busyTimeout, synchronous, journalMode)
		}
		// Double-quoted strings are identifiers only.
		if _, err := conn.ExecContext(ctx, `SELECT "not a column"`); err == nil {
			t.Errorf("connection %d accepts double-quoted string literals", i)
		}
		// Defensive mode: writable_schema cannot be switched on.
		var writableSchema int
		if _, err := conn.ExecContext(ctx, "PRAGMA writable_schema = ON"); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, "PRAGMA writable_schema").Scan(&writableSchema); err != nil || writableSchema != 0 {
			t.Errorf("connection %d: writable_schema=%d err=%v; defensive mode is off", i, writableSchema, err)
		}
	}
}

func TestUUIDv7RoundTripAndOrdering(t *testing.T) {
	t.Parallel()
	db := sqlitetest.New(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE items (id TEXT PRIMARY KEY, seq INTEGER NOT NULL) STRICT"); err != nil {
		t.Fatal(err)
	}
	var generated []uuid.UUID
	for seq := range 50 {
		id := uuid.MustNew()
		generated = append(generated, id)
		if _, err := db.ExecContext(ctx, "INSERT INTO items (id, seq) VALUES (?, ?)", id, seq); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT id FROM items ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var stored []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		stored = append(stored, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stored, generated) {
		t.Fatal("SQLite TEXT ordering differs from UUIDv7 generation order")
	}
	var found uuid.UUID
	if err := db.QueryRowContext(ctx, "SELECT id FROM items WHERE id = ?", generated[7]).Scan(&found); err != nil || found != generated[7] {
		t.Fatalf("lookup by uuid.UUID parameter: %s err=%v", found, err)
	}
}

func TestTimesRoundTripInUTCAndSortChronologically(t *testing.T) {
	t.Parallel()
	db := sqlitetest.New(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE events (id INTEGER PRIMARY KEY, at TIMESTAMP NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	saoPaulo := time.FixedZone("BRT", -3*60*60)
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, saoPaulo)
	// Inserted out of order, mixing whole seconds and fractions of varying
	// length, which must still sort by instant.
	times := []time.Time{
		base.Add(1500 * time.Millisecond),
		base,
		base.Add(time.Second),
		base.Add(1123456789 * time.Nanosecond),
		base.Add(-time.Hour).UTC(),
		time.Now(), // carries a monotonic clock reading
	}
	for _, at := range times {
		if _, err := db.ExecContext(ctx, "INSERT INTO events (at) VALUES (?)", at); err != nil {
			t.Fatal(err)
		}
	}
	var raw string
	if err := db.QueryRowContext(ctx, "SELECT at || '' FROM events WHERE id = 2").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "2026-09-26 15:00:00+00:00" {
		t.Fatalf("stored %q, want UTC text", raw)
	}
	var sqliteView string
	if err := db.QueryRowContext(ctx, "SELECT datetime(at) FROM events WHERE id = 1").Scan(&sqliteView); err != nil || sqliteView != "2026-09-26 15:00:01" {
		t.Fatalf("SQLite date functions read %q (err=%v)", sqliteView, err)
	}

	rows, err := db.QueryContext(ctx, "SELECT at FROM events ORDER BY at")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var scanned []time.Time
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatal(err)
		}
		if at.Location() != time.UTC {
			t.Fatalf("scanned %v in %v, want UTC", at, at.Location())
		}
		scanned = append(scanned, at)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(times)
	slices.SortFunc(want, func(a, b time.Time) int { return a.Compare(b) })
	if len(scanned) != len(want) {
		t.Fatalf("scanned %d rows", len(scanned))
	}
	for i := range want {
		if !scanned[i].Equal(want[i]) {
			t.Fatalf("row %d: got %v, want %v (ORDER BY at must be chronological and lossless)", i, scanned[i], want[i])
		}
	}
}

func TestTranslateAgainstRealErrors(t *testing.T) {
	t.Parallel()
	db := sqlitetest.New(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE projects (id TEXT PRIMARY KEY);
		CREATE TABLE tasks (
			id TEXT PRIMARY KEY,
			project_id TEXT REFERENCES projects (id),
			title TEXT NOT NULL CONSTRAINT tasks_title_length CHECK (length(title) <= 5),
			CONSTRAINT tasks_title_key UNIQUE (title)
		);
		CREATE TABLE positions (task_id TEXT PRIMARY KEY, position INTEGER NOT NULL) STRICT;`); err != nil {
		t.Fatal(err)
	}
	existing := uuid.MustNew()
	if _, err := db.ExecContext(ctx, "INSERT INTO tasks (id, title) VALUES (?, 'a')", existing); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, sql      string
		args           []any
		wantCode       string
		wantStatus     int
		wantSQLite     int
		wantConstraint string
	}{
		{"unique", "INSERT INTO tasks (id, title) VALUES (?, 'a')", []any{uuid.MustNew()}, "already_exists", http.StatusConflict, 2067, "tasks.title"},
		{"primary key", "INSERT INTO tasks (id, title) VALUES (?, 'b')", []any{existing}, "already_exists", http.StatusConflict, 1555, "tasks.id"},
		{"foreign key is enforced", "INSERT INTO tasks (id, project_id, title) VALUES (?, ?, 'b')", []any{uuid.MustNew(), uuid.MustNew()}, "reference_violation", http.StatusConflict, 787, ""},
		{"named check", "INSERT INTO tasks (id, title) VALUES (?, 'too long')", []any{uuid.MustNew()}, "constraint_violation", http.StatusBadRequest, 275, "tasks_title_length"},
		{"not null", "INSERT INTO tasks (id, title) VALUES (?, NULL)", []any{uuid.MustNew()}, "internal_error", http.StatusInternalServerError, 1299, ""},
		{"strict type mismatch", "INSERT INTO positions (task_id, position) VALUES (?, 'first')", []any{existing}, "constraint_violation", http.StatusBadRequest, 3091, "positions.position"},
		{"syntax error", "INSERT INTO", nil, "internal_error", http.StatusInternalServerError, 1, ""},
	}
	for _, test := range cases {
		_, err := db.ExecContext(ctx, test.sql, test.args...)
		if code := sqlite.ErrorCode(err); code != test.wantSQLite {
			t.Errorf("%s: SQLite code %d, want %d (%v)", test.name, code, test.wantSQLite, err)
		}
		translated := fault.From(sqlite.Translate(err))
		if translated == nil || translated.Code != test.wantCode || translated.Status() != test.wantStatus {
			t.Errorf("%s: translated %v from %v", test.name, translated, err)
			continue
		}
		if got := translated.Metadata["constraint"]; got != test.wantConstraint {
			t.Errorf("%s: constraint %q, want %q", test.name, got, test.wantConstraint)
		}
		if !errors.Is(translated, err) {
			t.Errorf("%s: original error lost from the chain", test.name)
		}
		if db.Translate(err).Error() != translated.Error() {
			t.Errorf("%s: DB.Translate and Translate disagree", test.name)
		}
	}
	var missing string
	err := db.QueryRowContext(ctx, "SELECT id FROM tasks WHERE title = 'none'").Scan(&missing)
	if translated := fault.From(sqlite.Translate(err)); translated == nil || translated.Status() != http.StatusNotFound {
		t.Errorf("no rows: got %v", err)
	}
	if sqlite.Translate(nil) != nil {
		t.Error("nil must stay nil")
	}
}

func TestInTxCommitRollbackAndPanic(t *testing.T) {
	t.Parallel()
	db := sqlitetest.New(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE counters (name TEXT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	insert := func(name string) func(*sql.Tx) error {
		return func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO counters (name) VALUES (?)", name)
			return err
		}
	}
	if err := db.InTx(ctx, insert("committed")); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("business rule failed")
	if err := db.InTx(ctx, func(tx *sql.Tx) error { _ = insert("rolled-back")(tx); return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("fn error must be returned unchanged, got %v", err)
	}
	func() {
		defer func() { _ = recover() }()
		_ = db.InTx(ctx, func(tx *sql.Tx) error { _ = insert("panicked")(tx); panic("boom") })
	}()

	rows, err := db.QueryContext(ctx, "SELECT name FROM counters ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	_ = rows.Close()
	if !slices.Equal(names, []string{"committed"}) {
		t.Fatalf("rows=%v, want only the committed one", names)
	}
	if inUse := db.SQL().Stats().InUse; inUse != 0 {
		t.Fatalf("%d connections leaked", inUse)
	}
	// The write lock was released after the rollback and the panic.
	if err := db.InTx(ctx, insert("after")); err != nil {
		t.Fatal(err)
	}
}

// TestConcurrentWritersDoNotFailWithBusy runs read-modify-write transactions
// from many goroutines without retries. Deferred transactions would fail
// with SQLITE_BUSY when two readers try to upgrade to writers; immediate
// transactions queue on the write lock within busy_timeout instead.
func TestConcurrentWritersDoNotFailWithBusy(t *testing.T) {
	t.Parallel()
	db := sqlitetest.New(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE counter (id INTEGER PRIMARY KEY, value INTEGER NOT NULL); INSERT INTO counter VALUES (1, 0)"); err != nil {
		t.Fatal(err)
	}
	const writers, increments = 8, 25
	var group sync.WaitGroup
	errs := make(chan error, writers*increments)
	for range writers {
		group.Go(func() {
			for range increments {
				errs <- db.InTx(ctx, func(tx *sql.Tx) error {
					var value int
					if err := tx.QueryRowContext(ctx, "SELECT value FROM counter WHERE id = 1").Scan(&value); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx, "UPDATE counter SET value = ? WHERE id = 1", value+1)
					return err
				})
			}
		})
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write failed: %v (SQLite code %d)", err, sqlite.ErrorCode(err))
		}
	}
	var value int
	if err := db.QueryRowContext(ctx, "SELECT value FROM counter WHERE id = 1").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != writers*increments {
		t.Fatalf("counter=%d, want %d: updates were lost", value, writers*increments)
	}
}

func TestReadOnlyTransactionsDoNotWaitForTheWriter(t *testing.T) {
	t.Parallel()
	db := sqlitetest.New(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE notes (body TEXT NOT NULL); INSERT INTO notes VALUES ('first')"); err != nil {
		t.Fatal(err)
	}
	writing, release := make(chan struct{}), make(chan struct{})
	writer := make(chan error, 1)
	go func() {
		writer <- db.InTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, "INSERT INTO notes VALUES ('second')"); err != nil {
				return err
			}
			close(writing)
			<-release
			return nil
		})
	}()
	<-writing
	started := time.Now()
	var count int
	err := db.InTxWith(ctx, sqldb.TxOptions{ReadOnly: true}, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT count(*) FROM notes").Scan(&count)
	})
	close(release)
	if err != nil || count != 1 {
		t.Fatalf("read-only transaction: count=%d err=%v, want the committed snapshot", count, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("read-only transaction waited %v for the writer", elapsed)
	}
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
}

func TestContextCancellationInterruptsLongQueries(t *testing.T) {
	t.Parallel()
	db := sqlitetest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	var count int64
	err := db.QueryRowContext(ctx, "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 1000000000000) SELECT count(*) FROM n").Scan(&count)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("query ran %v past its deadline", elapsed)
	}
	if translated := fault.From(sqlite.Translate(err)); translated == nil || translated.Code != "timeout" {
		t.Fatalf("expected a timeout fault, got %v (count=%d)", err, count)
	}
	// The interrupted connection is healthy afterwards.
	if err := db.QueryRowContext(context.Background(), "SELECT 1").Scan(&count); err != nil || count != 1 {
		t.Fatalf("pool unusable after an interrupt: %v", err)
	}
}

func migrationFS() fstest.MapFS {
	return fstest.MapFS{
		"20260101000000_create_accounts.up.sql": {Data: []byte(`
			CREATE TABLE accounts (id TEXT PRIMARY KEY, email TEXT NOT NULL);
			CREATE UNIQUE INDEX accounts_email_key ON accounts (lower(email));
			INSERT INTO accounts (id, email) VALUES ('seed', 'seed@example.com');`)},
		"20260101000000_create_accounts.down.sql": {Data: []byte("DROP TABLE accounts;")},
		"20260102000000_create_sessions.up.sql": {Data: []byte(`
			CREATE TABLE sessions (
				id TEXT PRIMARY KEY,
				account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE CASCADE
			);
			CREATE INDEX sessions_account_id_idx ON sessions (account_id);`)},
		"20260102000000_create_sessions.down.sql": {Data: []byte("DROP INDEX sessions_account_id_idx;\nDROP TABLE sessions;")},
	}
}

func newMigrator(t *testing.T, cfg config.Database, fsys fstest.MapFS) *migrate.Migrator {
	t.Helper()
	migrations, err := migrate.LoadMigrations(fsys)
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := sqlite.NewMigrator(cfg, migrations, discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = migrator.Close() })
	return migrator
}

func schemaObjects(t *testing.T, db *sqldb.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), "SELECT name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func TestMigratorLifecycle(t *testing.T) {
	t.Parallel()
	db, cfg := sqlitetest.NewWithConfig(t)
	ctx := context.Background()
	migrator := newMigrator(t, cfg, migrationFS())

	applied, err := migrator.Up(ctx)
	if err != nil || len(applied) != 2 {
		t.Fatalf("applied=%d err=%v", len(applied), err)
	}
	// Every statement of the multi-statement files ran.
	want := []string{"accounts", "accounts_email_key", migrate.Table, "sessions", "sessions_account_id_idx"}
	if got := schemaObjects(t, db); !slices.Equal(got, want) {
		t.Fatalf("schema=%v, want %v", got, want)
	}
	var seeded int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM accounts").Scan(&seeded); err != nil || seeded != 1 {
		t.Fatalf("seed row: %d err=%v", seeded, err)
	}
	if again, err := migrator.Up(ctx); err != nil || len(again) != 0 {
		t.Fatalf("second Up must be a no-op: applied=%d err=%v", len(again), err)
	}
	statuses, err := migrator.Status(ctx)
	if err != nil || len(statuses) != 2 || statuses[0].State != migrate.StateApplied || statuses[1].AppliedAt.IsZero() {
		t.Fatalf("statuses=%+v err=%v", statuses, err)
	}

	reverted, err := migrator.Down(ctx, 2)
	if err != nil || len(reverted) != 2 || reverted[0].Name != "create_sessions" || reverted[1].Name != "create_accounts" {
		t.Fatalf("reverted=%+v err=%v", reverted, err)
	}
	if got := schemaObjects(t, db); !slices.Equal(got, []string{migrate.Table}) {
		t.Fatalf("schema after rollback: %v", got)
	}
}

func TestMigratorFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("failed migration leaves no trace", func(t *testing.T) {
		t.Parallel()
		db, cfg := sqlitetest.NewWithConfig(t)
		broken := migrationFS()
		broken["20260103000000_broken.up.sql"] = &fstest.MapFile{Data: []byte(`
			CREATE TABLE half_done (id INTEGER);
			INSERT INTO half_done VALUES (1);
			ALTER TABLE accounts ADD COLUMN name TEXT;
			SELECT * FROM missing_table;`)}
		applied, err := newMigrator(t, cfg, broken).Up(ctx)
		if err == nil || !strings.Contains(err.Error(), "20260103000000_broken") {
			t.Fatalf("expected the broken migration to fail, got %v", err)
		}
		if len(applied) != 2 {
			t.Fatalf("the migrations before the broken one must stay applied: %d", len(applied))
		}
		var halfDone, recorded, nameColumn int
		if err := db.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM sqlite_master WHERE name = 'half_done'),
			(SELECT count(*) FROM forge_schema_migrations WHERE version = 20260103000000),
			(SELECT count(*) FROM pragma_table_info('accounts') WHERE name = 'name')`).Scan(&halfDone, &recorded, &nameColumn); err != nil {
			t.Fatal(err)
		}
		if halfDone != 0 || recorded != 0 || nameColumn != 0 {
			t.Fatalf("partial migration persisted: table=%d recorded=%d column=%d", halfDone, recorded, nameColumn)
		}
	})

	t.Run("modified migration", func(t *testing.T) {
		t.Parallel()
		_, cfg := sqlitetest.NewWithConfig(t)
		if _, err := newMigrator(t, cfg, migrationFS()).Up(ctx); err != nil {
			t.Fatal(err)
		}
		edited := migrationFS()
		edited["20260101000000_create_accounts.up.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE accounts (id TEXT);")}
		if _, err := newMigrator(t, cfg, edited).Up(ctx); err == nil || !strings.Contains(err.Error(), "modified") {
			t.Fatalf("expected modified-migration error, got %v", err)
		}
	})

	t.Run("database ahead of build", func(t *testing.T) {
		t.Parallel()
		_, cfg := sqlitetest.NewWithConfig(t)
		if _, err := newMigrator(t, cfg, migrationFS()).Up(ctx); err != nil {
			t.Fatal(err)
		}
		older := migrationFS()
		delete(older, "20260102000000_create_sessions.up.sql")
		delete(older, "20260102000000_create_sessions.down.sql")
		if _, err := newMigrator(t, cfg, older).Up(ctx); err == nil || !strings.Contains(err.Error(), "does not contain") {
			t.Fatalf("expected unknown-migration error, got %v", err)
		}
	})

	t.Run("irreversible rollback reverts nothing", func(t *testing.T) {
		t.Parallel()
		_, cfg := sqlitetest.NewWithConfig(t)
		fsys := migrationFS()
		delete(fsys, "20260102000000_create_sessions.down.sql")
		migrator := newMigrator(t, cfg, fsys)
		if _, err := migrator.Up(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := migrator.Down(ctx, 2); err == nil || !strings.Contains(err.Error(), "irreversible") {
			t.Fatalf("expected irreversible error, got %v", err)
		}
		statuses, err := migrator.Status(ctx)
		if err != nil || statuses[0].State != migrate.StateApplied || statuses[1].State != migrate.StateApplied {
			t.Fatalf("rollback was partially applied: %+v err=%v", statuses, err)
		}
	})
}

// TestTableRebuildOutsideTransaction follows the documented recipe for
// changing a referenced table, and checks that a failed rebuild rolls back.
func TestTableRebuildOutsideTransaction(t *testing.T) {
	t.Parallel()
	db, cfg := sqlitetest.NewWithConfig(t)
	ctx := context.Background()
	if _, err := newMigrator(t, cfg, migrationFS()).Up(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO sessions (id, account_id) VALUES ('s1', 'seed')"); err != nil {
		t.Fatal(err)
	}
	rebuild := func(body string) fstest.MapFS {
		fsys := migrationFS()
		fsys["20260103000000_rebuild_accounts.up.sql"] = &fstest.MapFile{Data: []byte("-- forge:no-transaction\n" + `
			PRAGMA foreign_keys = OFF;
			BEGIN;
			CREATE TABLE accounts_new (id TEXT PRIMARY KEY, email TEXT NOT NULL, name TEXT NOT NULL DEFAULT '');
			INSERT INTO accounts_new (id, email) SELECT id, email FROM accounts;
			DROP TABLE accounts;
			ALTER TABLE accounts_new RENAME TO accounts;
			CREATE UNIQUE INDEX accounts_email_key ON accounts (lower(email));
			` + body + `
			COMMIT;
			PRAGMA foreign_keys = ON;`)}
		return fsys
	}

	if _, err := newMigrator(t, cfg, rebuild("SELECT * FROM missing_table;")).Up(ctx); err == nil {
		t.Fatal("expected the failing rebuild to fail")
	}
	var nameColumn int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('accounts') WHERE name = 'name'").Scan(&nameColumn); err != nil || nameColumn != 0 {
		t.Fatalf("failed rebuild persisted: name column=%d err=%v", nameColumn, err)
	}

	if _, err := newMigrator(t, cfg, rebuild("")).Up(ctx); err != nil {
		t.Fatal(err)
	}
	var violations, sessions int
	if err := db.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM pragma_foreign_key_check), (SELECT count(*) FROM sessions JOIN accounts ON accounts.id = sessions.account_id)").
		Scan(&violations, &sessions); err != nil {
		t.Fatal(err)
	}
	if violations != 0 || sessions != 1 {
		t.Fatalf("rebuild broke references: violations=%d joined sessions=%d", violations, sessions)
	}
	_, err := db.ExecContext(ctx, "INSERT INTO sessions (id, account_id) VALUES ('s2', 'nobody')")
	if translated := fault.From(sqlite.Translate(err)); translated == nil || translated.Code != "reference_violation" {
		t.Fatalf("foreign keys not enforced after the rebuild: %v", err)
	}
}

func TestConcurrentMigratorsApplyOnce(t *testing.T) {
	t.Parallel()
	_, cfg := sqlitetest.NewWithConfig(t)
	var group sync.WaitGroup
	results := make(chan int, 5)
	for range 5 {
		group.Go(func() {
			applied, err := newMigrator(t, cfg, migrationFS()).Up(context.Background())
			if err != nil {
				t.Error(err)
			}
			results <- len(applied)
		})
	}
	group.Wait()
	close(results)
	sum := 0
	for count := range results {
		sum += count
	}
	if sum != 2 {
		t.Fatalf("migrations applied %d times across migrators, want exactly 2", sum)
	}
}

func TestMigratorWaitsForLockHolder(t *testing.T) {
	t.Parallel()
	_, cfg := sqlitetest.NewWithConfig(t)
	// A migration that takes a while holds the lock; a second migrator
	// polls until the first finishes, then finds nothing to do.
	slow := migrationFS()
	slow["20260103000000_slow.up.sql"] = &fstest.MapFile{Data: []byte(
		"CREATE TABLE slow AS WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 50000) SELECT i FROM n;")}
	first, second := newMigrator(t, cfg, slow), newMigrator(t, cfg, slow)
	done := make(chan []migrate.Migration, 2)
	var group sync.WaitGroup
	for _, migrator := range []*migrate.Migrator{first, second} {
		group.Go(func() {
			applied, err := migrator.Up(context.Background())
			if err != nil {
				t.Error(err)
			}
			done <- applied
		})
	}
	group.Wait()
	close(done)
	total := 0
	for applied := range done {
		total += len(applied)
	}
	if total != 3 {
		t.Fatalf("applied %d migrations in total, want 3", total)
	}
}

func TestStatusIsReadOnlyAndRollbackRevertsMostRecentlyApplied(t *testing.T) {
	t.Parallel()
	db, cfg := sqlitetest.NewWithConfig(t)
	ctx := context.Background()
	newer := fstest.MapFS{
		"20260105000000_newer.up.sql":   {Data: []byte("CREATE TABLE newer (id INTEGER);")},
		"20260105000000_newer.down.sql": {Data: []byte("DROP TABLE newer;")},
	}
	statuses, err := newMigrator(t, cfg, newer).Status(ctx)
	if err != nil || len(statuses) != 1 || statuses[0].State != migrate.StatePending {
		t.Fatalf("status on a never-migrated database: %+v err=%v", statuses, err)
	}
	if objects := schemaObjects(t, db); len(objects) != 0 {
		t.Fatalf("Status changed the schema: %v", objects)
	}
	if _, err := newMigrator(t, cfg, newer).Up(ctx); err != nil {
		t.Fatal(err)
	}
	// An older version from another branch lands later: out of order.
	both := fstest.MapFS{
		"20260101000000_older.up.sql":   {Data: []byte("CREATE TABLE older (id INTEGER);")},
		"20260101000000_older.down.sql": {Data: []byte("DROP TABLE older;")},
	}
	for name, file := range newer {
		both[name] = file
	}
	migrator := newMigrator(t, cfg, both)
	if _, err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	reverted, err := migrator.Down(ctx, 1)
	if err != nil || len(reverted) != 1 || reverted[0].Name != "older" {
		t.Fatalf("rollback must revert the most recently applied migration (older), got %+v err=%v", reverted, err)
	}
}

func TestCommandsThroughApplicationBinary(t *testing.T) {
	t.Parallel()
	cfg := sqlitetest.Config(t)
	lookup := func(key string) (string, bool) {
		switch key {
		case "FORGE_ENV":
			return "test", true
		case "FORGE_DATABASE_URL":
			return cfg.URL.Reveal(), true
		}
		return "", false
	}
	noop := func(context.Context, *forge.App) error { return nil }
	commands := sqlite.Commands(migrationFS())
	run := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := forge.Execute(context.Background(), args, lookup, &stdout, &stderr, noop, commands...); code != 0 {
			t.Fatalf("%v: code=%d stderr=%s", args, code, stderr.String())
		}
		return stdout.String()
	}
	if output := run("migrate", "status"); strings.Count(output, "pending") != 2 {
		t.Fatalf("status before migrating: %s", output)
	}
	if output := run("migrate"); !strings.Contains(output, "applied  20260102000000_create_sessions") {
		t.Fatalf("migrate output: %s", output)
	}
	if output := run("migrate"); !strings.Contains(output, "up to date") {
		t.Fatalf("second migrate output: %s", output)
	}
	if output := run("migrate", "status"); strings.Count(output, "applied") != 2 {
		t.Fatalf("status output: %s", output)
	}
	if output := run("rollback"); !strings.Contains(output, "reverted 20260102000000_create_sessions") {
		t.Fatalf("rollback output: %s", output)
	}
	if output := run("migrate", "status"); !strings.Contains(output, "pending   20260102000000_create_sessions") {
		t.Fatalf("status after rollback: %s", output)
	}
}

func TestNewMigratedAppliesMigrations(t *testing.T) {
	t.Parallel()
	db := sqlitetest.NewMigrated(t, migrationFS())
	if got := schemaObjects(t, db); !slices.Contains(got, "sessions") {
		t.Fatalf("schema=%v", got)
	}
}

func TestReadOnlyTransactionsRejectWritesAndResetTheConnection(t *testing.T) {
	t.Parallel()
	db := sqlitetest.New(t)
	db.SQL().SetMaxOpenConns(1) // every step below reuses one connection
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE notes (body TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	readOnly := sqldb.TxOptions{ReadOnly: true}
	write := func(body string) error {
		_, err := db.ExecContext(ctx, "INSERT INTO notes VALUES (?)", body)
		return err
	}

	// A write rolls the read-only transaction back.
	err := db.InTxWith(ctx, readOnly, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO notes VALUES ('sneaky')")
		return err
	})
	if code := sqlite.ErrorCode(err); code&0xff != 8 { // SQLITE_READONLY
		t.Fatalf("write in a read-only transaction: SQLite code %d, err %v", code, err)
	}
	if translated := fault.From(sqlite.Translate(err)); translated == nil || translated.Code != "internal_error" {
		t.Fatalf("read-only violation translated to %v", translated)
	}
	if err := write("after rollback"); err != nil {
		t.Fatalf("connection stuck read-only after rollback: %v", err)
	}

	// A committed read-only transaction also resets the connection.
	var count int
	if err := db.InTxWith(ctx, readOnly, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT count(*) FROM notes").Scan(&count)
	}); err != nil || count != 1 {
		t.Fatalf("read-only read: count=%d err=%v", count, err)
	}
	if err := write("after commit"); err != nil {
		t.Fatalf("connection stuck read-only after commit: %v", err)
	}

	// A panic inside a read-only transaction too.
	func() {
		defer func() { _ = recover() }()
		_ = db.InTxWith(ctx, readOnly, func(*sql.Tx) error { panic("boom") })
	}()
	if err := write("after panic"); err != nil {
		t.Fatalf("connection stuck read-only after a panic: %v", err)
	}
	var queryOnly int
	if err := db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 0 {
		t.Fatalf("query_only=%d err=%v", queryOnly, err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM notes").Scan(&count); err != nil || count != 3 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestIsolationLevels(t *testing.T) {
	t.Parallel()
	db := sqlitetest.New(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE notes (body TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	insert := func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO notes VALUES ('x')")
		return err
	}
	for _, level := range []sql.IsolationLevel{sql.LevelDefault, sql.LevelSerializable} {
		if err := db.InTxWith(ctx, sqldb.TxOptions{Isolation: level}, insert); err != nil {
			t.Errorf("%s: %v", level, err)
		}
	}
	for _, level := range []sql.IsolationLevel{
		sql.LevelReadUncommitted, sql.LevelReadCommitted, sql.LevelWriteCommitted,
		sql.LevelRepeatableRead, sql.LevelSnapshot, sql.LevelLinearizable,
	} {
		ran := false
		err := db.InTxWith(ctx, sqldb.TxOptions{Isolation: level}, func(*sql.Tx) error { ran = true; return nil })
		if !errors.Is(err, sqlite.ErrIsolationLevel) || ran {
			t.Errorf("%s: want ErrIsolationLevel before running, got ran=%v err=%v", level, ran, err)
		}
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM notes").Scan(&count); err != nil || count != 2 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

// TestMigrationLockFollowsSymlinks migrates one database through two
// spellings of its path: they must share one lock.
func TestMigrationLockFollowsSymlinks(t *testing.T) {
	t.Parallel()
	_, cfg := sqlitetest.NewWithConfig(t)
	link := filepath.Join(filepath.Dir(cfg.SQLitePath()), "current.db")
	if err := os.Symlink(cfg.SQLitePath(), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	slow := migrationFS()
	slow["20260103000000_slow.up.sql"] = &fstest.MapFile{Data: []byte(
		"CREATE TABLE slow AS WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 50000) SELECT i FROM n;")}
	results := make(chan int, 4)
	var group sync.WaitGroup
	for _, target := range []config.Database{cfg, sqliteConfig(link), cfg, sqliteConfig(link)} {
		group.Go(func() {
			applied, err := newMigrator(t, target, slow).Up(context.Background())
			if err != nil {
				t.Error(err)
			}
			results <- len(applied)
		})
	}
	group.Wait()
	close(results)
	total := 0
	for count := range results {
		total += count
	}
	if total != 3 {
		t.Fatalf("applied %d migrations through two paths, want 3", total)
	}
}
