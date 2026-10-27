package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var prettyTime = time.Date(2026, 10, 27, 12, 4, 5, 123_000_000, time.UTC)

// prettyLine writes one record through a handler and returns what it wrote.
func prettyLine(t *testing.T, opts PrettyOptions, level slog.Level, message string, attrs ...slog.Attr) string {
	t.Helper()
	var out bytes.Buffer
	handler := NewPrettyHandler(&out, opts)
	record := slog.NewRecord(prettyTime, level, message, 0)
	record.AddAttrs(attrs...)
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

const requestID = "0197a3c2-0000-7000-8000-000000000001"

func httpAttrs(status int, extra ...slog.Attr) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("method", "GET"), slog.String("route", "/posts/{id}"), slog.Int("status", status),
		slog.Duration("duration", 4100*time.Microsecond),
	}
	return append(attrs, extra...)
}

// paint is the expected ANSI form, written out in the tests so that a change
// of colors is a deliberate edit of the goldens.
func paint(code, text string) string { return "\x1b[" + code + "m" + text + "\x1b[0m" }

func TestPrettyHTTPLine(t *testing.T) {
	attrs := httpAttrs(200, slog.Int("queries", 3), slog.String("request_id", requestID), slog.String(LogTraceIDKey, "abc"))
	plain := "12:04:05.123  HTTP  GET /posts/{id}  200  4.1ms  queries=3  req=" + requestID + "\n"
	if got := prettyLine(t, PrettyOptions{}, slog.LevelInfo, LogEventHTTP, attrs...); got != plain {
		t.Fatalf("plain:\n%q\nwant\n%q", got, plain)
	}
	colored := paint("2", "12:04:05.123") + "  " + paint("34", "HTTP") + "  GET /posts/{id}  " + paint("32", "200") +
		"  4.1ms  queries=3  " + paint("2", "req="+requestID) + "\n"
	if got := prettyLine(t, PrettyOptions{Color: true}, slog.LevelInfo, LogEventHTTP, attrs...); got != colored {
		t.Fatalf("color:\n%q\nwant\n%q", got, colored)
	}
}

func TestPrettyHTTPStatusColors(t *testing.T) {
	for status, code := range map[int]string{101: "2", 200: "32", 204: "32", 302: "36", 404: "33", 422: "33", 500: "31", 503: "31"} {
		got := prettyLine(t, PrettyOptions{Color: true}, slog.LevelInfo, LogEventHTTP, httpAttrs(status)...)
		if !strings.Contains(got, paint(code, fmt.Sprint(status))) {
			t.Errorf("status %d: %q", status, got)
		}
	}
}

func TestPrettyHTTPRouteOfTheRootPath(t *testing.T) {
	for route, want := range map[string]string{"/{$}": "/", "/posts/{$}": "/posts/", "/posts/{id}": "/posts/{id}"} {
		got := prettyLine(t, PrettyOptions{}, slog.LevelInfo, LogEventHTTP,
			slog.String("method", "GET"), slog.String("route", route), slog.Int("status", 200), slog.Duration("duration", time.Millisecond))
		if wantLine := "12:04:05.123  HTTP  GET " + want + "  200  1ms\n"; got != wantLine {
			t.Errorf("%s: %q want %q", route, got, wantLine)
		}
	}
}

func TestPrettyDimsCorrelationIDs(t *testing.T) {
	got := prettyLine(t, PrettyOptions{Color: true}, slog.LevelWarn, "rejected", slog.String("request_id", "r1"), slog.String(LogTraceIDKey, "t1"), slog.String("code", "x"))
	want := paint("2", "12:04:05.123") + "  " + paint("33", "WARN") + "  rejected  " + paint("2", "request_id=r1") + " " + paint("2", "trace_id=t1") + " " + paint("2", "code=") + "x\n"
	if got != want {
		t.Fatalf("%q want %q", got, want)
	}
}

