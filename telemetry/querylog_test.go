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

func TestSanitizeStatement(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"placeholders stay":      {"SELECT id FROM posts WHERE id = $1 AND slug = $2", "SELECT id FROM posts WHERE id = $1 AND slug = $2"},
		"question marks stay":    {"SELECT * FROM posts WHERE id = ? AND a = :name AND b = @p1", "SELECT * FROM posts WHERE id = ? AND a = :name AND b = @p1"},
		"whitespace collapses":   {"  SELECT  a,\n\tb\r\n FROM   t \n", "SELECT a, b FROM t"},
		"string literal":         {"SELECT * FROM users WHERE email = 'ana@example.com'", "SELECT * FROM users WHERE email = '?'"},
		"doubled quote":          {"SELECT 'it''s a secret', x", "SELECT '?', x"},
		"backslash escape":       {`SELECT 'a\'b secret', x`, "SELECT '?', x"},
		"unterminated string":    {"SELECT 'oops secret", "SELECT '?'"},
		"numeric literals":       {"SELECT * FROM t WHERE cpf = 12345678901 AND n = 1.5e3 AND h = 0xFF LIMIT 10", "SELECT * FROM t WHERE cpf = ? AND n = ? AND h = ? LIMIT ?"},
		"digits in identifiers":  {"SELECT t1.col_2, a$b FROM t1 JOIN t2 ON t1.id = t2.t1_id", "SELECT t1.col_2, a$b FROM t1 JOIN t2 ON t1.id = t2.t1_id"},
		"quoted identifiers":     {`SELECT "Weird Name", ` + "`other col`" + ` FROM "t"`, `SELECT "Weird Name", ` + "`other col`" + ` FROM "t"`},
		"unterminated ident":     {`SELECT "oops`, `SELECT "oops`},
		"dollar quoting":         {"DO $$ BEGIN secret(); END $$; SELECT 1", "DO $$?$$; SELECT ?"},
		"tagged dollar quoting":  {"SELECT $tag$secret$tag$, $1", "SELECT $$?$$, $1"},
		"unterminated dollar":    {"SELECT $q$secret", "SELECT $$?$$"},
		"line comment":           {"SELECT 1 -- secret note\nFROM t", "SELECT ? FROM t"},
		"trailing line comment":  {"SELECT 1 -- secret", "SELECT ?"},
		"block comment":          {"SELECT /* secret */ 1 FROM t", "SELECT ? FROM t"},
		"unterminated block":     {"SELECT 1 /* secret", "SELECT ?"},
		"lone dollar":            {"SELECT $ FROM t", "SELECT $ FROM t"},
		"multi-byte":             {"SELECT título, 'ç' FROM tábua", "SELECT título, '?' FROM tábua"},
		"operators and casts":    {"SELECT a::int, b->>'k' FROM t WHERE c <> 5", "SELECT a::int, b->>'?' FROM t WHERE c <> ?"},
		"empty":                  {"", ""},
		"minus and dollar digit": {"SELECT a - $1, -2", "SELECT a - $1, -?"},
	} {
		if got := SanitizeStatement(tc.in); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
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

	q.Log(ctx, "SELECT * FROM users WHERE email = $1 AND token = 'abc'", 2*time.Millisecond, "")
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
	q.Log(context.Background(), "UPDATE t SET a = 1", 150*time.Millisecond, "timeout")
	got = out.String()
	if !strings.Contains(got, `"slow":true`) || !strings.Contains(got, `"error":"timeout"`) || strings.Contains(got, "request_id") {
		t.Fatalf("%s", got)
	}

	// Exactly at the threshold is not slow; a zero threshold never flags.
	out.Reset()
	q.Log(context.Background(), "SELECT 1", 100*time.Millisecond, "")
	(&QueryLog{Logger: logger, Level: slog.LevelInfo}).Log(context.Background(), "SELECT 1", time.Hour, "")
	if strings.Contains(out.String(), "slow") {
		t.Fatalf("%s", out.String())
	}
}

func TestQueryLogLevelAndNil(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	// Debug in production: the default Info handler drops it.
	(&QueryLog{Logger: logger, Level: slog.LevelDebug}).Log(context.Background(), "SELECT 1", time.Millisecond, "")
	if out.Len() != 0 {
		t.Fatalf("%s", out.String())
	}
	var none *QueryLog
	none.Log(context.Background(), "SELECT 1", time.Millisecond, "")
	(&QueryLog{}).Log(context.Background(), "SELECT 1", time.Millisecond, "")
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

func TestSanitizeStatementIncompleteDollarTags(t *testing.T) {
	// $abc without its closing $ is not a quote tag, and $1 is a placeholder.
	if got, want := SanitizeStatement("SELECT $abc, $ab1"), "SELECT $abc, $ab1"; got != want {
		t.Fatalf("%q want %q", got, want)
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
