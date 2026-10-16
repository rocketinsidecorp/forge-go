package mysql_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/mysql"
	"github.com/rocketinsidecorp/forge-go/mysql/mysqltest"
	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func isMariaDB(t *testing.T, db sqldb.DBTX) bool {
	t.Helper()
	var version string
	if err := db.QueryRowContext(context.Background(), "SELECT VERSION()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return strings.Contains(strings.ToLower(version), "mariadb")
}

func exec(t *testing.T, db sqldb.DBTX, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func faultCode(err error) string {
	if translated := fault.From(mysql.Translate(err)); translated != nil {
		return translated.Code
	}
	return ""
}

func TestSessionIsConfigured(t *testing.T) {
	t.Parallel()
	db := mysqltest.New(t)
	mariaDB := isMariaDB(t, db)
	var timeZone, sqlMode, charset string
	var timeout float64
	query := "SELECT @@session.time_zone, @@session.sql_mode, @@session.character_set_connection, @@session.max_execution_time"
	want := 30000.0
	if mariaDB {
		query = "SELECT @@session.time_zone, @@session.sql_mode, @@session.character_set_connection, @@session.max_statement_time"
		want = 30
	}
	if err := db.QueryRowContext(context.Background(), query).Scan(&timeZone, &sqlMode, &charset, &timeout); err != nil {
		t.Fatal(err)
	}
	if timeZone != "+00:00" || charset != "utf8mb4" || timeout != want {
		t.Fatalf("time_zone=%s charset=%s timeout=%v (want +00:00, utf8mb4, %v)", timeZone, charset, timeout, want)
	}
	for _, mode := range []string{"STRICT_ALL_TABLES", "NO_ZERO_DATE", "NO_ZERO_IN_DATE", "ERROR_FOR_DIVISION_BY_ZERO", "ONLY_FULL_GROUP_BY"} {
		if !strings.Contains(sqlMode, mode) {
			t.Errorf("sql_mode %s lacks %s", sqlMode, mode)
		}
	}
	// Multiple statements are refused on the application pool.
	if _, err := db.ExecContext(context.Background(), "SELECT 1; SELECT 2"); err == nil {
		t.Fatal("the application pool accepted stacked statements")
	}
}

func TestOpenWithWrongPasswordDoesNotLeakIt(t *testing.T) {
	t.Parallel()
	cfg := mysqltest.Config(t)
	parsed, err := url.Parse(cfg.URL.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	parsed.User = url.UserPassword(parsed.User.Username(), "wrong-hunter2")
	cfg.URL = config.NewSecret(parsed.String())
	_, err = mysql.Open(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected an authentication failure")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("authentication error leaked the password: %v", err)
	}
}

func TestTLSParameter(t *testing.T) {
	t.Parallel()
	cfg := mysqltest.Config(t)
	parsed, err := url.Parse(cfg.URL.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	parsed.RawQuery = "tls=skip-verify"
	cfg.URL = config.NewSecret(parsed.String())
	db, err := mysql.Open(context.Background(), cfg)
	if err != nil {
		t.Skipf("test server does not offer TLS: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var name, cipher string
	if err := db.QueryRowContext(context.Background(), "SHOW SESSION STATUS LIKE 'Ssl_cipher'").Scan(&name, &cipher); err != nil {
		t.Fatal(err)
	}
	if cipher == "" {
		t.Fatal("tls=skip-verify connected without TLS")
	}
	// tls=true verifies the certificate, which a test server's self-signed
	// one fails: the connection must be refused, not downgraded.
	parsed.RawQuery = "tls=true"
	cfg.URL = config.NewSecret(parsed.String())
	if verified, err := mysql.Open(context.Background(), cfg); err == nil {
		_ = verified.Close()
		t.Log("the test server's certificate is trusted by this host")
	} else if strings.Contains(err.Error(), parsed.User.String()) {
		t.Fatalf("TLS error leaked credentials: %v", err)
	}
}

func TestUUIDv7RoundTripAndOrdering(t *testing.T) {
	t.Parallel()
	db := mysqltest.New(t)
	ctx := context.Background()
	exec(t, db, "CREATE TABLE items (id CHAR(36) NOT NULL PRIMARY KEY, seq INT NOT NULL)")
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
		t.Fatal("CHAR(36) ordering differs from UUIDv7 generation order")
	}
	var found int
	if err := db.QueryRowContext(ctx, "SELECT seq FROM items WHERE id = ?", generated[7]).Scan(&found); err != nil || found != 7 {
		t.Fatalf("lookup by uuid: seq=%d err=%v", found, err)
	}
}

func TestTimestampsAreUTC(t *testing.T) {
	t.Parallel()
	db := mysqltest.New(t)
	ctx := context.Background()
	exec(t, db, "CREATE TABLE events (id INT PRIMARY KEY, at_datetime DATETIME(6) NOT NULL, at_timestamp TIMESTAMP(6) NOT NULL)")
	saoPaulo := time.FixedZone("BRT", -3*60*60)
	written := time.Date(2026, 9, 26, 12, 0, 0, 123456000, saoPaulo)
	if _, err := db.ExecContext(ctx, "INSERT INTO events VALUES (1, ?, ?)", written, written); err != nil {
		t.Fatal(err)
	}
	var atDatetime, atTimestamp, now, utcNow time.Time
	if err := db.QueryRowContext(ctx, "SELECT at_datetime, at_timestamp, NOW(6), UTC_TIMESTAMP(6) FROM events").Scan(&atDatetime, &atTimestamp, &now, &utcNow); err != nil {
		t.Fatal(err)
	}
	for name, scanned := range map[string]time.Time{"DATETIME": atDatetime, "TIMESTAMP": atTimestamp} {
		if scanned.Location() != time.UTC || scanned.Hour() != 15 || !scanned.Equal(written) {
			t.Errorf("%s scanned %v in %v, want %v", name, scanned, scanned.Location(), written.UTC())
		}
	}
	if now.Location() != time.UTC || now.Sub(utcNow).Abs() > time.Second {
		t.Errorf("session is not in UTC: NOW()=%v UTC_TIMESTAMP()=%v", now, utcNow)
	}
}

func TestStrictModeRejectsBadData(t *testing.T) {
	t.Parallel()
	db := mysqltest.New(t)
	ctx := context.Background()
	exec(t, db, "CREATE TABLE notes (title VARCHAR(5) NOT NULL, priority TINYINT NOT NULL DEFAULT 0, due DATE NULL)")
	tests := []struct {
		name, sql string
		args      []any
	}{
		{"too long", "INSERT INTO notes (title) VALUES (?)", []any{"too long"}},
		{"out of range", "INSERT INTO notes (title, priority) VALUES ('a', ?)", []any{1000}},
		{"invalid date", "INSERT INTO notes (title, due) VALUES ('a', ?)", []any{"2026-02-30"}},
	}
	for _, test := range tests {
		_, err := db.ExecContext(ctx, test.sql, test.args...)
		translated := fault.From(mysql.Translate(err))
		if translated == nil || translated.Code != "constraint_violation" || translated.Status() != http.StatusBadRequest {
			t.Errorf("%s: got %v", test.name, err)
		}
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notes").Scan(&count); err != nil || count != 0 {
		t.Fatalf("bad data was stored: count=%d err=%v", count, err)
	}
}

func TestTranslateAgainstRealConstraints(t *testing.T) {
	t.Parallel()
	db := mysqltest.New(t)
	ctx := context.Background()
	exec(t, db,
		"CREATE TABLE projects (id CHAR(36) PRIMARY KEY)",
		`CREATE TABLE tasks (
			id CHAR(36) PRIMARY KEY,
			project_id CHAR(36) NULL,
			title VARCHAR(100) NOT NULL,
			CONSTRAINT tasks_title_length CHECK (CHAR_LENGTH(title) <= 5),
			CONSTRAINT tasks_title_key UNIQUE (title),
			CONSTRAINT tasks_project_fk FOREIGN KEY (project_id) REFERENCES projects (id)
		)`)
	project := uuid.MustNew()
	if _, err := db.ExecContext(ctx, "INSERT INTO projects (id) VALUES (?)", project); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO tasks (id, project_id, title) VALUES (?, ?, 'a')", uuid.MustNew(), project); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, sql      string
		args           []any
		wantCode       string
		wantConstraint string
	}{
		{"unique", "INSERT INTO tasks (id, title) VALUES (?, 'a')", []any{uuid.MustNew()}, "already_exists", "tasks_title_key"},
		{"foreign key child", "INSERT INTO tasks (id, project_id, title) VALUES (?, ?, 'b')", []any{uuid.MustNew(), uuid.MustNew()}, "reference_violation", "tasks_project_fk"},
		{"foreign key parent", "DELETE FROM projects WHERE id = ?", []any{project}, "reference_violation", "tasks_project_fk"},
		{"check", "INSERT INTO tasks (id, title) VALUES (?, 'too long')", []any{uuid.MustNew()}, "constraint_violation", "tasks_title_length"},
		{"not null", "INSERT INTO tasks (id, title) VALUES (?, NULL)", []any{uuid.MustNew()}, "internal_error", ""},
	}
	for _, test := range cases {
		_, err := db.ExecContext(ctx, test.sql, test.args...)
		translated := fault.From(mysql.Translate(err))
		if translated == nil || translated.Code != test.wantCode {
			t.Errorf("%s: got %v", test.name, err)
			continue
		}
		// MySQL 8 qualifies duplicate keys with the table: tasks.tasks_title_key.
		if constraint := translated.Metadata["constraint"]; !strings.HasSuffix(constraint, test.wantConstraint) || (test.wantConstraint == "") != (constraint == "") {
			t.Errorf("%s: constraint=%q want %q (err %v)", test.name, constraint, test.wantConstraint, err)
		}
	}
	var missing string
	err := db.QueryRowContext(ctx, "SELECT id FROM tasks WHERE title = 'none'").Scan(&missing)
	if translated := fault.From(mysql.Translate(err)); translated == nil || translated.Status() != http.StatusNotFound {
		t.Errorf("no rows: got %v", err)
	}
	// Through the pool's own Translate too.
	_, err = db.ExecContext(ctx, "INSERT INTO tasks (id, title) VALUES (?, 'a')", uuid.MustNew())
	if translated := fault.From(db.Translate(err)); translated == nil || translated.Code != "already_exists" {
		t.Errorf("db.Translate: got %v", err)
	}
}

func TestStatementTimeoutIsApplied(t *testing.T) {
	t.Parallel()
	_, cfg := mysqltest.NewWithConfig(t)
	cfg.StatementTimeout = 50 * time.Millisecond
	db, err := mysql.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	exec(t, db, "CREATE TABLE ticks (id INT PRIMARY KEY)", "INSERT INTO ticks VALUES (1)")
	// MySQL's max_execution_time bounds only SELECT, and a SELECT whose sole
	// work is SLEEP() is cut short without an error, so the sleep runs per row.
	started := time.Now()
	var one int
	err = db.QueryRowContext(ctx, "SELECT 1 FROM ticks WHERE SLEEP(?) = 0", 2).Scan(&one)
	if code := faultCode(err); code != "timeout" {
		t.Fatalf("expected a timeout fault, got %v (number %d)", err, mysql.ErrorNumber(err))
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("statement ran %v despite a 50ms timeout", elapsed)
	}
	// The connection survives the timeout.
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatal(err)
	}
	if isMariaDB(t, db) {
		// MariaDB's max_statement_time bounds writes too.
		_, err := db.ExecContext(ctx, "UPDATE ticks SET id = id WHERE SLEEP(2) = 0")
		if code := faultCode(err); code != "timeout" {
			t.Fatalf("MariaDB write: expected a timeout fault, got %v", err)
		}
	}
}

func TestInTxCommitRollbackAndPanic(t *testing.T) {
	t.Parallel()
	db := mysqltest.New(t)
	ctx := context.Background()
	exec(t, db, "CREATE TABLE counters (name VARCHAR(50) PRIMARY KEY)")
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
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"committed"}) {
		t.Fatalf("rows=%v, want only the committed one", names)
	}
	if inUse := db.SQL().Stats().InUse; inUse != 0 {
		t.Fatalf("%d connections leaked", inUse)
	}
}

// TestDeadlockRetry forces a real deadlock: each transaction locks one row,
// waits for the other, then locks the other's row. InnoDB aborts one with
// 1213 and InTxWith reruns it.
func TestDeadlockRetry(t *testing.T) {
	t.Parallel()
	db := mysqltest.New(t)
	ctx := context.Background()
	exec(t, db, "CREATE TABLE accounts (name VARCHAR(10) PRIMARY KEY, balance INT NOT NULL)",
		"INSERT INTO accounts VALUES ('a', 100), ('b', 100)")
	var barrier sync.WaitGroup
	barrier.Add(2)
	var attempts atomic.Int32
	transfer := func(from, to string) error {
		first := true
		return db.InTxWith(ctx, sqldb.TxOptions{Attempts: 5}, func(tx *sql.Tx) error {
			attempts.Add(1)
			if _, err := tx.ExecContext(ctx, "UPDATE accounts SET balance = balance - 10 WHERE name = ?", from); err != nil {
				return err
			}
			if first {
				first = false
				barrier.Done()
				barrier.Wait() // both hold their first row before either takes the second
			}
			_, err := tx.ExecContext(ctx, "UPDATE accounts SET balance = balance + 10 WHERE name = ?", to)
			return err
		})
	}
	errs := make(chan error, 2)
	go func() { errs <- transfer("a", "b") }()
	go func() { errs <- transfer("b", "a") }()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("retry did not resolve the deadlock: %v", err)
		}
	}
	if attempts.Load() <= 2 {
		t.Fatalf("attempts=%d: expected a deadlock retry", attempts.Load())
	}
	var total, a int
	if err := db.QueryRowContext(ctx, "SELECT SUM(balance), MAX(CASE WHEN name = 'a' THEN balance END) FROM accounts").Scan(&total, &a); err != nil {
		t.Fatal(err)
	}
	if total != 200 || a != 100 {
		t.Fatalf("total=%d a=%d: both transfers must apply exactly once", total, a)
	}
}