func TestPrettyHTTPWithoutRouteOrRequest(t *testing.T) {
	got := prettyLine(t, PrettyOptions{}, slog.LevelInfo, LogEventHTTP,
		slog.String("method", "GET"), slog.String("route", ""), slog.Int("status", 404), slog.Duration("duration", 850*time.Microsecond),
		slog.String("extra", "a b"))
	if want := "12:04:05.123  HTTP  GET -  404  850µs  extra=\"a b\"\n"; got != want {
		t.Fatalf("%q want %q", got, want)
	}
}

func TestPrettySQLLines(t *testing.T) {
	statement := "SELECT id, title FROM posts WHERE id = $1"
	base := []slog.Attr{slog.String("statement", statement), slog.Duration("duration", 2300*time.Microsecond)}
	inRequest := append(append([]slog.Attr(nil), base...), slog.String("request_id", requestID))

	// Inside a request the timestamp column is blank: the line reads as the
	// request's body.
	if got, want := prettyLine(t, PrettyOptions{}, slog.LevelInfo, LogEventSQL, inRequest...), "              SQL   2.3ms  "+statement+"\n"; got != want {
		t.Fatalf("in request:\n%q\nwant\n%q", got, want)
	}
	if got, want := prettyLine(t, PrettyOptions{}, slog.LevelInfo, LogEventSQL, base...), "12:04:05.123  SQL   2.3ms  "+statement+"\n"; got != want {
		t.Fatalf("outside a request:\n%q\nwant\n%q", got, want)
	}
	fast := "            " + "  " + paint("35", "SQL ") + "  2.3ms  " + statement + "\n"
	if got := prettyLine(t, PrettyOptions{Color: true}, slog.LevelInfo, LogEventSQL, inRequest...); got != fast {
		t.Fatalf("color:\n%q\nwant\n%q", got, fast)
	}
	slow := append(append([]slog.Attr(nil), inRequest...), slog.Bool("slow", true))
	want := "            " + "  " + paint("35", "SQL ") + "  " + paint("33", "2.3ms") + "  " + statement + "\n"
	if got := prettyLine(t, PrettyOptions{Color: true}, slog.LevelInfo, LogEventSQL, slow...); got != want {
		t.Fatalf("slow:\n%q\nwant\n%q", got, want)
	}
	failed := append(append([]slog.Attr(nil), inRequest...), slog.String("error", "unique_violation"))
	want = "            " + "  " + paint("35", "SQL ") + "  " + paint("31", "2.3ms") + "  " + statement + "  " + paint("31", "error=unique_violation") + "\n"
	if got := prettyLine(t, PrettyOptions{Color: true}, slog.LevelInfo, LogEventSQL, failed...); got != want {
		t.Fatalf("failed:\n%q\nwant\n%q", got, want)
	}
}

func jobAttrs(outcome string, extra ...slog.Attr) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("component", "queue"), slog.String("worker_id", "w1"),
		slog.String("job_id", "job-1"), slog.String("kind", "send_welcome"), slog.String("queue", "default"),
		slog.Int("attempt", 1), slog.Int("max_attempts", 5), slog.String("outcome", outcome),
		slog.Duration("duration", 35*time.Millisecond),
	}
	return append(attrs, extra...)
}

