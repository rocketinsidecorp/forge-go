package mysql_test

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/mysql"
)

func mysqlError(number uint16, message string) error {
	return fmt.Errorf("insert task: %w", &driver.MySQLError{Number: number, Message: message})
}

func TestTranslate(t *testing.T) {
	t.Parallel()
	existing := fault.New("task_not_found", "task not found", fault.CategoryNotFound, 0)
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	tests := []struct {
		name           string
		err            error
		wantCode       string
		wantStatus     int
		wantRetryable  bool
		wantConstraint string
	}{
		{"no rows", fmt.Errorf("get: %w", sql.ErrNoRows), "not_found", http.StatusNotFound, false, ""},
		{"duplicate mysql 8", mysqlError(1062, "Duplicate entry 'secret' for key 'tasks.tasks_title_key'"), "already_exists", http.StatusConflict, false, "tasks.tasks_title_key"},
		{"duplicate mariadb", mysqlError(1062, "Duplicate entry 'secret' for key 'tasks_title_key'"), "already_exists", http.StatusConflict, false, "tasks_title_key"},
		{"child row", mysqlError(1452, "Cannot add or update a child row: a foreign key constraint fails (`app`.`tasks`, CONSTRAINT `tasks_project_fk` FOREIGN KEY (`project_id`) REFERENCES `projects` (`id`))"), "reference_violation", http.StatusConflict, false, "tasks_project_fk"},
		{"parent row", mysqlError(1451, "Cannot delete or update a parent row: a foreign key constraint fails (`app`.`tasks`, CONSTRAINT `tasks_project_fk` FOREIGN KEY (`project_id`) REFERENCES `projects` (`id`))"), "reference_violation", http.StatusConflict, false, "tasks_project_fk"},
		{"check mysql", mysqlError(3819, "Check constraint 'tasks_title_length' is violated."), "constraint_violation", http.StatusBadRequest, false, "tasks_title_length"},
		{"check mariadb", mysqlError(4025, "CONSTRAINT `tasks_title_length` failed for `app`.`tasks`"), "constraint_violation", http.StatusBadRequest, false, "tasks_title_length"},
		{"data too long", mysqlError(1406, "Data too long for column 'title' at row 1"), "constraint_violation", http.StatusBadRequest, false, ""},
		{"out of range", mysqlError(1264, "Out of range value for column 'priority' at row 1"), "constraint_violation", http.StatusBadRequest, false, ""},
		{"incorrect datetime", mysqlError(1292, "Incorrect datetime value: 'secret' for column 'due_at' at row 1"), "constraint_violation", http.StatusBadRequest, false, ""},
		{"not null is a server bug", mysqlError(1048, "Column 'title' cannot be null"), "internal_error", http.StatusInternalServerError, false, ""},
		{"deadlock", mysqlError(1213, "Deadlock found when trying to get lock; try restarting transaction"), "transaction_conflict", http.StatusConflict, true, ""},
		{"lock wait timeout", mysqlError(1205, "Lock wait timeout exceeded; try restarting transaction"), "transaction_conflict", http.StatusConflict, true, ""},
		{"mysql statement timeout", mysqlError(3024, "Query execution was interrupted, maximum statement execution time exceeded"), "timeout", http.StatusServiceUnavailable, true, ""},
		{"mariadb statement timeout", mysqlError(1969, "Query execution was interrupted (max_statement_time exceeded)"), "timeout", http.StatusServiceUnavailable, true, ""},
		{"too many connections", mysqlError(1040, "Too many connections"), "database_unavailable", http.StatusServiceUnavailable, true, ""},
		{"server shutdown", mysqlError(1053, "Server shutdown in progress"), "database_unavailable", http.StatusServiceUnavailable, true, ""},
		{"invalid connection", fmt.Errorf("query: %w", driver.ErrInvalidConn), "database_unavailable", http.StatusServiceUnavailable, true, ""},
		{"bad connection", fmt.Errorf("query: %w", sqldriver.ErrBadConn), "database_unavailable", http.StatusServiceUnavailable, true, ""},
		{"dial error", fmt.Errorf("connect: %w", dialErr), "database_unavailable", http.StatusServiceUnavailable, true, ""},
		{"deadline", context.DeadlineExceeded, "timeout", http.StatusServiceUnavailable, true, ""},
		{"syntax error", mysqlError(1064, "You have an error in your SQL syntax near 'secret'"), "internal_error", http.StatusInternalServerError, false, ""},
		{"unknown", errors.New("boom"), "internal_error", http.StatusInternalServerError, false, ""},
		{"already a fault", existing, "task_not_found", http.StatusNotFound, false, ""},
	}
	for _, test := range tests {
		translated := fault.From(mysql.Translate(test.err))
		if translated == nil {
			t.Fatalf("%s: not translated to a fault", test.name)
		}
		if translated.Code != test.wantCode || translated.Status() != test.wantStatus || translated.Retryable != test.wantRetryable {
			t.Errorf("%s: got code=%s status=%d retryable=%v", test.name, translated.Code, translated.Status(), translated.Retryable)
		}
		if translated.Metadata["constraint"] != test.wantConstraint {
			t.Errorf("%s: constraint=%q want %q", test.name, translated.Metadata["constraint"], test.wantConstraint)
		}
		if strings.Contains(translated.Message, "secret") {
			t.Errorf("%s: database message leaked: %s", test.name, translated.Message)
		}
		if test.name != "already a fault" && !errors.Is(translated, test.err) && !errors.Is(translated.Unwrap(), test.err) {
			t.Errorf("%s: original error lost from the chain", test.name)
		}
	}
	if mysql.Translate(nil) != nil {
		t.Error("nil must stay nil")
	}
}

func TestConstraintNameResistsHostileValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"value contains the marker", mysqlError(1062, "Duplicate entry 'x' for key 'fake' for key 'tasks.tasks_title_key'"), "tasks.tasks_title_key"},
		{"truncated message", mysqlError(1062, "Duplicate entry 'x' for key '"), ""},
		{"no key", mysqlError(1062, "Duplicate entry 'x'"), ""},
		{"fk without constraint", mysqlError(1452, "Cannot add or update a child row"), ""},
		{"unterminated constraint", mysqlError(4025, "CONSTRAINT `half"), ""},
		{"other error", mysqlError(1048, "Column 'title' cannot be null"), ""},
		{"not a mysql error", errors.New("CONSTRAINT `x`"), ""},
	}
	for _, test := range tests {
		if got := mysql.ConstraintName(test.err); got != test.want {
			t.Errorf("%s: got %q want %q", test.name, got, test.want)
		}
	}
}

func TestErrorNumber(t *testing.T) {
	t.Parallel()
	if got := mysql.ErrorNumber(mysqlError(1062, "dup")); got != 1062 {
		t.Fatalf("ErrorNumber=%d want 1062", got)
	}
	if got := mysql.ErrorNumber(errors.New("plain")); got != 0 {
		t.Fatalf("ErrorNumber=%d want 0", got)
	}
}

func TestOpenRejectsConfiguration(t *testing.T) {
	t.Parallel()
	const password = "hunter2"
	tests := []struct {
		name, url, wantInError string
	}{
		{"no url", "", "not configured"},
		{"postgres url", "postgres://app:" + password + "@127.0.0.1:5432/app", "selects the postgres adapter"},
		{"sqlite url", "sqlite:storage/app.db", "selects the sqlite adapter"},
		{"unsupported scheme", "http://app:" + password + "@127.0.0.1/app", "mysql://"},
		{"malformed url", "mysql://app:" + password + "@[::1/app", ""},
		{"no database", "mysql://app:" + password + "@127.0.0.1:3306/", "mysql://"},
		{"nested path", "mysql://app:" + password + "@127.0.0.1:3306/app/extra", "mysql://"},
		{"no user", "mysql://127.0.0.1:3306/app", "mysql://"},
		{"no host", "mysql://app:" + password + "@/app", "mysql://"},
		{"bad port", "mysql://app:" + password + "@127.0.0.1:99999/app", "mysql://"},
		{"unescaped hash in password", "mysql://app:hunt#er2@127.0.0.1/app", "percent-encode"},
		{"parseTime", "mysql://app:" + password + "@127.0.0.1/app?parseTime=false", `"parseTime" is not supported`},
		{"multiStatements", "mysql://app:" + password + "@127.0.0.1/app?multiStatements=true", `"multiStatements" is not supported`},
		{"allowAllFiles", "mysql://app:" + password + "@127.0.0.1/app?tls=true&allowAllFiles=true", `"allowAllFiles" is not supported`},
		{"session variable", "mysql://app:" + password + "@127.0.0.1/app?sql_mode=%27%27", `"sql_mode" is not supported`},
		{"password parameter", "mysql://app@127.0.0.1/app?password=" + password, `"password" is not supported`},
		{"bad tls value", "mysql://app:" + password + "@127.0.0.1/app?tls=custom", "tls must be one of"},
		{"repeated tls", "mysql://app:" + password + "@127.0.0.1/app?tls=true&tls=false", "tls must be one of"},
		{"bad escape", "mysql://app:" + password + "@127.0.0.1/app?tls=%zz", "mysql://"},
	}
	for _, test := range tests {
		cfg := config.Default().Database
		cfg.URL = config.NewSecret(test.url)
		db, err := mysql.Open(context.Background(), cfg)
		if err == nil {
			_ = db.Close()
			t.Errorf("%s: expected an error", test.name)
			continue
		}
		if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), "hunt#er2") {
			t.Errorf("%s: error leaked the password: %v", test.name, err)
		}
		if !strings.Contains(err.Error(), test.wantInError) {
			t.Errorf("%s: error %q does not mention %q", test.name, err, test.wantInError)
		}
		if _, err := mysql.NewMigrator(cfg, nil, discard); err == nil {
			t.Errorf("%s: NewMigrator accepted the configuration", test.name)
		}
	}
}