func migrationFS() fstest.MapFS {
	return fstest.MapFS{
		"20260101000000_create_accounts.up.sql": {Data: []byte(`
			CREATE TABLE accounts (id CHAR(36) NOT NULL PRIMARY KEY, email VARCHAR(255) NOT NULL);
			CREATE UNIQUE INDEX accounts_email_key ON accounts (email);
			INSERT INTO accounts (id, email) VALUES ('0190a0a0-0000-7000-8000-000000000000', 'seed@example.com');`)},
		"20260101000000_create_accounts.down.sql": {Data: []byte("DROP TABLE accounts;")},
		"20260102000000_add_account_name.up.sql": {Data: []byte(`
			ALTER TABLE accounts ADD COLUMN name VARCHAR(100) NULL;
			CREATE INDEX accounts_name_idx ON accounts (name);`)},
		"20260102000000_add_account_name.down.sql": {Data: []byte(`
			DROP INDEX accounts_name_idx ON accounts;
			ALTER TABLE accounts DROP COLUMN name;`)},
	}
}

func newMigrator(t *testing.T, cfg config.Database, fsys fstest.MapFS) *migrate.Migrator {
	t.Helper()
	migrations, err := migrate.LoadMigrations(fsys)
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := mysql.NewMigrator(cfg, migrations, discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = migrator.Close() })
	return migrator
}