func TestPrettyJobLines(t *testing.T) {
	got := prettyLine(t, PrettyOptions{}, slog.LevelInfo, "job succeeded", jobAttrs("succeeded")...)
	if want := "12:04:05.123  JOB   send_welcome succeeded  35ms  queue=default attempt=1\n"; got != want {
		t.Fatalf("%q want %q", got, want)
	}
	got = prettyLine(t, PrettyOptions{Color: true}, slog.LevelInfo, "job succeeded", jobAttrs("succeeded")...)
	want := paint("2", "12:04:05.123") + "  " + paint("36", "JOB ") + "  send_welcome " + paint("32", "succeeded") + "  35ms  " +
		paint("2", "queue=") + "default " + paint("2", "attempt=") + "1\n"
	if got != want {
		t.Fatalf("color:\n%q\nwant\n%q", got, want)
	}

	// A failure is red and carries what an operator needs to act: the job
	// and the sanitized error.
	retry := jobAttrs("retry", slog.Duration("retry_in", 5*time.Second), slog.String("error", "boom"))
	got = prettyLine(t, PrettyOptions{Color: true}, slog.LevelWarn, "job failed; retry scheduled", retry...)
	want = paint("2", "12:04:05.123") + "  " + paint("36", "JOB ") + "  send_welcome " + paint("31", "failed, retry in 5s") + "  35ms  " +
		paint("2", "queue=") + "default " + paint("2", "attempt=") + "1 " + paint("2", "job_id=") + "job-1 " + paint("2", "error=") + "boom\n"
	if got != want {
		t.Fatalf("retry:\n%q\nwant\n%q", got, want)
	}
	got = prettyLine(t, PrettyOptions{}, slog.LevelError, "job failed permanently", jobAttrs("dead", slog.String("error", "bad input"))...)
	if want := "12:04:05.123  JOB   send_welcome failed permanently  35ms  queue=default attempt=1 job_id=job-1 error=\"bad input\"\n"; got != want {
		t.Fatalf("dead:\n%q\nwant\n%q", got, want)
	}
	got = prettyLine(t, PrettyOptions{}, slog.LevelWarn, "job failed; retry scheduled", jobAttrs("retry")...)
	if !strings.Contains(got, "send_welcome failed, retrying  35ms") {
		t.Fatalf("retry without delay: %q", got)
	}
	got = prettyLine(t, PrettyOptions{Color: true}, slog.LevelInfo, "job released at shutdown", jobAttrs("released")...)
	if !strings.Contains(got, paint("33", "released")) {
		t.Fatalf("released: %q", got)
	}
}

func TestPrettyOtherRecords(t *testing.T) {
	got := prettyLine(t, PrettyOptions{}, slog.LevelInfo, "serving HTTP", slog.String("address", "127.0.0.1:8080"), slog.Int("workers", 2))
	if want := "12:04:05.123  INFO  serving HTTP  address=127.0.0.1:8080 workers=2\n"; got != want {
		t.Fatalf("%q want %q", got, want)
	}
	for level, tag := range map[slog.Level]string{slog.LevelDebug: "DBG ", slog.LevelInfo: "INFO", slog.LevelWarn: "WARN", slog.LevelError: "ERR ", slog.LevelError + 4: "ERR "} {
		if got := prettyLine(t, PrettyOptions{}, level, "m"); got != "12:04:05.123  "+tag+"  m\n" {
			t.Errorf("%v: %q", level, got)
		}
	}
	colors := map[slog.Level]string{slog.LevelDebug: "2", slog.LevelInfo: "32", slog.LevelWarn: "33", slog.LevelError: "31"}
	for level, code := range colors {
		got := prettyLine(t, PrettyOptions{Color: true}, level, "m", slog.String("k", "v"))
		if !strings.Contains(got, "\x1b["+code+"m") || !strings.Contains(got, paint("2", "k=")+"v") {
			t.Errorf("%v: %q", level, got)
		}
	}
	// Records that look like an event but lack its attributes are plain.
	for _, message := range []string{LogEventHTTP, LogEventSQL} {
		got := prettyLine(t, PrettyOptions{}, slog.LevelInfo, message, slog.String("method", "GET"))
		if want := "12:04:05.123  INFO  " + message + "  method=GET\n"; got != want {
			t.Errorf("%s: %q", message, got)
		}
	}
	// So does a job record without its outcome.
	got = prettyLine(t, PrettyOptions{}, slog.LevelWarn, "job lease lost", slog.String("kind", "k"), slog.String("job_id", "j"))
	if want := "12:04:05.123  WARN  job lease lost  kind=k job_id=j\n"; got != want {
		t.Fatalf("%q", got)
	}
}

