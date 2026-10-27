package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/web"
)

var allDialects = []string{DialectPostgreSQL, DialectMySQL, DialectSQLite, "mariadb", ""}

// What every dialect, and an unknown one, masks the same way.
func TestSanitizeStatementCommonRules(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"placeholders stay":      {"SELECT id FROM posts WHERE id = $1 AND slug = $2", "SELECT id FROM posts WHERE id = $1 AND slug = $2"},
		"question marks stay":    {"SELECT * FROM posts WHERE id = ? AND a = :name AND b = @p1", "SELECT * FROM posts WHERE id = ? AND a = :name AND b = @p1"},
		"whitespace collapses":   {"  SELECT  a,\n\tb\r\n FROM   t \n", "SELECT a, b FROM t"},
		"string literal":         {"SELECT * FROM users WHERE email = 'ana@example.com'", "SELECT * FROM users WHERE email = '?'"},
		"doubled quote":          {"SELECT 'it''s a secret', x", "SELECT '?', x"},
		"unterminated string":    {"SELECT 'oops secret", "SELECT '?'"},
		"numeric literals":       {"SELECT * FROM t WHERE cpf = 12345678901 AND n = 1.5e3 AND h = 0xFF LIMIT 10", "SELECT * FROM t WHERE cpf = ? AND n = ? AND h = ? LIMIT ?"},
		"digits in identifiers":  {"SELECT t1.col_2, a$b FROM t1 JOIN t2 ON t1.id = t2.t1_id", "SELECT t1.col_2, a$b FROM t1 JOIN t2 ON t1.id = t2.t1_id"},
		"backtick identifiers":   {"SELECT `other col` FROM `t`", "SELECT `other col` FROM `t`"},
		"unterminated backtick":  {"SELECT `oops secret", "SELECT `?"},
		"line comment":           {"SELECT 1 -- secret note\nFROM t", "SELECT ? FROM t"},
		"trailing line comment":  {"SELECT 1 -- secret", "SELECT ?"},
		"block comment":          {"SELECT /* secret */ 1 FROM t", "SELECT ? FROM t"},
		"unterminated block":     {"SELECT 1 /* secret", "SELECT ?"},
		"lone dollar":            {"SELECT $ FROM t", "SELECT $ FROM t"},
		"incomplete dollar tags": {"SELECT $abc, $ab1", "SELECT $abc, $ab1"},
		"multi-byte":             {"SELECT título, 'ç' FROM tábua", "SELECT título, '?' FROM tábua"},
		"operators and casts":    {"SELECT a::int, b->>'k' FROM t WHERE c <> 5", "SELECT a::int, b->>'?' FROM t WHERE c <> ?"},
		"empty":                  {"", ""},
		"minus and dollar digit": {"SELECT a - $1, -2", "SELECT a - $1, -?"},
	} {
		for _, system := range allDialects {
			if got := SanitizeStatementFor(system, tc.in); got != tc.want {
				t.Errorf("%s (%q):\n got %q\nwant %q", name, system, got, tc.want)
			}
		}
	}
	if SanitizeStatement("SELECT 'x'") != SanitizeStatementFor("", "SELECT 'x'") {
		t.Error("SanitizeStatement is the unknown dialect")
	}
}

type dialectCase struct{ name, in, want string }

func runDialectCases(t *testing.T, systems []string, cases []dialectCase) {
	t.Helper()
	for _, tc := range cases {
		for _, system := range systems {
			if got := SanitizeStatementFor(system, tc.in); got != tc.want {
				t.Errorf("%s (%q):\n got %q\nwant %q", tc.name, system, got, tc.want)
			}
		}
	}
}

// MySQL and MariaDB: "..." is a string (Forge's sql_mode has no ANSI_QUOTES),
// a backslash escapes, # and "-- " are comments.
func TestSanitizeStatementMySQL(t *testing.T) {
	runDialectCases(t, []string{DialectMySQL, "mariadb"}, []dialectCase{
		{"double-quoted string", `SELECT "hunter2-mysql-dq" AS pw`, `SELECT "?" AS pw`},
		{"double-quoted in a comparison", `SELECT * FROM users WHERE token = "s3cr3t" AND id = 4`, `SELECT * FROM users WHERE token = "?" AND id = ?`},
		{"doubled double quote", `SELECT "a""secret", x`, `SELECT "?", x`},
		{"escaped double quote", `SELECT "a\"secret", x`, `SELECT "?", x`},
		{"escaped single quote", `SELECT 'a\'b secret', x`, "SELECT '?', x"},
		{"backslash at the end of a string", `SELECT 'C:\\' AS p, 'topsecret' AS q`, "SELECT '?' AS p, '?' AS q"},
		{"hash comment", "SELECT 1 # secret note\nFROM t", "SELECT ? FROM t"},
		{"hash comment at the end", "SELECT 1 # secret", "SELECT ?"},
		{"dash comment needs a space", "SELECT 1 -- secret\nFROM t", "SELECT ? FROM t"},
		{"dash comment before a tab", "SELECT 1 --\tsecret\nFROM t", "SELECT ? FROM t"},
		{"dash comment at the end", "SELECT 1 --", "SELECT ?"},
		{"double dash without a space is arithmetic", "SELECT 5--3, 'x'", "SELECT ?--?, '?'"},
		{"hash is not a comment in a string", "SELECT '#', 7", "SELECT '?', ?"},
		{"dollar quotes are not quotes", "SELECT $$ 'secret'", "SELECT $$ '?'"},
		{"block comments do not nest", "SELECT /* a /* b */ 'secret' */ 1", "SELECT '?' */ ?"},
		{"unterminated double quote masks the rest", `SELECT "oops secret, 'more'`, `SELECT "?"`},
		{"unterminated string after an escape", `SELECT 'oops\'`, "SELECT '?'"},
	})
}