func tableExists(t *testing.T, db sqldb.DBTX, table string) bool {
	t.Helper()
	var count int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count > 0
}

func TestMigratorLifecycle(t *testing.T) {
	t.Parallel()
	db, cfg := mysqltest.NewWithConfig(t)
	ctx := context.Background()
	migrator := newMigrator(t, cfg, migrationFS())

	applied, err := migrator.Up(ctx)
	if err != nil || len(applied) != 2 {
		t.Fatalf("applied=%d err=%v", len(applied), err)
	}
	var email string
	if err := db.QueryRowContext(ctx, "SELECT email FROM accounts").Scan(&email); err != nil || email != "seed@example.com" {
		t.Fatalf("every statement of a multi-statement file must run: email=%q err=%v", email, err)
	}
	if again, err := migrator.Up(ctx); err != nil || len(again) != 0 {
		t.Fatalf("second Up must be a no-op: applied=%d err=%v", len(again), err)
	}
	statuses, err := migrator.Status(ctx)
	if err != nil || len(statuses) != 2 || statuses[0].State != migrate.StateApplied || statuses[1].AppliedAt.IsZero() {
		t.Fatalf("statuses=%+v err=%v", statuses, err)
	}

	reverted, err := migrator.Down(ctx, 2)
	if err != nil || len(reverted) != 2 || reverted[0].Name != "add_account_name" || reverted[1].Name != "create_accounts" {
		t.Fatalf("reverted=%+v err=%v", reverted, err)
	}
	if tableExists(t, db, "accounts") {
		t.Fatal("accounts still exists after rollback")
	}
	statuses, err = migrator.Status(ctx)
	if err != nil || statuses[0].State != migrate.StatePending || statuses[1].State != migrate.StatePending {
		t.Fatalf("statuses after rollback=%+v err=%v", statuses, err)
	}
}

func TestMigratorFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("failed statement is reported and not recorded", func(t *testing.T) {
		t.Parallel()
		db, cfg := mysqltest.NewWithConfig(t)
		broken := fstest.MapFS{"20260101000000_broken.up.sql": {Data: []byte("CREATE TABLE half_done (id int); SELECT * FROM missing_table;")}}
		if _, err := newMigrator(t, cfg, broken).Up(ctx); err == nil {
			t.Fatal("an error in the second statement of a file must fail the migration")
		}
		var recorded int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM forge_schema_migrations").Scan(&recorded); err != nil || recorded != 0 {
			t.Fatalf("failed migration recorded: count=%d err=%v", recorded, err)
		}
		// Documented MySQL behavior: DDL commits implicitly, so the
		// statements before the failure stay applied.
		if !tableExists(t, db, "half_done") {
			t.Fatal("expected the implicitly committed CREATE TABLE to persist")
		}
	})

	t.Run("modified migration", func(t *testing.T) {
		t.Parallel()
		_, cfg := mysqltest.NewWithConfig(t)
		if _, err := newMigrator(t, cfg, migrationFS()).Up(ctx); err != nil {
			t.Fatal(err)
		}
		edited := migrationFS()
		edited["20260101000000_create_accounts.up.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE accounts (id CHAR(36));")}
		if _, err := newMigrator(t, cfg, edited).Up(ctx); err == nil || !strings.Contains(err.Error(), "modified") {
			t.Fatalf("expected modified-migration error, got %v", err)
		}
	})

	t.Run("database ahead of build", func(t *testing.T) {
		t.Parallel()
		_, cfg := mysqltest.NewWithConfig(t)
		if _, err := newMigrator(t, cfg, migrationFS()).Up(ctx); err != nil {
			t.Fatal(err)
		}
		older := migrationFS()
		delete(older, "20260102000000_add_account_name.up.sql")
		delete(older, "20260102000000_add_account_name.down.sql")
		if _, err := newMigrator(t, cfg, older).Up(ctx); err == nil || !strings.Contains(err.Error(), "does not contain") {
			t.Fatalf("expected unknown-migration error, got %v", err)
		}
	})

	t.Run("irreversible rollback reverts nothing", func(t *testing.T) {
		t.Parallel()
		db, cfg := mysqltest.NewWithConfig(t)
		fsys := migrationFS()
		delete(fsys, "20260101000000_create_accounts.down.sql")
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
		var hasName int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'accounts' AND column_name = 'name'").Scan(&hasName); err != nil || hasName != 1 {
			t.Fatalf("the reversible migration was reverted anyway: column count=%d err=%v", hasName, err)
		}
	})
}