type labeled struct{}

func (labeled) String() string { return "a stringer" }

func TestPrettyValueFormats(t *testing.T) {
	got := prettyLine(t, PrettyOptions{}, slog.LevelInfo, "m",
		slog.String("empty", ""), slog.String("spaces", "a b"), slog.String("equals", "a=b"), slog.String("quote", `a"b`),
		slog.Uint64("u", 7), slog.Float64("f", 1.5), slog.Bool("b", true), slog.Duration("d", 1250*time.Millisecond),
		slog.Time("t", prettyTime), slog.Any("err", errors.New("failed: x")), slog.Any("s", labeled{}), slog.Duration("tiny", 12*time.Microsecond), slog.Duration("slow", 12*time.Second), slog.Duration("round", 2*time.Second),
		slog.Duration("edge", 999950*time.Microsecond))
	want := `12:04:05.123  INFO  m  empty="" spaces="a b" equals="a=b" quote="a\"b" u=7 f=1.5 b=true d=1.25s ` +
		`t=2026-10-27T12:04:05.123Z err="failed: x" s="a stringer" tiny=12µs slow=12s round=2s edge=1000ms` + "\n"
	if got != want {
		t.Fatalf("\n%q\nwant\n%q", got, want)
	}
}

func TestPrettyEscapesControlCharacters(t *testing.T) {
	// Input cannot forge a line or drive the terminal, in the message, a
	// key or a value.
	got := prettyLine(t, PrettyOptions{Color: true}, slog.LevelInfo, "login\n12:00:00.000  ERR  forged\x1b[2J\t\r\x7f",
		slog.String("bad\nkey", "v\x1b[31mred"), slog.String("route", "x"))
	if strings.Count(got, "\n") != 1 || strings.Contains(got, "\x1b[2J") || strings.Contains(got, "\x1b[31mred") {
		t.Fatalf("not escaped: %q", got)
	}
	if !strings.Contains(got, `login\n12:00:00.000  ERR  forged\x1b[2J\t\r\x7f`) || !strings.Contains(got, `bad\nkey=`) || !strings.Contains(got, `"v\x1b[31mred"`) {
		t.Fatalf("escapes: %q", got)
	}
	got = prettyLine(t, PrettyOptions{}, slog.LevelInfo, LogEventHTTP,
		slog.String("method", "GE\nT"), slog.String("route", "/a\x1b[0m"), slog.Int("status", 200), slog.Duration("duration", time.Millisecond), slog.String("request_id", "r\n1"))
	if strings.Count(got, "\n") != 1 || strings.Contains(got, "\x1b") {
		t.Fatalf("http line not escaped: %q", got)
	}
	got = prettyLine(t, PrettyOptions{}, slog.LevelInfo, LogEventSQL,
		slog.String("statement", "SELECT \"a\x1b[2J\""), slog.Duration("duration", time.Millisecond), slog.String("error", "x\ny"))
	if strings.Count(got, "\n") != 1 || strings.Contains(got, "\x1b") {
		t.Fatalf("sql line not escaped: %q", got)
	}
	got = prettyLine(t, PrettyOptions{}, slog.LevelInfo, "job succeeded", jobAttrs("succeeded\n")...)
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("job line not escaped: %q", got)
	}
}

// fixedTime stamps every record with prettyTime, as slog.Logger would stamp
// it with the wall clock.
type fixedTime struct{ slog.Handler }

func (h fixedTime) Handle(ctx context.Context, record slog.Record) error {
	record.Time = prettyTime
	return h.Handler.Handle(ctx, record)
}

func (h fixedTime) WithAttrs(attrs []slog.Attr) slog.Handler {
	return fixedTime{h.Handler.WithAttrs(attrs)}
}
func (h fixedTime) WithGroup(name string) slog.Handler { return fixedTime{h.Handler.WithGroup(name)} }