// PostgreSQL: a backslash escapes only inside E'...', dollar quotes are
// strings, block comments nest, "..." is an identifier.
func TestSanitizeStatementPostgreSQL(t *testing.T) {
	runDialectCases(t, []string{DialectPostgreSQL}, []dialectCase{
		{"backslash is not an escape", `SELECT 'C:\' AS p, 'topsecret' AS q`, "SELECT '?' AS p, '?' AS q"},
		{"backslash then quote ends the string", `SELECT 'a\', 'secret'`, "SELECT '?', '?'"},
		{"E string escapes", `SELECT E'a\'b secret', x`, "SELECT E'?', x"},
		{"lowercase e string escapes", `SELECT e'a\'b secret', x`, "SELECT e'?', x"},
		{"E must be a token", `SELECT name'a\' , 'secret'`, "SELECT name'?' , '?'"},
		{"E at the start of the statement", `E'a\'b secret'`, "E'?'"},
		{"double-quoted identifier", `SELECT "Weird Name" FROM "t"`, `SELECT "Weird Name" FROM "t"`},
		{"unterminated identifier masks the rest", `SELECT "oops, 'secret'`, `SELECT "?`},
		{"dollar quoting", "DO $$ BEGIN secret(); END $$; SELECT 1", "DO $$?$$; SELECT ?"},
		{"tagged dollar quoting", "SELECT $tag$secret$tag$, $1", "SELECT $$?$$, $1"},
		{"unterminated dollar quote masks the rest", "SELECT $q$secret", "SELECT $$?$$"},
		{"nested block comment", "SELECT /* a /* b */ secret */ 1", "SELECT ?"},
		{"unterminated nested comment drops the rest", "SELECT /* a /* b */ secret", "SELECT"},
		{"hash is an operator", "SELECT a #> '{k}'", "SELECT a #> '?'"},
		{"dash comment without a space", "SELECT 1 --secret\nFROM t", "SELECT ? FROM t"},
	})
}

// SQLite: no backslash escapes, "..." is an identifier.
func TestSanitizeStatementSQLite(t *testing.T) {
	runDialectCases(t, []string{DialectSQLite}, []dialectCase{
		{"backslash is not an escape", `SELECT 'C:\' AS p, 'topsecret' AS q`, "SELECT '?' AS p, '?' AS q"},
		{"double-quoted identifier", `SELECT "id", "name" FROM "products" LIMIT ?`, `SELECT "id", "name" FROM "products" LIMIT ?`},
		{"unterminated identifier masks the rest", `SELECT "oops, 'secret'`, `SELECT "?`},
		{"dollar is not a quote", "SELECT $a$ 'secret'", "SELECT $a$ '?'"},
		{"hash is not a comment", "SELECT 1 # 'secret'", "SELECT ? # '?'"},
		{"block comments do not nest", "SELECT /* a /* b */ 'secret' */ 1", "SELECT '?' */ ?"},
	})
}

// An unknown dialect cannot tell a backslash from an escape: it masks the
// rest of the statement rather than guess, and treats "..." as a string.
func TestSanitizeStatementUnknownDialectFailsClosed(t *testing.T) {
	runDialectCases(t, []string{"", "oracle"}, []dialectCase{
		{"backslash masks the rest", `SELECT 'C:\' AS p, 'topsecret' AS q`, "SELECT '?'"},
		{"escaped quote masks the rest", `SELECT 'a\'b secret', x`, "SELECT '?'"},
		{"double quotes are strings", `SELECT "hunter2" AS pw`, `SELECT "?" AS pw`},
		{"dollar quotes are strings", "SELECT $$secret$$, 1", "SELECT $$?$$, ?"},
		{"hash comment", "SELECT 1 # secret", "SELECT ?"},
	})
}