func TestRollbackRevertsMostRecentlyAppliedAndStatusIsReadOnly(t *testing.T) {
	t.Parallel()
	db, cfg := mysqltest.NewWithConfig(t)
	ctx := context.Background()
	newer := fstest.MapFS{
		"20260105000000_newer.up.sql":   {Data: []byte("CREATE TABLE newer (id int);")},
		"20260105000000_newer.down.sql": {Data: []byte("DROP TABLE newer;")},
	}
	statuses, err := newMigrator(t, cfg, newer).Status(ctx)
	if err != nil || len(statuses) != 1 || statuses[0].State != migrate.StatePending {
		t.Fatalf("status on a never-migrated database: %+v err=%v", statuses, err)
	}
	if tableExists(t, db, migrate.Table) {
		t.Fatal("Status created the migrations table")
	}
	if _, err := newMigrator(t, cfg, newer).Up(ctx); err != nil {
		t.Fatal(err)
	}
	// An older version from another branch lands later: out of order.
	both := fstest.MapFS{
		"20260101000000_older.up.sql":   {Data: []byte("CREATE TABLE older (id int);")},
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

func TestConcurrentMigratorsApplyOnce(t *testing.T) {
	t.Parallel()
	_, cfg := mysqltest.NewWithConfig(t)
	var total sync.WaitGroup
	results := make(chan int, 5)
	for range 5 {
		total.Go(func() {
			applied, err := newMigrator(t, cfg, migrationFS()).Up(context.Background())
			if err != nil {
				t.Error(err)
			}
			results <- len(applied)
		})
	}
	total.Wait()
	close(results)
	sum := 0
	for count := range results {
		sum += count
	}
	if sum != 2 {
		t.Fatalf("migrations applied %d times across replicas, want exactly 2", sum)
	}
}

// TestMigrationLockIsScopedPerDatabase holds one database's lock and migrates
// another database on the same server: GET_LOCK is server-wide, so an
// unscoped name would make the second migrator wait.
func TestMigrationLockIsScopedPerDatabase(t *testing.T) {
	t.Parallel()
	first, _ := mysqltest.NewWithConfig(t)
	_, second := mysqltest.NewWithConfig(t)
	ctx := context.Background()
	conn, err := first.SQL().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	lock := "CONCAT('forge_schema_migrations:', LEFT(SHA2(DATABASE(), 256), 32))"
	var acquired int
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK("+lock+", 0)").Scan(&acquired); err != nil || acquired != 1 {
		t.Fatalf("take the first database's lock: acquired=%d err=%v", acquired, err)
	}
	upCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := newMigrator(t, second, migrationFS()).Up(upCtx); err != nil {
		t.Fatalf("a lock on another database blocked migration: %v", err)
	}

	// The same database's lock does block, until the context ends.
	_, firstCfg := mysqltest.NewWithConfig(t)
	holder, err := mysql.Open(ctx, firstCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	holderConn, err := holder.SQL().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holderConn.Close() }()
	if err := holderConn.QueryRowContext(ctx, "SELECT GET_LOCK("+lock+", 0)").Scan(&acquired); err != nil || acquired != 1 {
		t.Fatalf("take the lock: acquired=%d err=%v", acquired, err)
	}
	blockedCtx, cancelBlocked := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancelBlocked()
	if _, err := newMigrator(t, firstCfg, migrationFS()).Up(blockedCtx); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the migrator to wait for the held lock, got %v", err)
	}
}

