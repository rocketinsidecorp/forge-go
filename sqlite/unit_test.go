package sqlite

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlite3 "modernc.org/sqlite/lib"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/sqldb"
)

// lockHelperEnv makes the test binary a process that takes the migration
// lock for the database file it names and waits to be killed.
const lockHelperEnv = "FORGE_SQLITE_TEST_LOCK_HOLDER"

func TestMain(m *testing.M) {
	if path := os.Getenv(lockHelperEnv); path != "" {
		holdLockUntilKilled(path)
		return
	}
	os.Exit(m.Run())
}

func holdLockUntilKilled(path string) {
	acquired, err := newMigrationLock(path).tryLock(context.Background(), new(sql.Conn))
	if err != nil || !acquired {
		fmt.Printf("not locked: acquired=%v err=%v\n", acquired, err)
		os.Exit(2)
	}
	fmt.Println("locked")
	time.Sleep(time.Minute) // real-clock: helper process that holds the lock until the test kills it
	os.Exit(3)
}

// driverMessage formats a message the way modernc.org/sqlite does.
func driverMessage(detail string, code int) string {
	return fmt.Sprintf("constraint failed: %s (%d)", detail, code)
}

func TestClassifyResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		code           int
		message        string
		wantKind       sqldb.Kind
		wantConstraint string
		wantFault      string
		wantStatus     int
		wantRetryable  bool
	}{
		{"unique", sqlite3.SQLITE_CONSTRAINT_UNIQUE, driverMessage("UNIQUE constraint failed: tasks.title", 2067),
			sqldb.KindUniqueViolation, "tasks.title", "already_exists", http.StatusConflict, false},
		{"composite unique", sqlite3.SQLITE_CONSTRAINT_UNIQUE, driverMessage("UNIQUE constraint failed: tasks.project_id, tasks.title", 2067),
			sqldb.KindUniqueViolation, "tasks.project_id, tasks.title", "already_exists", http.StatusConflict, false},
		{"primary key", sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY, driverMessage("UNIQUE constraint failed: tasks.id", 1555),
			sqldb.KindUniqueViolation, "tasks.id", "already_exists", http.StatusConflict, false},
		{"foreign key", sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY, driverMessage("FOREIGN KEY constraint failed", 787),
			sqldb.KindForeignKeyViolation, "", "reference_violation", http.StatusConflict, false},
		{"named check", sqlite3.SQLITE_CONSTRAINT_CHECK, driverMessage("CHECK constraint failed: tasks_title_length", 275),
			sqldb.KindInvalidData, "tasks_title_length", "constraint_violation", http.StatusBadRequest, false},
		{"unnamed check keeps its expression", sqlite3.SQLITE_CONSTRAINT_CHECK, driverMessage("CHECK constraint failed: status IN ('open', 'done')", 275),
			sqldb.KindInvalidData, "status IN ('open', 'done')", "constraint_violation", http.StatusBadRequest, false},
		{"strict type mismatch", sqlite3.SQLITE_CONSTRAINT_DATATYPE, driverMessage("cannot store TEXT value in INTEGER column tasks.position", 3091),
			sqldb.KindInvalidData, "tasks.position", "constraint_violation", http.StatusBadRequest, false},
		{"not null is a server bug", sqlite3.SQLITE_CONSTRAINT_NOTNULL, driverMessage("NOT NULL constraint failed: tasks.title", 1299),
			sqldb.KindUnknown, "", "internal_error", http.StatusInternalServerError, false},
		{"other constraint", sqlite3.SQLITE_CONSTRAINT_TRIGGER, driverMessage("raised by trigger", 1811),
			sqldb.KindUnknown, "", "internal_error", http.StatusInternalServerError, false},
		{"busy", sqlite3.SQLITE_BUSY, "database is locked (5) (SQLITE_BUSY)",
			sqldb.KindConflict, "", "transaction_conflict", http.StatusConflict, true},
		{"busy snapshot", sqlite3.SQLITE_BUSY_SNAPSHOT, "database is locked (517)",
			sqldb.KindConflict, "", "transaction_conflict", http.StatusConflict, true},
		{"locked", sqlite3.SQLITE_LOCKED, "database table is locked (6)",
			sqldb.KindConflict, "", "transaction_conflict", http.StatusConflict, true},
		{"locked shared cache", sqlite3.SQLITE_LOCKED_SHAREDCACHE, "database table is locked (262)",
			sqldb.KindConflict, "", "transaction_conflict", http.StatusConflict, true},
		{"interrupted", sqlite3.SQLITE_INTERRUPT, "interrupted (9)",
			sqldb.KindTimeout, "", "timeout", http.StatusServiceUnavailable, true},
		{"disk full", sqlite3.SQLITE_FULL, "database or disk is full (13)",
			sqldb.KindUnavailable, "", "database_unavailable", http.StatusServiceUnavailable, true},
		{"io error", sqlite3.SQLITE_IOERR_WRITE, "disk I/O error (778)",
			sqldb.KindUnavailable, "", "database_unavailable", http.StatusServiceUnavailable, true},
		{"cannot open", sqlite3.SQLITE_CANTOPEN, "unable to open database file (14)",
			sqldb.KindUnavailable, "", "database_unavailable", http.StatusServiceUnavailable, true},
		{"syntax error", sqlite3.SQLITE_ERROR, "SQL logic error: near \"SELEC\": syntax error (1)",
			sqldb.KindUnknown, "", "internal_error", http.StatusInternalServerError, false},
		{"write in a read-only transaction is a server bug", sqlite3.SQLITE_READONLY, "attempt to write a readonly database (8)",
			sqldb.KindUnknown, "", "internal_error", http.StatusInternalServerError, false},
		{"corrupt", sqlite3.SQLITE_CORRUPT, "database disk image is malformed (11)",
			sqldb.KindUnknown, "", "internal_error", http.StatusInternalServerError, false},
	}
	for _, test := range tests {
		got := classifyResult(test.code, test.message)
		if got.Kind != test.wantKind || got.Constraint != test.wantConstraint {
			t.Errorf("%s: got %+v, want kind %d constraint %q", test.name, got, test.wantKind, test.wantConstraint)
		}
		cause := errors.New(test.message)
		translated := fault.From(sqldb.Translate(cause, func(error) sqldb.Classification { return got }))
		if translated == nil {
			t.Fatalf("%s: not translated to a fault", test.name)
		}
		if translated.Code != test.wantFault || translated.Status() != test.wantStatus || translated.Retryable != test.wantRetryable {
			t.Errorf("%s: got code=%s status=%d retryable=%v", test.name, translated.Code, translated.Status(), translated.Retryable)
		}
		if strings.Contains(translated.Message, "constraint failed") || strings.Contains(translated.Message, "tasks") {
			t.Errorf("%s: database message leaked: %s", test.name, translated.Message)
		}
	}
}

