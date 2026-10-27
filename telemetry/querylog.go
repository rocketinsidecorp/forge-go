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

// Log writes the record of one statement of the database system (a
// Dialect constant, which picks the literal rules) that took duration and ended with
// errorType ("" for success; a class such as "unique_violation", never the
// error's text, which could quote a value). A nil QueryLog does nothing.
func (q *QueryLog) Log(ctx context.Context, system, statement string, duration time.Duration, errorType string) {
	if q == nil || q.Logger == nil || !q.Logger.Enabled(ctx, q.Level) {
		return
	}
	attrs := make([]slog.Attr, 0, 5)
	attrs = append(attrs, slog.String("statement", SanitizeStatementFor(system, statement)), slog.Duration("duration", duration))
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

// SQL dialect names SanitizeStatementFor knows: the db.system.name of the
// database adapters. Any other name gets the cautious rules of an unknown
// dialect.
const (
	DialectPostgreSQL = "postgresql"
	DialectMySQL      = "mysql" // MySQL and MariaDB
	DialectSQLite     = "sqlite"
)

// backslashMode says whether a backslash escapes the next byte in a string.
type backslashMode uint8

const (
	// backslashNever: a backslash is an ordinary character (PostgreSQL with
	// standard_conforming_strings, SQLite).
	backslashNever backslashMode = iota
	// backslashAlways: it escapes (MySQL, MariaDB).
	backslashAlways
	// backslashAfterE: it escapes only in an E'...' string (PostgreSQL).
	backslashAfterE
	// backslashUnknown: the dialect is not known, so a backslash in a string
	// cannot be told from an escape; the rest of the statement is masked.
	backslashUnknown
)

// dialect is the lexical rules SanitizeStatementFor needs.
type dialect struct {
	backslash backslashMode
	// doubleQuoteLiteral: "..." is a string, not an identifier (MySQL
	// without ANSI_QUOTES, which Forge's sql_mode does not set).
	doubleQuoteLiteral bool
	// hashComment: # starts a line comment (MySQL).
	hashComment bool
	// dashNeedsSpace: -- starts a comment only before whitespace (MySQL).
	dashNeedsSpace bool
	// nestedComments: /* */ nest (PostgreSQL).
	nestedComments bool
	// dollarQuote: $tag$...$tag$ is a string (PostgreSQL).
	dollarQuote bool
}

func dialectOf(system string) dialect {
	switch system {
	case DialectPostgreSQL:
		return dialect{backslash: backslashAfterE, nestedComments: true, dollarQuote: true}
	case DialectMySQL, "mariadb":
		return dialect{backslash: backslashAlways, doubleQuoteLiteral: true, hashComment: true, dashNeedsSpace: true}
	case DialectSQLite:
		return dialect{backslash: backslashNever}
	}
	// Unknown: the union of what could hide a literal.
	return dialect{backslash: backslashUnknown, doubleQuoteLiteral: true, hashComment: true, dollarQuote: true}
}

// SanitizeStatement is SanitizeStatementFor an unknown dialect.
func SanitizeStatement(statement string) string { return SanitizeStatementFor("", statement) }

// SanitizeStatementFor returns statement in one line for a log: whitespace
// collapsed, comments dropped, string literals masked as '?', numeric
// literals as ?, and the result cut at MaxLoggedStatement runes. Parameter
// placeholders ($1, ?, :name) stay, and so do quoted identifiers. The lexical
// rules follow the dialect (DialectPostgreSQL, DialectMySQL, DialectSQLite):
// whether "..." is a string or an identifier, whether a backslash escapes,
// which comments exist. Statements built by string concatenation would
// otherwise write their values, so it fails closed: an unterminated string,
// quoted identifier or dollar quote masks the rest of the statement, an
// unterminated comment drops it, and a backslash in a string of an unknown
// dialect masks the rest.
func SanitizeStatementFor(system, statement string) string {
	d := dialectOf(system)
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
		case c == '-' && strings.HasPrefix(statement[i:], "--") && (!d.dashNeedsSpace || dashComment(statement, i)):
			space = true
			i = endOfLine(statement, i)
		case c == '#' && d.hashComment:
			space = true
			i = endOfLine(statement, i)
		case c == '/' && strings.HasPrefix(statement[i:], "/*"):
			space = true
			i = skipBlockComment(statement, i, d.nestedComments)
		case c == '\'':
			emit("'?'")
			i = skipQuoted(statement, i, d.escapes(statement, i), d.backslash == backslashUnknown)
		case c == '"' && d.doubleQuoteLiteral:
			emit(`"?"`)
			i = skipQuoted(statement, i, d.backslash != backslashNever, d.backslash == backslashUnknown)
		case c == '$' && d.dollarQuote && dollarTagEnd(statement, i) > 0:
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
			// An identifier: kept, it names a table or a column. One that
			// is not closed is not trusted to be one: the rest is masked.
			end := strings.IndexByte(statement[i+1:], c)
			if end < 0 {
				emit(string(c) + "?")
				i = len(statement)
				break
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

// escapes reports whether the single-quoted string that starts at i treats a
// backslash as an escape.
func (d dialect) escapes(s string, i int) bool {
	switch d.backslash {
	case backslashAlways, backslashUnknown:
		return true
	case backslashAfterE:
		// E'...' (or e'...'): the E must be a token of its own.
		return i > 0 && (s[i-1] == 'E' || s[i-1] == 'e') && (i == 1 || !isIdentifierByte(s[i-2]))
	}
	return false
}

func isIdentifierByte(c byte) bool {
	return isLetter(c) || isDigit(c) || c == '_' || c == '$' || c >= utf8.RuneSelf
}

// dashComment reports whether the -- at i starts a MySQL comment: it must be
// followed by whitespace, a control character or the end.
func dashComment(s string, i int) bool {
	return i+2 >= len(s) || s[i+2] <= ' '
}

// endOfLine returns the index of the newline that ends the line of i, or the
// end of s.
func endOfLine(s string, i int) int {
	if end := strings.IndexByte(s[i:], '\n'); end >= 0 {
		return i + end
	}
	return len(s)
}

// skipBlockComment returns the index after the comment that starts at i, or
// the end of s when it is not closed.
func skipBlockComment(s string, i int, nested bool) int {
	depth := 0
	for i < len(s) {
		switch {
		case strings.HasPrefix(s[i:], "/*") && (nested || depth == 0):
			depth++
			i += 2
		case strings.HasPrefix(s[i:], "*/"):
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return len(s)
}

// skipQuoted returns the index after the quoted string that starts at i. A
// doubled quote is one quote; with escapes a backslash escapes the next byte.
// A string that is not closed runs to the end, so what follows an unbalanced
// quote is masked too; with maskOnBackslash a backslash does the same, as
// in a dialect where it may or may not be an escape.
func skipQuoted(s string, i int, escapes, maskOnBackslash bool) int {
	quote := s[i]
	for i++; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if maskOnBackslash {
				return len(s)
			}
			if escapes {
				i++
			}
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
