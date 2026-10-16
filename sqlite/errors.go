package sqlite

import (
	"errors"
	"strings"

	driver "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/rocketinsidecorp/forge-go/sqldb"
)

// Markers SQLite puts before the constraint in its error messages, for
// example "UNIQUE constraint failed: tasks.title" and
// "CHECK constraint failed: tasks_title_length".
const (
	uniqueMarker   = "UNIQUE constraint failed: "
	checkMarker    = "CHECK constraint failed: "
	datatypeMarker = " column "
)

// ErrorCode returns the SQLite extended result code in err's chain, such as
// 2067 (SQLITE_CONSTRAINT_UNIQUE), or 0. The primary code is ErrorCode(err) &
// 0xff. See https://www.sqlite.org/rescode.html.
func ErrorCode(err error) int {
	var sqliteErr *driver.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code()
	}
	return 0
}

// ConstraintName returns what SQLite reports about the violated constraint
// in err's chain, or "": the "table.column" list of a UNIQUE or PRIMARY KEY
// violation (for example "tasks.title"), the name of a named CHECK constraint
// (the expression of an unnamed one), or the "table.column" of a STRICT type
// mismatch. SQLite does not name the foreign key that failed. Use it to turn
// a specific violation into a domain error.
func ConstraintName(err error) string {
	var sqliteErr *driver.Error
	if !errors.As(err, &sqliteErr) {
		return ""
	}
	return constraintName(sqliteErr.Code(), sqliteErr.Error())
}

func constraintName(code int, message string) string {
	switch code {
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
		return detailAfter(message, strings.Index(message, uniqueMarker), len(uniqueMarker))
	case sqlite3.SQLITE_CONSTRAINT_CHECK:
		return detailAfter(message, strings.Index(message, checkMarker), len(checkMarker))
	case sqlite3.SQLITE_CONSTRAINT_DATATYPE:
		return detailAfter(message, strings.LastIndex(message, datatypeMarker), len(datatypeMarker))
	}
	return ""
}

// detailAfter returns the text after the marker at index, without the
// " (code)" suffix the driver appends.
func detailAfter(message string, index, markerLen int) string {
	if index < 0 {
		return ""
	}
	detail := message[index+markerLen:]
	if open := strings.LastIndex(detail, " ("); open >= 0 && isCodeSuffix(detail[open+2:]) {
		detail = detail[:open]
	}
	return strings.TrimSpace(detail)
}

// isCodeSuffix reports whether s is "<digits>)", optionally followed by
// " (SQLITE_BUSY)", as the driver formats result codes.
func isCodeSuffix(s string) bool {
	digits, rest, found := strings.Cut(s, ")")
	if !found || digits == "" || strings.Trim(digits, "0123456789") != "" {
		return false
	}
	return rest == "" || rest == " (SQLITE_BUSY)"
}

// Translate maps a database error to a *fault.Error that is safe to return to
// clients (see sqldb.Translate). The original error stays in the chain.
func Translate(err error) error { return sqldb.Translate(err, classify) }

// classify maps SQLite result codes to database-independent kinds.
func classify(err error) sqldb.Classification {
	var sqliteErr *driver.Error
	if !errors.As(err, &sqliteErr) {
		return sqldb.Classification{}
	}
	return classifyResult(sqliteErr.Code(), sqliteErr.Error())
}

// classifyResult is classify for an extended result code and the driver's
// message.
func classifyResult(code int, message string) sqldb.Classification {
	switch code {
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
		return sqldb.Classification{Kind: sqldb.KindUniqueViolation, Constraint: constraintName(code, message)}
	case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
		return sqldb.Classification{Kind: sqldb.KindForeignKeyViolation}
	case sqlite3.SQLITE_CONSTRAINT_CHECK, sqlite3.SQLITE_CONSTRAINT_DATATYPE:
		// A value the client sent: a CHECK constraint, or a value of the
		// wrong type for a STRICT table's column.
		return sqldb.Classification{Kind: sqldb.KindInvalidData, Constraint: constraintName(code, message)}
	case sqlite3.SQLITE_CONSTRAINT_NOTNULL:
		// A column the code forgot to set is a server bug, as in every
		// other adapter.
		return sqldb.Classification{}
	}
	switch code & 0xff {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		// The write lock stayed taken for longer than busy_timeout, or a
		// read snapshot went stale: retrying the transaction can succeed.
		return sqldb.Classification{Kind: sqldb.KindConflict}
	case sqlite3.SQLITE_INTERRUPT:
		// Only context cancellation interrupts statements; the driver
		// usually reports the context error itself.
		return sqldb.Classification{Kind: sqldb.KindTimeout}
	case sqlite3.SQLITE_FULL, sqlite3.SQLITE_IOERR, sqlite3.SQLITE_CANTOPEN:
		return sqldb.Classification{Kind: sqldb.KindUnavailable}
	}
	return sqldb.Classification{}
}
