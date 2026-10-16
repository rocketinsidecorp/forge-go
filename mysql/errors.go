package mysql

import (
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"net"
	"strings"

	driver "github.com/go-sql-driver/mysql"

	"github.com/rocketinsidecorp/forge-go/sqldb"
)

// Server error numbers Forge handles. See
// https://dev.mysql.com/doc/mysql-errors/8.4/en/server-error-reference.html
// and https://mariadb.com/kb/en/mariadb-error-code-reference/.
const (
	errDuplicateEntry        = 1062 // ER_DUP_ENTRY
	errRowIsReferenced       = 1451 // ER_ROW_IS_REFERENCED_2: parent row still in use
	errNoReferencedRow       = 1452 // ER_NO_REFERENCED_ROW_2: parent row missing
	errCheckViolationMySQL   = 3819 // ER_CHECK_CONSTRAINT_VIOLATED
	errCheckViolationMariaDB = 4025 // ER_CONSTRAINT_FAILED
	errDataTooLong           = 1406 // ER_DATA_TOO_LONG
	errOutOfRange            = 1264 // ER_WARN_DATA_OUT_OF_RANGE
	errTruncatedWrongValue   = 1292 // ER_TRUNCATED_WRONG_VALUE: incorrect datetime or value
	errDeadlock              = 1213 // ER_LOCK_DEADLOCK
	errLockWaitTimeout       = 1205 // ER_LOCK_WAIT_TIMEOUT
	errQueryTimeoutMySQL     = 3024 // ER_QUERY_TIMEOUT: max_execution_time exceeded
	errQueryTimeoutMariaDB   = 1969 // ER_STATEMENT_TIMEOUT: max_statement_time exceeded
	errTooManyConnections    = 1040 // ER_CON_COUNT_ERROR
	errTooManyUserConns      = 1203 // ER_TOO_MANY_USER_CONNECTIONS
	errServerShutdown        = 1053 // ER_SERVER_SHUTDOWN
)

// ErrorNumber returns the MySQL or MariaDB error number in err's chain, or 0.
func ErrorNumber(err error) uint16 {
	var mysqlErr *driver.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number
	}
	return 0
}

// ConstraintName returns the violated key or constraint in err's chain, or "".
// The server reports it only inside the message, so it is parsed from there:
// MySQL 8 names a duplicate key "table.key", MariaDB just "key". Use it to
// turn a specific violation into a domain error, for example
// "tasks.tasks_title_key" into "a task with this title already exists".
func ConstraintName(err error) string {
	var mysqlErr *driver.MySQLError
	if !errors.As(err, &mysqlErr) {
		return ""
	}
	message := mysqlErr.Message
	switch mysqlErr.Number {
	case errDuplicateEntry:
		// Duplicate entry '<value>' for key '<key>'. The value is user data
		// and may itself contain the marker, so the key is the last one.
		const marker = "for key '"
		start := strings.LastIndex(message, marker)
		if start < 0 || !strings.HasSuffix(message, "'") || start+len(marker) > len(message)-1 {
			return ""
		}
		return message[start+len(marker) : len(message)-1]
	case errRowIsReferenced, errNoReferencedRow, errCheckViolationMariaDB:
		// ... CONSTRAINT `<name>` ...
		return between(message, "CONSTRAINT `", "`")
	case errCheckViolationMySQL:
		// Check constraint '<name>' is violated.
		return between(message, "Check constraint '", "' is violated")
	}
	return ""
}

// between returns the text after the first prefix and before the next
// suffix, or "".
func between(message, prefix, suffix string) string {
	_, rest, found := strings.Cut(message, prefix)
	if !found {
		return ""
	}
	name, _, found := strings.Cut(rest, suffix)
	if !found {
		return ""
	}
	return name
}

// Translate maps a database error to a *fault.Error that is safe to return to
// clients (see sqldb.Translate). The original error stays in the chain.
func Translate(err error) error { return sqldb.Translate(err, classify) }

// classify maps driver, network and server errors to database-independent
// kinds. Anything unrecognized, including a NOT NULL violation (1048), is
// KindUnknown: a server bug reported as a logged 500.
func classify(err error) sqldb.Classification {
	if errors.Is(err, sql.ErrNoRows) {
		return sqldb.Classification{Kind: sqldb.KindNotFound}
	}
	if errors.Is(err, driver.ErrInvalidConn) || errors.Is(err, sqldriver.ErrBadConn) {
		return sqldb.Classification{Kind: sqldb.KindUnavailable}
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return sqldb.Classification{Kind: sqldb.KindUnavailable}
	}
	switch ErrorNumber(err) {
	case errDuplicateEntry:
		return sqldb.Classification{Kind: sqldb.KindUniqueViolation, Constraint: ConstraintName(err)}
	case errRowIsReferenced, errNoReferencedRow:
		return sqldb.Classification{Kind: sqldb.KindForeignKeyViolation, Constraint: ConstraintName(err)}
	case errCheckViolationMySQL, errCheckViolationMariaDB, errDataTooLong, errOutOfRange, errTruncatedWrongValue:
		return sqldb.Classification{Kind: sqldb.KindInvalidData, Constraint: ConstraintName(err)}
	case errDeadlock, errLockWaitTimeout:
		return sqldb.Classification{Kind: sqldb.KindConflict}
	case errQueryTimeoutMySQL, errQueryTimeoutMariaDB:
		return sqldb.Classification{Kind: sqldb.KindTimeout}
	case errTooManyConnections, errTooManyUserConns, errServerShutdown:
		return sqldb.Classification{Kind: sqldb.KindUnavailable}
	}
	return sqldb.Classification{}
}