func TestOpenUnreachableHostFailsFast(t *testing.T) {
	t.Parallel()
	for _, rawURL := range []string{
		"mysql://app:hunter2@127.0.0.1:1/app",       // refused
		"mysql://app:hunter2@192.0.2.1:3306/app",    // TEST-NET-1: never answers
		"mysql://app:hunter2@[::1]:1/app?tls=false", // IPv6 literal
	} {
		cfg := config.Default().Database
		cfg.URL = config.NewSecret(rawURL)
		cfg.ConnectTimeout = 300 * time.Millisecond
		started := time.Now()
		_, err := mysql.Open(context.Background(), cfg)
		if err == nil {
			t.Fatalf("%s: expected a connection failure", cfg.URL)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("connection error leaked the password: %v", err)
		}
		if elapsed := time.Since(started); elapsed > 3*time.Second {
			t.Fatalf("Open ignored the connect timeout: took %v", elapsed)
		}
		if translated := fault.From(mysql.Translate(err)); translated == nil || (translated.Code != "database_unavailable" && translated.Code != "timeout") {
			t.Errorf("connection failure translated to %v", translated)
		}
	}
}

// A lazy pool returns at once on an unreachable server; its first query
// reports the failure.
func TestLazyOpenConnectsOnFirstQuery(t *testing.T) {
	t.Parallel()
	cfg := config.Default().Database
	cfg.URL = config.NewSecret("mysql://app:hunter2@127.0.0.1:1/app")
	cfg.ConnectTimeout = 300 * time.Millisecond
	db, err := mysql.Open(context.Background(), cfg, mysql.Lazy(true))
	if err != nil {
		t.Fatalf("a lazy pool on an unreachable server: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Ping(context.Background()); err == nil {
		t.Fatal("a lazy pool on an unreachable server answered a ping")
	}
	if _, err := mysql.Open(context.Background(), cfg, mysql.Lazy(true), mysql.Lazy(false)); err == nil {
		t.Fatal("Lazy(false) did not connect at Open")
	}
}

// TestUnknownParameterNamesAreNotEchoed is a regression test: with an
// unencoded "?" in the password, part of the password became a parameter
// name and was quoted in the startup error.
func TestUnknownParameterNamesAreNotEchoed(t *testing.T) {
	t.Parallel()
	cfg := config.Default().Database
	// "p&secretpart" was meant as the password: the unencoded "&" made
	// "secretpart" a parameter name.
	cfg.URL = config.NewSecret("mysql://u@h/db?tls=false&secretpart")
	_, err := mysql.Open(context.Background(), cfg)
	if err == nil || strings.Contains(err.Error(), "secretpart") || !strings.Contains(err.Error(), "unsupported parameter") {
		t.Fatalf("expected a generic unsupported-parameter error without the fragment, got %v", err)
	}
}
