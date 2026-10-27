package postgres

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rocketinsidecorp/forge-go/postgres/internal/pgurl"
)

// URLRefusedError is how IsolatedConnConfig refuses a URL it cannot make
// independent of the environment: one net/url and pgx would read
// differently, or a set PG* TLS file variable the URL does not name. Its
// message names variables and parameters, never a value or the URL, so it is
// safe to show.
type URLRefusedError = pgurl.RefusedError

// IsolatedConnConfig parses raw, a postgres:// or postgresql:// URL, with pgx
// without letting the process environment decide anything: pgx reads
// PGSSLMODE, PGOPTIONS, PGTARGETSESSIONATTRS, PGSERVICE, PGUSER and the other
// PG* variables as defaults for what the URL leaves unset and has no option
// to ignore them. The environment is not edited (that would tear it under
// every concurrent reader): the libpq default of each parameter the
// environment sets is written into the URL, which pgx prefers, a service
// named by PGSERVICE is replaced by an empty one, and the session settings
// the URL does not name (PGOPTIONS, PGTZ, PGAPPNAME) are removed from the
// result. Every query parameter of raw must be in allowedKeys (dbname is
// accepted wherever database is listed).
//
// A refusal is a *URLRefusedError. Any other error either says the URL is
// not valid or comes from pgx, whose parse errors can quote raw: callers
// must not show them as they are. The forge db commands use it for their
// administration connections.
func IsolatedConnConfig(raw string, allowedKeys []string) (*pgx.ConnConfig, error) {
	iso, err := pgurl.Isolate(raw, pgurl.Everything, allowedKeys)
	if err != nil {
		var refused *URLRefusedError
		if errors.As(err, &refused) {
			return nil, refused
		}
		return nil, errors.New("the PostgreSQL connection URL is not valid")
	}
	defer iso.Close()
	cfg, err := pgx.ParseConfigWithOptions(iso.URL, pgx.ParseConfigOptions{
		ParseConfigOptions: pgconn.ParseConfigOptions{ConnStringAllowedKeys: append(append([]string(nil), allowedKeys...), iso.PinnedKeys()...)},
	})
	if err != nil {
		return nil, err
	}
	iso.StripSession(cfg.RuntimeParams)
	return cfg, nil
}
