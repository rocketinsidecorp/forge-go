package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/internal/waittest"
	"github.com/rocketinsidecorp/forge-go/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
	"github.com/rocketinsidecorp/forge-go/web"
)

// serveLogging serves an application with the environment env, a SQLite pool
// and these routes, sends each of paths once, stops it and returns what it
// logged to standard output:
//
//	GET /posts/{id}  runs three statements, one with an email in its text
//	GET /login       logs a record with sensitive attributes
func serveLogging(t *testing.T, env map[string]string, paths ...string) string {
	t.Helper()
	env["FORGE_HTTP_ADDR"] = anyAddress
	lookup := envLookup(env)
	database := sqlitetest.Config(t)
	configure := func(ctx context.Context, app *forge.App) error {
		db, err := sqlite.Open(ctx, database, sqlite.WithTelemetry(app.Telemetry()))
		if err != nil {
			return err
		}
		if err := app.OnShutdown(db.Shutdown); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, "CREATE TABLE posts (id INTEGER PRIMARY KEY, title TEXT, owner TEXT)"); err != nil {
			return err
		}
		if err := app.HandleFunc("GET /posts/{id}", func(w http.ResponseWriter, r *http.Request) {
			for _, query := range []string{
				"SELECT id, title FROM posts WHERE id = ?",
				"SELECT owner FROM posts WHERE owner = 'ana@example.com'",
				"SELECT count(*) FROM posts",
			} {
				var scanned any
				_ = db.QueryRowContext(r.Context(), query, r.PathValue("id")).Scan(&scanned)
			}
			w.WriteHeader(http.StatusOK)
		}); err != nil {
			return err
		}
		return app.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
			web.Logger(r.Context()).Info("login attempt", "email", "ana@example.com", "password", "hunter2", "user", "ana")
			w.WriteHeader(http.StatusNoContent)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	var output safeBuffer
	done := make(chan int, 1)
	go func() { done <- forge.Execute(ctx, nil, lookup, &output, io.Discard, configure) }()

	address := regexp.MustCompile(`serving HTTP.*?address"?[=:]"?([0-9.:]+)`)
	var base string
	waitFor(t, func() bool {
		match := address.FindStringSubmatch(output.String())
		if match == nil {
			return false
		}
		base = "http://" + match[1]
		return true
	})
	for _, path := range paths {
		response, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d: %s", code, output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	return output.String()
}

// The development log of a request reads like Rails': the statements the
// request ran, then its line with the route (not the path), the status, the
// time and the number of queries, which is how an N+1 shows.
func TestPrettyDevelopmentLog(t *testing.T) {
	got := serveLogging(t, map[string]string{"FORGE_ENV": "development", "FORGE_LOG_FORMAT": "pretty"}, "/posts/42?token=tok-leak-1&email=ana@example.com")
	if strings.Contains(got, "\x1b") {
		t.Fatalf("colors on a pipe: %q", got)
	}
	// Every statement of the request, then the request line.
	sql := regexp.MustCompile(`(?m)^ {14}SQL   \S+  SELECT id, title FROM posts WHERE id = \?\n` +
		` {14}SQL   \S+  SELECT owner FROM posts WHERE owner = '\?'\n` +
		` {14}SQL   \S+  SELECT count\(\*\) FROM posts\n` +
		`\d\d:\d\d:\d\d\.\d{3}  HTTP  GET /posts/\{id\}  200  \S+  queries=3  req=[0-9a-f-]{36}\n`)
	if !sql.MatchString(got) {
		t.Fatalf("the request is not laid out as SQL lines and an HTTP line:\n%s", got)
	}
	// Nothing of the request but its route: no path value, no query string, no
	// argument, no literal of a statement.
	for _, leak := range []string{"/posts/42", "tok-leak-1", "ana@example.com"} {
		if strings.Contains(got, leak) {
			t.Fatalf("%q leaked into the log:\n%s", leak, got)
		}
	}
	// Other records keep the plain layout.
	if !regexp.MustCompile(`(?m)^\d\d:\d\d:\d\d\.\d{3}  INFO  serving HTTP  address=\S+$`).MatchString(got) {
		t.Fatalf("the serving line:\n%s", got)
	}
}

func TestPrettyLogRedactsSensitiveAttributes(t *testing.T) {
	got := serveLogging(t, map[string]string{"FORGE_ENV": "development", "FORGE_LOG_FORMAT": "pretty", "FORGE_LOG_REDACT_KEYS": "user"}, "/login")
	if !regexp.MustCompile(`login attempt  .*email=\[REDACTED\] password=\[REDACTED\] user=\[REDACTED\]`).MatchString(got) {
		t.Fatalf("%s", got)
	}
	for _, leak := range []string{"ana@example.com", "hunter2", "=ana"} {
		if strings.Contains(got, leak) {
			t.Fatalf("%q leaked:\n%s", leak, got)
		}
	}
}

// Outside development the request, statement and job lines are Debug: off by
// default, on with FORGE_LOG_LEVEL=debug, and then in JSON with the event
// names every format shares.
func TestJSONLogCarriesTheSameEvents(t *testing.T) {
	off := serveLogging(t, map[string]string{"FORGE_ENV": "staging"}, "/posts/1")
	if strings.Contains(off, "http request") || strings.Contains(off, "sql query") {
		t.Fatalf("events at Info outside development:\n%s", off)
	}
	got := serveLogging(t, map[string]string{"FORGE_ENV": "staging", "FORGE_LOG_LEVEL": "debug"}, "/posts/1?token=tok-leak-1", "/login")
	var http, statements, redacted int
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		switch record["msg"] {
		case "http request":
			http++
			if record["route"] == "/posts/{id}" {
				if record["queries"] != float64(3) || record["level"] != "DEBUG" || record["status"] != float64(200) {
					t.Fatalf("%v", record)
				}
			}
		case "sql query":
			// The startup statement belongs to no request.
			if record["request_id"] != nil {
				statements++
			}
			if record["statement"] == nil {
				t.Fatalf("%v", record)
			}
		case "login attempt":
			if record["email"] == "[REDACTED]" && record["password"] == "[REDACTED]" && record["user"] == "ana" {
				redacted++
			}
		}
	}
	if http != 2 || statements != 3 || redacted != 1 {
		t.Fatalf("http %d, statements %d, redacted %d:\n%s", http, statements, redacted, got)
	}
	if strings.Contains(got, "tok-leak-1") || strings.Contains(got, "ana@example.com") || strings.Contains(got, "hunter2") {
		t.Fatalf("leak:\n%s", got)
	}
}

func TestLogFormatAutoWritesTextToAPipeInDevelopment(t *testing.T) {
	got := serveLogging(t, map[string]string{"FORGE_ENV": "development"}, "/login")
	if !strings.Contains(got, "level=INFO msg=\"http request\"") || !strings.Contains(got, `status=204`) || !strings.Contains(got, "email=[REDACTED]") {
		t.Fatalf("%s", got)
	}
}

// anyAddress, envLookup, waitFor and safeBuffer are the root package's test
// helpers (runtime_test.go, forge_telemetry_test.go), for its log tests that
// moved here.
const anyAddress = "127.0.0.1:0"

func envLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	waittest.Until(t, 5*time.Second, "the condition", condition)
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