func TestClassifyIgnoresOtherErrors(t *testing.T) {
	t.Parallel()
	for _, err := range []error{nil, errors.New("UNIQUE constraint failed: tasks.title (2067)"), sql.ErrConnDone} {
		if got := classify(err); got != (sqldb.Classification{}) {
			t.Errorf("classify(%v) = %+v, want unknown", err, got)
		}
		if ErrorCode(err) != 0 || ConstraintName(err) != "" {
			t.Errorf("%v: not a driver error, but ErrorCode/ConstraintName found one", err)
		}
	}
}

func TestIsCodeSuffix(t *testing.T) {
	t.Parallel()
	for suffix, want := range map[string]bool{
		"2067)": true, "5) (SQLITE_BUSY)": true, ")": false, "x)": false, "1, 2)": false, "2067": false, "2067) extra": false,
	} {
		if got := isCodeSuffix(suffix); got != want {
			t.Errorf("isCodeSuffix(%q) = %v, want %v", suffix, got, want)
		}
	}
}

func TestFileURIEscapesThePath(t *testing.T) {
	t.Parallel()
	params := url.Values{"_txlock": {"immediate"}}
	uri := fileURI("/data/we?ird#name %41&_pragma=foreign_keys(0).db", params)
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "file" || parsed.Path != "/data/we?ird#name %41&_pragma=foreign_keys(0).db" || parsed.Fragment != "" {
		t.Fatalf("path not preserved: %s parsed as %+v", uri, parsed)
	}
	if query := parsed.Query(); len(query) != 1 || query.Get("_txlock") != "immediate" {
		t.Fatalf("the path injected parameters: %s", uri)
	}
}

