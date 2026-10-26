package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Errors that are not a missing uuidv7() pass through unchanged, without a
// query on the session.
func TestExplainScriptErrorLeavesOtherErrorsAlone(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"not a PostgreSQL error":   errors.New("boom"),
		"another SQLSTATE":         &pgconn.PgError{Code: "42P01", Message: `relation "uuidv7" does not exist`},
		"another missing function": &pgconn.PgError{Code: "42883", Message: "function gen_uuid() does not exist"},
	} {
		if got := (dialect{}).ExplainScriptError(context.Background(), nil, err); got != err {
			t.Errorf("%s: got %v, want the error unchanged", name, got)
		}
	}
}