func TestPrettyGroupsAndLoggerAttributes(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(fixedTime{NewPrettyHandler(&out, PrettyOptions{})})
	logger.With("request_id", "r1").WithGroup("db").With("pool", "main").WithGroup("").Info("opened",
		slog.Group("conn", slog.Int("n", 2), slog.Group("empty")), slog.Group("", slog.String("inline", "x")), slog.Attr{}, slog.Any("lazy", lazy{}))
	want := "12:04:05.123  INFO  opened  request_id=r1 db.pool=main db.conn.n=2 db.inline=x db.lazy=resolved\n"
	if out.String() != want {
		t.Fatalf("%q want %q", out.String(), want)
	}
	// With no attributes the handler is reused; an empty group adds none.
	handler := NewPrettyHandler(&out, PrettyOptions{})
	if empty := handler.WithAttrs([]slog.Attr{slog.Group("empty")}); empty.(*prettyHandler).attrs != nil {
		t.Fatal("an empty group adds nothing")
	}
	if handler.WithAttrs(nil) != handler || handler.WithGroup("") != handler {
		t.Fatal("empty additions must return the handler")
	}
	// An HTTP line built with the logger's attributes: the request ID of
	// the request logger reaches the line.
	out.Reset()
	logger.With("request_id", "r2").LogAttrs(context.Background(), slog.LevelInfo, LogEventHTTP, httpAttrs(201)...)
	if want := "12:04:05.123  HTTP  GET /posts/{id}  201  4.1ms  req=r2\n"; out.String() != want {
		t.Fatalf("%q want %q", out.String(), want)
	}
}

type lazy struct{}

func (lazy) LogValue() slog.Value { return slog.StringValue("resolved") }

