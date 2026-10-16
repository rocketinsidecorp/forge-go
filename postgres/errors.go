package postgres

import (
	"errors"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rocketinsidecorp/forge-go/sqldb"
)

// SQLSTATE codes Forge handles. See
// https://www.postgresql.org/docs/current/errcodes-appendix.html.
const (
	codeUniqueViolation      = "23505"
	codeForeignKeyViolation  = "23503"
	codeCheckViolation       = "23514"
	codeExclusionViolation   = "23P01"
	codeSerializationFailure = "40001"
	codeDeadlockDetected     = "40P01"
	codeQueryCanceled        = "57014"
	codeTooManyConnections   = "53300"
	codeAdminShutdown        = "57P01"
	codeCannotConnectNow     = "57P03"
)

// clientDataErrors are the violations a client can cause by sending bad
// values, so they are reported as 400. Other data exceptions (a NOT NULL
// column the code forgot, a failed cast, division by zero) are server bugs
// and fall through to a logged 500.
var clientDataErrors = map[string]bool{
	codeCheckViolation: true,
	"22001":            true, // string_data_right_truncation: value too long
	"22003":            true, // numeric_value_out_of_range
	"22007":            true, // invalid_datetime_format
	"22008":            true, // datetime_field_overflow
}

// SQLState returns the PostgreSQL error code in err's chain, or "".
func SQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// ConstraintName returns the violated constraint in err's chain, or "". Use it
// to turn a specific unique violation into a domain error, for example
// "tasks_title_key" into "a task with this title already exists".
func ConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// Translate maps a database error to a *fault.Error that is safe to return to
// clients (see sqldb.Translate). The original error stays in the chain.
func Translate(err error) error { return sqldb.Translate(err, classify) }

// classify maps pgx and PostgreSQL errors to database-independent kinds.
func classify(err error) sqldb.Classification {
	if errors.Is(err, pgx.ErrNoRows) {
		return sqldb.Classification{Kind: sqldb.KindNotFound}
	}
	// A connection that failed to open or died mid-query: the database or
	// the network is unavailable, as the other adapters report it.
	var connectErr *pgconn.ConnectError
	var netErr *net.OpError
	if errors.As(err, &connectErr) || errors.As(err, &netErr) || errors.Is(err, pgconn.ErrConnClosed) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || pgconn.SafeToRetry(err) {
		return sqldb.Classification{Kind: sqldb.KindUnavailable}
	}
	code, constraint := SQLState(err), ConstraintName(err)
	switch {
	case code == codeUniqueViolation:
		return sqldb.Classification{Kind: sqldb.KindUniqueViolation, Constraint: constraint}
	case code == codeForeignKeyViolation:
		return sqldb.Classification{Kind: sqldb.KindForeignKeyViolation, Constraint: constraint}
	case code == codeExclusionViolation:
		return sqldb.Classification{Kind: sqldb.KindExclusionViolation, Constraint: constraint}
	case clientDataErrors[code]:
		return sqldb.Classification{Kind: sqldb.KindInvalidData, Constraint: constraint}
	case code == codeSerializationFailure || code == codeDeadlockDetected:
		return sqldb.Classification{Kind: sqldb.KindConflict}
	case code == codeQueryCanceled:
		return sqldb.Classification{Kind: sqldb.KindTimeout}
	case code == codeTooManyConnections || code == codeAdminShutdown || code == codeCannotConnectNow || strings.HasPrefix(code, "08"):
		// Class 08 is connection exceptions.
		return sqldb.Classification{Kind: sqldb.KindUnavailable}
	}
	return sqldb.Classification{}
}
