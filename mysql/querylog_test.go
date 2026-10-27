package mysql_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/mysql"
	"github.com/rocketinsidecorp/forge-go/mysql/mysqltest"
	"github.com/rocketinsidecorp/forge-go/telemetry"
)

// On MySQL and MariaDB "..." is a string, not an identifier (Forge's sql_mode
// has no ANSI_QUOTES), and a backslash escapes: the statement lines must not
// carry either kind of literal.
func TestStatementLogMasksMySQLLiterals(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	tel, err := telemetry.New(telemetry.Options{ServiceName: "test", QueryLog: &telemetry.QueryLog{Logger: logger, Level: slog.LevelInfo}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	ctx := context.Background()
	db, err := mysql.Open(ctx, mysqltest.Config(t), mysql.WithTelemetry(tel))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, query := range []string{
		`SELECT "hunter2-mysql-dq" AS pw`,
		`SELECT CONCAT('C:\\', 'topsecret-bs') AS p`,
		"SELECT 1 # hash-secret\n + 2",
	} {
		var scanned any
		if err := db.QueryRowContext(ctx, query).Scan(&scanned); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	got := out.String()
	t.Logf("statement log:\n%s", got)
	for _, leak := range []string{"hunter2-mysql-dq", "topsecret-bs", "hash-secret"} {
		if strings.Contains(got, leak) {
			t.Fatalf("%q leaked into the statement log: %s", leak, got)
		}
	}
	if !strings.Contains(got, `SELECT \"?\" AS pw`) {
		t.Fatalf("the statement is not logged: %s", got)
	}
}
