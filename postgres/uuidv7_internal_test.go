package postgres

import (
	"context"
	"errors"
	"strings"
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
		"a name containing uuidv7": &pgconn.PgError{Code: "42883", Message: "function my_uuidv7_helper() does not exist"},
		"a qualified look-alike":   &pgconn.PgError{Code: "42883", Message: "function app.uuidv7_text(uuid) does not exist"},
	} {
		if got := (dialect{}).ExplainScriptError(context.Background(), nil, err); got != err {
			t.Errorf("%s: got %v, want the error unchanged", name, got)
		}
	}
}

func TestUUIDv7HintNamesOnlyServersWithoutIt(t *testing.T) {
	t.Parallel()
	for _, message := range []string{"function uuidv7() does not exist", "function public.uuidv7(text, integer) does not exist"} {
		if !missingUUIDv7(&pgconn.PgError{Code: "42883", Message: message}) {
			t.Errorf("%q is not recognized as a missing uuidv7()", message)
		}
	}
	err := &pgconn.PgError{Code: "42883", Message: "function uuidv7() does not exist"}
	if got := uuidv7Hint(err, 180000, "18.0"); got != err {
		t.Errorf("PostgreSQL 18: got %v, want the error unchanged", got)
	}
	got := uuidv7Hint(err, 160015, "16.15")
	var pgErr *pgconn.PgError
	if !errors.As(got, &pgErr) || !strings.Contains(got.Error(), "this server is PostgreSQL 16.15: uuidv7() needs PostgreSQL 18") {
		t.Errorf("PostgreSQL 16: got %v, want the hint wrapping the PostgreSQL error", got)
	}
}