func TestPrettyLevelAndClock(t *testing.T) {
	var out bytes.Buffer
	handler := NewPrettyHandler(&out, PrettyOptions{Now: func() time.Time { return prettyTime }})
	if handler.Enabled(context.Background(), slog.LevelDebug) || !handler.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("the default level is Info")
	}
	if !NewPrettyHandler(&out, PrettyOptions{Level: slog.LevelDebug}).Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("Level is honored")
	}
	// A record without a time takes the injected clock's.
	if err := handler.Handle(context.Background(), slog.Record{Level: slog.LevelInfo, Message: "m"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "12:04:05.123  INFO  m\n" {
		t.Fatalf("%q", out.String())
	}
	// The default clock is the wall clock.
	out.Reset()
	if err := NewPrettyHandler(&out, PrettyOptions{}).Handle(context.Background(), slog.Record{Level: slog.LevelInfo, Message: "m"}); err != nil || len(out.String()) != len("12:04:05.123  INFO  m\n") {
		t.Fatalf("%v %q", err, out.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestPrettyReportsWriteErrors(t *testing.T) {
	record := slog.NewRecord(prettyTime, slog.LevelInfo, "m", 0)
	if err := NewPrettyHandler(failingWriter{}, PrettyOptions{}).Handle(context.Background(), record); err == nil {
		t.Fatal("write error swallowed")
	}
}

func TestPrettyReplaceAttrAppliesEverywhere(t *testing.T) {
	var out bytes.Buffer
	replace := func(groups []string, a slog.Attr) slog.Attr {
		switch a.Key {
		case slog.MessageKey:
			return slog.String(a.Key, strings.ToUpper(a.Value.String()))
		case "drop":
			return slog.Attr{}
		case "pair":
			return slog.Group("pair", slog.Int("a", 1), slog.Int("b", 2))
		}
		return slog.String(a.Key, strings.Join(groups, "/")+":"+a.Value.String())
	}
	logger := slog.New(fixedTime{NewPrettyHandler(&out, PrettyOptions{ReplaceAttr: replace})})
	logger.With("w", "1").WithGroup("g").Info("hello", "x", "2", "drop", "z", "pair", "p")
	want := "12:04:05.123  INFO  HELLO  w=:1 g.x=g:2 g.pair.a=1 g.pair.b=2\n"
	if out.String() != want {
		t.Fatalf("%q want %q", out.String(), want)
	}
}

func TestPrettyHandlerRedactsAndReportsItself(t *testing.T) {
	var out bytes.Buffer
	handler := NewPrettyOutputHandler(&out, PrettyOptions{ReplaceAttr: Redactor()})
	reporter, ok := handler.(LogOutputHandler)
	if !ok {
		t.Fatal("the pretty output handler must report its output")
	}
	if got := reporter.LogOutput(); got.Format != LogFormatText || got.ReplaceAttr == nil {
		t.Fatalf("LogOutput %+v", got)
	}
	slog.New(handler).Info("m", "user", slog.GroupValue(slog.String("email", "a@b.c"), slog.String("name", "Ana")))
	if want := "  INFO  m  user.email=[REDACTED] user.name=Ana\n"; !strings.HasSuffix(out.String(), want) || strings.Contains(out.String(), "a@b.c") {
		t.Fatalf("%q want %q", out.String(), want)
	}
}

func TestNewOutputHandlerPretty(t *testing.T) {
	var out bytes.Buffer
	handler := NewOutputHandler(&out, LogFormatPretty, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: Redactor()})
	slog.New(handler).Debug("m", "token", "t0k")
	if !strings.Contains(out.String(), "DBG   m  token=[REDACTED]") || strings.Contains(out.String(), "t0k") || strings.Contains(out.String(), "\x1b") {
		t.Fatalf("%q", out.String())
	}
	if got := logOutputOf(handler); got.Format != LogFormatText || got.ReplaceAttr == nil {
		t.Fatalf("LogOutput %+v", got)
	}
	out.Reset()
	slog.New(NewOutputHandler(&out, LogFormatPretty, nil)).Info("plain")
	if !strings.HasSuffix(out.String(), "  INFO  plain\n") {
		t.Fatalf("%q", out.String())
	}
	// Records of a traced context carry the trace, as in the other formats.
	tel := newTestTelemetry(t, Options{SampleRatio: 1})
	ctx, span := tel.Tracer("x").Start(context.Background(), "op", SpanKindInternal)
	defer span.End()
	out.Reset()
	slog.New(NewOutputHandler(&out, LogFormatPretty, nil)).InfoContext(ctx, "traced")
	if !strings.Contains(out.String(), "trace_id="+span.SpanContext().TraceID.String()) {
		t.Fatalf("%q", out.String())
	}
}

// The 8-bit C1 controls (CSI U+009B, OSC U+009D, ST U+009C), bidirectional
// overrides and invalid UTF-8 are escaped like the C0 ones: a terminal
// obeys them, and a request path can carry them.
func TestPrettyEscapesC1BidiAndInvalidUTF8(t *testing.T) {
	hostile := map[string]string{
		"CSI":     "\u009b2J",
		"OSC":     "\u009d0;PWNED\u009c",
		"RLO":     "a\u202eevil",
		"LRI":     "a\u2066b",
		"ZWSP":    "a\u200bb",
		"NBSP":    "a\u00a0b",
		"LS":      "a\u2028b",
		"astral":  "a\U000e0001b",
		"invalid": "a\xffb\xc3",
	}
	forbidden := []string{"\u009b", "\u009d", "\u009c", "\u202e", "\u2066", "\u200b", "\u00a0", "\u2028", "\U000e0001", "\xff", "\xc3("}
	check := func(what, got string) {
		t.Helper()
		if strings.Count(got, "\n") != 1 {
			t.Errorf("%s: more than one line: %q", what, got)
		}
		if !utf8Valid(got) {
			t.Errorf("%s: invalid UTF-8 written: %q", what, got)
		}
		for _, bad := range forbidden {
			if strings.Contains(got, bad) {
				t.Errorf("%s: raw %q reached the output: %q", what, bad, got)
			}
		}
	}
	for name, text := range hostile {
		check(name+" message", prettyLine(t, PrettyOptions{Color: true}, slog.LevelWarn, text, slog.String("k", "v")))
		check(name+" key", prettyLine(t, PrettyOptions{}, slog.LevelInfo, "m", slog.String(text, "v")))
		check(name+" value", prettyLine(t, PrettyOptions{}, slog.LevelInfo, "m", slog.String("k", text)))
		check(name+" any", prettyLine(t, PrettyOptions{}, slog.LevelInfo, "m", slog.Any("k", errors.New(text))))
		check(name+" http", prettyLine(t, PrettyOptions{}, slog.LevelInfo, LogEventHTTP,
			slog.String("method", text), slog.String("route", text), slog.Int("status", 200), slog.Duration("duration", time.Millisecond), slog.String("request_id", text)))
		check(name+" sql", prettyLine(t, PrettyOptions{}, slog.LevelInfo, LogEventSQL,
			slog.String("statement", text), slog.Duration("duration", time.Millisecond), slog.String("error", text)))
		check(name+" job", prettyLine(t, PrettyOptions{}, slog.LevelError, "job", jobAttrs(text, slog.String("error", text))...))
	}
	// The exact escapes.
	got := prettyLine(t, PrettyOptions{}, slog.LevelWarn, "/\u009b2J\u009d0;PWNED\u009c"+string(rune(0x202e))+"evil\xff", slog.String("k", "\u009b"))
	want := `12:04:05.123  WARN  /\u009b2J\u009d0;PWNED\u009c\u202eevil\xff  k="\u009b"` + "\n"
	if got != want {
		t.Fatalf("%q want %q", got, want)
	}
	// Printable text, accented, CJK and emoji, is left alone.
	got = prettyLine(t, PrettyOptions{}, slog.LevelInfo, "ação 日本語 😀 ünï")
	if got != "12:04:05.123  INFO  ação 日本語 😀 ünï\n" {
		t.Fatalf("%q", got)
	}
	// A literal U+FFFD is escaped byte by byte too.
	if got := clean("a\ufffdb"); got != `a\xef\xbf\xbdb` {
		t.Fatalf("%q", got)
	}
}

func utf8Valid(s string) bool { return utf8.ValidString(s) }

// A logger under WithGroup writes g.trace_id; the event lines still hide it.
func TestPrettyHidesTraceIDsInGroups(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(fixedTime{NewPrettyHandler(&out, PrettyOptions{})}).WithGroup("g").With(LogTraceIDKey, "t1", LogSpanIDKey, "s1", "request_id", "r1")
	logger.LogAttrs(context.Background(), slog.LevelInfo, LogEventSQL, slog.String("statement", "SELECT 1"), slog.Duration("duration", time.Millisecond))
	// Grouped attributes are not the event's own: it is a plain line, with the
	// correlation IDs shown.
	if !strings.Contains(out.String(), "g.trace_id=t1") {
		t.Fatalf("%q", out.String())
	}
	got := prettyLine(t, PrettyOptions{}, slog.LevelInfo, LogEventHTTP,
		slog.String("method", "GET"), slog.Int("status", 200), slog.Duration("duration", time.Millisecond),
		slog.String("g.trace_id", "t1"), slog.String("x.span_id", "s1"))
	if strings.Contains(got, "trace_id") || strings.Contains(got, "span_id") {
		t.Fatalf("%q", got)
	}
	if kept := drop([]prettyAttr{{key: "g.component"}, {key: "component"}, {key: "g.other"}}, "component"); len(kept) != 1 || kept[0].key != "g.other" {
		t.Fatalf("%v", kept)
	}
}
