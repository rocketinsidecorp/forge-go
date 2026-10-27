package telemetry

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/rocketinsidecorp/forge-go/web"
)

// Names of the log events that follow a request, a statement and a job, and
// that the pretty format lays out as HTTP, SQL and JOB lines. They are the
// same in every format, so a production dashboard filters on them.
const (
	LogEventHTTP = "http request"
	LogEventSQL  = "sql query"
)

// MaxLoggedStatement is the most runes of a statement that a SQL line
// carries; a longer one ends with an ellipsis.
const MaxLoggedStatement = 240

// QueryLog says how database statements are logged (see Options.QueryLog).
// The statement text is logged, never its arguments: a statement that
// the application built with literals has them masked (SanitizeStatement).
type QueryLog struct {
	// Logger receives one record per statement. It must write to standard
	// output only: records are not meant for the log exporter.
	Logger *slog.Logger
	// Level is the level of the records: Info in development, Debug
	// elsewhere (config.Config.EventLevel).
	Level slog.Level
	// Slow is the duration above which a record carries slow=true; zero
	// never flags.
	Slow time.Duration
}

// Log writes the record of one statement that took duration and ended with
// errorType ("" for success; a class such as "unique_violation", never the
// error's text, which could quote a value). A nil QueryLog does nothing.
func (q *QueryLog) Log(ctx context.Context, statement string, duration time.Duration, errorType string) {
	if q == nil || q.Logger == nil || !q.Logger.Enabled(ctx, q.Level) {
		return
	}
	attrs := make([]slog.Attr, 0, 5)
	attrs = append(attrs, slog.String("statement", SanitizeStatement(statement)), slog.Duration("duration", duration))
	if q.Slow > 0 && duration > q.Slow {
		attrs = append(attrs, slog.Bool("slow", true))
	}
	if errorType != "" {
		attrs = append(attrs, slog.String("error", errorType))
	}
	if id := web.RequestID(ctx); id != "" {
		attrs = append(attrs, slog.String("request_id", id))
	}
	q.Logger.LogAttrs(ctx, q.Level, LogEventSQL, attrs...)
}

// QueryLog returns the statement logging t was created with, or nil.
func (t *Telemetry) QueryLog() *QueryLog {
	if t == nil {
		return nil
	}
	return t.queryLog
}

type queryCounterKey struct{}

// QueryCounter counts the statements run with a context. The HTTP access log
// installs one per request so its line can say how many queries the request
// made, the number to watch for an N+1.
type QueryCounter struct{ n atomic.Int64 }

// WithQueryCounter returns ctx carrying a new counter, and the counter.
func WithQueryCounter(ctx context.Context) (context.Context, *QueryCounter) {
	counter := &QueryCounter{}
	return context.WithValue(ctx, queryCounterKey{}, counter), counter
}

// CountQuery counts one statement in the counter ctx carries, if any. It
// does not allocate.
func CountQuery(ctx context.Context) {
	if counter, ok := ctx.Value(queryCounterKey{}).(*QueryCounter); ok {
		counter.n.Add(1)
	}
}

// Count returns the statements counted so far.
func (c *QueryCounter) Count() int { return int(c.n.Load()) }

// SanitizeStatement returns statement in one line for a log: whitespace
// collapsed, comments dropped, string literals masked as '?', numeric
// literals as ?, and the result cut at MaxLoggedStatement runes. Parameter
// placeholders ($1, ?, :name) stay. Statements built by string concatenation
// would otherwise write their values; a literal that cannot be told from
// syntax is masked rather than shown.
func SanitizeStatement(statement string) string {
	var out strings.Builder
	out.Grow(min(len(statement), 2*MaxLoggedStatement))
	space := false
	emit := func(s string) {
		if space && out.Len() > 0 {
			out.WriteByte(' ')
		}
		space = false
		out.WriteString(s)
	}
	for i := 0; i < len(statement) && out.Len() < 4*MaxLoggedStatement; {
		c := statement[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			space = true
			i++
		case c == '-' && strings.HasPrefix(statement[i:], "--"):
			space = true
			if end := strings.IndexByte(statement[i:], '\n'); end >= 0 {
				i += end
			} else {
				i = len(statement)
			}
		case c == '/' && strings.HasPrefix(statement[i:], "/*"):
			space = true
			if end := strings.Index(statement[i+2:], "*/"); end >= 0 {
				i += end + 4
			} else {
				i = len(statement)
			}
		case c == '\'':
			emit("'?'")
			i = skipQuoted(statement, i)
		case c == '$' && dollarTagEnd(statement, i) > 0:
			tag := statement[i:dollarTagEnd(statement, i)]
			emit("$$?$$")
			if end := strings.Index(statement[i+len(tag):], tag); end >= 0 {
				i += len(tag) + end + len(tag)
			} else {
				i = len(statement)
			}
		case c == '$':
			// A placeholder ($1): it names an argument, never carries one.
			j := i + 1
			for j < len(statement) && isDigit(statement[j]) {
				j++
			}
			emit(statement[i:j])
			i = j
		case c == '"' || c == '`':
			// An identifier: kept, it names a table or a column.
			end := strings.IndexByte(statement[i+1:], c)
			if end < 0 {
				end = len(statement) - i - 2
			}
			emit(statement[i : i+end+2])
			i += end + 2
		case isDigit(c):
			j := i
			for j < len(statement) && (isDigit(statement[j]) || isLetter(statement[j]) || statement[j] == '_' || statement[j] == '.') {
				j++
			}
			emit("?")
			i = j
		case isIdentifierStart(c):
			// A word, with the digits it may hold (t1, col_2) and the
			// $ of Postgres and MySQL identifiers.
			j := i
			for j < len(statement) && (isDigit(statement[j]) || isLetter(statement[j]) || statement[j] == '_' || statement[j] == '$' || statement[j] >= utf8.RuneSelf) {
				j++
			}
			emit(statement[i:j])
			i = j
		default:
			// Punctuation, operators, placeholders; one rune at a time so
			// multi-byte text is not cut.
			_, size := utf8.DecodeRuneInString(statement[i:])
			emit(statement[i : i+size])
			i += size
		}
	}
	return truncateRunes(out.String(), MaxLoggedStatement)
}

// skipQuoted returns the index after the quoted string that starts at i. A
// backslash escapes the next byte (MySQL, PostgreSQL E-strings), a doubled quote is
// one quote; an unterminated string runs to the end, so what follows an
// unbalanced quote is masked too.
func skipQuoted(s string, i int) int {
	quote := s[i]
	for i++; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case quote:
			if i+1 < len(s) && s[i+1] == quote {
				i++
				continue
			}
			return i + 1
		}
	}
	return len(s)
}

// dollarTagEnd returns the index after the dollar-quote tag ($$ or $name$)
// that starts at i, or 0 when there is none ($1 is a placeholder).
func dollarTagEnd(s string, i int) int {
	j := i + 1
	if j < len(s) && s[j] == '$' {
		return j + 1
	}
	if j >= len(s) || (!isLetter(s[j]) && s[j] != '_') {
		return 0
	}
	for j < len(s) && (isLetter(s[j]) || isDigit(s[j]) || s[j] == '_') {
		j++
	}
	if j < len(s) && s[j] == '$' {
		return j + 1
	}
	return 0
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isIdentifierStart(c byte) bool {
	return isLetter(c) || c == '_' || c >= utf8.RuneSelf
}

// truncateRunes cuts s to at most n runes, ending with an ellipsis when it
// was longer.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n-1]) + "…"
}