func TestCommandsThroughApplicationBinary(t *testing.T) {
	t.Parallel()
	cfg := mysqltest.Config(t)
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
	commands := mysql.Commands(migrationFS())
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
	if output := run("migrate"); !strings.Contains(output, "applied  20260102000000_add_account_name") {
		t.Fatalf("migrate output: %s", output)
	}
	if output := run("migrate"); !strings.Contains(output, "up to date") {
		t.Fatalf("second migrate output: %s", output)
	}
	if output := run("migrate", "status"); strings.Count(output, "applied") != 2 {
		t.Fatalf("status output: %s", output)
	}
	if output := run("rollback"); !strings.Contains(output, "reverted 20260102000000_add_account_name") {
		t.Fatalf("rollback output: %s", output)
	}
	if output := run("migrate", "status"); !strings.Contains(output, "pending") {
		t.Fatalf("status after rollback: %s", output)
	}
}

func TestNewMigratedAppliesMigrations(t *testing.T) {
	t.Parallel()
	db := mysqltest.NewMigrated(t, migrationFS())
	if !tableExists(t, db, "accounts") || !tableExists(t, db, migrate.Table) {
		t.Fatal("NewMigrated did not apply the migrations")
	}
}

// TestMigrationCannotRedirectBookkeeping is a regression test: a migration
// running USE made the bookkeeping and the lock target another database.
func TestMigrationCannotRedirectBookkeeping(t *testing.T) {
	t.Parallel()
	_, cfg := mysqltest.NewWithConfig(t)
	loaded, err := migrate.LoadMigrations(fstest.MapFS{"20260101000000_switch.up.sql": {Data: []byte("USE mysql; SELECT 1;")}})
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := mysql.NewMigrator(cfg, loaded, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrator.Close() }()
	_, err = migrator.Up(context.Background())
	if err == nil || !strings.Contains(err.Error(), "switched the session") || strings.Contains(err.Error(), "lock was not held") {
		t.Fatalf("expected only the session-switch error, got %v", err)
	}
}