func TestMigrationLockExcludesOtherHoldersInProcess(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app.db")
	first, second := newMigrationLock(path), newMigrationLock(path)
	ctx := context.Background()
	sessionA, sessionB := new(sql.Conn), new(sql.Conn)

	if acquired, err := first.tryLock(ctx, sessionA); err != nil || !acquired {
		t.Fatalf("first lock: acquired=%v err=%v", acquired, err)
	}
	started := time.Now()
	if acquired, err := second.tryLock(ctx, sessionB); err != nil || acquired {
		t.Fatalf("second lock while held: acquired=%v err=%v", acquired, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("tryLock waited %v instead of failing at once", elapsed)
	}
	if _, err := first.tryLock(ctx, sessionA); err == nil {
		t.Fatal("taking the lock twice for one session must fail")
	}
	if err := second.unlock(sessionB); err == nil {
		t.Fatal("unlocking a lock that is not held must fail")
	}
	if err := first.unlock(sessionA); err != nil {
		t.Fatal(err)
	}
	if acquired, err := second.tryLock(ctx, sessionB); err != nil || !acquired {
		t.Fatalf("lock after release: acquired=%v err=%v", acquired, err)
	}
	if err := second.unlock(sessionB); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path + lockFileSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("lock database grew to %d bytes; it must stay empty", info.Size())
	}
}

// TestMigrationLockIsReleasedWhenHolderCrashes kills a process holding the
// lock: the operating system releases it, so no stale lock can remain.
func TestMigrationLockIsReleasedWhenHolderCrashes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app.db")
	holder := exec.Command(os.Args[0], "-test.run=^$")
	holder.Env = append(os.Environ(), lockHelperEnv+"="+path)
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = holder.Process.Kill()
			_ = holder.Wait()
		}
	})
	line := make(chan string, 1)
	go func() {
		text, _ := bufio.NewReader(stdout).ReadString('\n')
		line <- strings.TrimSpace(text)
	}()
	select {
	case text := <-line:
		if text != "locked" {
			t.Fatalf("holder process: %s", text)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("holder process did not take the lock")
	}

	lock, session := newMigrationLock(path), new(sql.Conn)
	ctx := context.Background()
	if acquired, err := lock.tryLock(ctx, session); err != nil || acquired {
		t.Fatalf("lock taken while another process holds it: acquired=%v err=%v", acquired, err)
	}
	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait() // reports the kill
	killed = true
	if acquired, err := lock.tryLock(ctx, session); err != nil || !acquired {
		t.Fatalf("lock of a killed process was not released: acquired=%v err=%v", acquired, err)
	}
	if err := lock.unlock(session); err != nil {
		t.Fatal(err)
	}
}

func TestBrokenConnectionIsDiscarded(t *testing.T) {
	t.Parallel()
	db, err := connect(filepath.Join(t.TempDir(), "app.db"), url.Values{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	session, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Raw(func(raw any) error {
		wrapped, ok := raw.(*conn)
		if !ok {
			return fmt.Errorf("pool connection is %T, not the Forge wrapper", raw)
		}
		wrapped.broken.Store(true) // as if resetting query_only had failed
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = session.Close()
	if open := db.Stats().OpenConnections; open != 0 {
		t.Fatalf("a connection possibly stuck in query_only went back to the pool (%d open)", open)
	}
}
