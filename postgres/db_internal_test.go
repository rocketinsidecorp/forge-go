package postgres

import (
	"os"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
)

// pgx reads PGSSLMODE and the other TLS variables as defaults for what the
// URL leaves out. They must not decide the TLS of a URL that is parsed, and
// parsing must not touch the process environment.
func TestParseConfigIgnoresPGTLSVariables(t *testing.T) {
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGSSLROOTCERT", "/nonexistent/ca.pem")
	t.Setenv("PGSSLCERT", "/nonexistent/cert.pem")
	t.Setenv("PGSSLKEY", "/nonexistent/key.pem")

	database := func(raw string) config.Database {
		cfg := config.Default().Database
		cfg.URL = config.NewSecret(raw)
		return cfg
	}

	// No sslmode in the URL: libpq's default, prefer, not PGSSLMODE=disable.
	pool, err := parseConfig(database("postgres://app:pw@db.internal:5432/app"))
	if err != nil {
		t.Fatal(err)
	}
	conn := pool.ConnConfig.Config
	if conn.TLSConfig == nil || len(conn.Fallbacks) != 1 {
		t.Errorf("PGSSLMODE changed the mode: TLS=%v fallbacks=%d", conn.TLSConfig != nil, len(conn.Fallbacks))
	}

	// sslmode=require in the URL keeps TLS on every attempt, and the
	// unreadable PGSSLROOTCERT is not consulted.
	pool, err = parseConfig(database("postgres://app:pw@db.internal:5432/app?sslmode=require&pool_max_conns=3"))
	if err != nil {
		t.Fatalf("a PG* TLS variable changed the parse: %v", err)
	}
	if conn = pool.ConnConfig.Config; conn.TLSConfig == nil || len(conn.Fallbacks) != 0 {
		t.Errorf("sslmode=require lost TLS: TLS=%v fallbacks=%d", conn.TLSConfig != nil, len(conn.Fallbacks))
	}
	if pool.MaxConns != 3 {
		t.Errorf("pool parameters must survive: max conns %d", pool.MaxConns)
	}

	for _, name := range []string{"PGSSLMODE", "PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY"} {
		if os.Getenv(name) == "" {
			t.Errorf("%s was removed from the process environment", name)
		}
	}
}