func TestSanitizeStatementTruncates(t *testing.T) {
	long := "SELECT " + strings.Repeat("column_name, ", 100) + "x FROM t"
	got := SanitizeStatement(long)
	if utf8.RuneCountInString(got) != MaxLoggedStatement || !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, "SELECT column_name,") {
		t.Fatalf("%d runes: %q", utf8.RuneCountInString(got), got)
	}
	// Multi-byte text is cut between runes.
	got = SanitizeStatement("SELECT " + strings.Repeat("é", 1000))
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != MaxLoggedStatement {
		t.Fatalf("%q", got)
	}
	// A huge statement is not scanned to its end.
	huge := "SELECT " + strings.Repeat("a ", 1<<20)
	if got := SanitizeStatement(huge); utf8.RuneCountInString(got) != MaxLoggedStatement {
		t.Fatalf("%d", utf8.RuneCountInString(got))
	}
	if SanitizeStatement(strings.Repeat("x", MaxLoggedStatement)) != strings.Repeat("x", MaxLoggedStatement) {
		t.Fatal("a statement of exactly the maximum is kept")
	}
}

func TestQueryLogWritesTheStatementNeverArguments(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	q := &QueryLog{Logger: logger, Level: slog.LevelInfo, Slow: 100 * time.Millisecond}
	ctx := web.WithRequestID(context.Background(), "req-1")

	q.Log(ctx, "sqlite", "SELECT * FROM users WHERE email = $1 AND token = 'abc'", 2*time.Millisecond, "")
	got := out.String()
	for _, want := range []string{`"msg":"sql query"`, `"level":"INFO"`, `"statement":"SELECT * FROM users WHERE email = $1 AND token = '?'"`, `"request_id":"req-1"`, `"duration":2000000`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "abc") || strings.Contains(got, `"slow"`) || strings.Contains(got, `"error"`) {
		t.Fatalf("%s", got)
	}

	out.Reset()
	q.Log(context.Background(), "sqlite", "UPDATE t SET a = 1", 150*time.Millisecond, "timeout")
	got = out.String()
	if !strings.Contains(got, `"slow":true`) || !strings.Contains(got, `"error":"timeout"`) || strings.Contains(got, "request_id") {
		t.Fatalf("%s", got)
	}

	// Exactly at the threshold is not slow; a zero threshold never flags.
	out.Reset()
	q.Log(context.Background(), "sqlite", "SELECT 1", 100*time.Millisecond, "")
	(&QueryLog{Logger: logger, Level: slog.LevelInfo}).Log(context.Background(), "sqlite", "SELECT 1", time.Hour, "")
	if strings.Contains(out.String(), "slow") {
		t.Fatalf("%s", out.String())
	}
}

func TestQueryLogLevelAndNil(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	// Debug in production: the default Info handler drops it.
	(&QueryLog{Logger: logger, Level: slog.LevelDebug}).Log(context.Background(), "sqlite", "SELECT 1", time.Millisecond, "")
	if out.Len() != 0 {
		t.Fatalf("%s", out.String())
	}
	var none *QueryLog
	none.Log(context.Background(), "sqlite", "SELECT 1", time.Millisecond, "")
	(&QueryLog{}).Log(context.Background(), "sqlite", "SELECT 1", time.Millisecond, "")
	var tel *Telemetry
	if tel.QueryLog() != nil {
		t.Fatal("a nil Telemetry has no query log")
	}
	want := &QueryLog{Logger: logger}
	if got := newTestTelemetry(t, Options{QueryLog: want}).QueryLog(); got != want {
		t.Fatal("Options.QueryLog is not kept")
	}
}

func TestQueryCounter(t *testing.T) {
	// Without a counter counting does nothing.
	CountQuery(context.Background())
	ctx, counter := WithQueryCounter(context.Background())
	if counter.Count() != 0 {
		t.Fatal("a new counter starts at zero")
	}
	for range 3 {
		CountQuery(ctx)
	}
	// The counter follows the context to derived contexts and goroutines.
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { CountQuery(child); close(done) }()
	<-done
	if counter.Count() != 4 {
		t.Fatalf("count %d", counter.Count())
	}
	if allocs := testing.AllocsPerRun(100, func() { CountQuery(ctx) }); allocs != 0 {
		t.Fatalf("CountQuery allocates %v", allocs)
	}
}

func TestFromConfigLogsStatementsAtTheEventLevel(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for environment, want := range map[config.Environment]slog.Level{config.Development: slog.LevelInfo, config.Test: slog.LevelDebug} {
		cfg := config.Default()
		cfg.Environment = environment
		cfg.Log.SlowQuery = 7 * time.Millisecond
		tel, err := FromConfig(cfg, logger)
		if err != nil {
			t.Fatal(err)
		}
		got := tel.QueryLog()
		if got == nil || got.Logger != logger || got.Level != want || got.Slow != 7*time.Millisecond {
			t.Fatalf("%s: %+v", environment, got)
		}
		shutdown(t, tel)
	}
	tel, err := FromConfig(config.Default(), nil)
	if err != nil || tel.QueryLog() != nil {
		t.Fatalf("without a logger there is nothing to log to: %v %v", tel.QueryLog(), err)
	}
	shutdown(t, tel)
}
