package postgres_test

import (
	"errors"
	"maps"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/postgres"
)

// adminKeys is what the forge db commands allow (internal/dbtasks).
var adminKeys = []string{"host", "port", "database", "user", "password", "connect_timeout", "sslmode", "sslrootcert"}

// An empty or nil list of allowed parameters is refused: Isolate skips its
// key check for nil, which would let service, servicefile and options in.
func TestIsolatedConnConfigRefusesAMissingAllowList(t *testing.T) {
	t.Parallel()
	for name, keys := range map[string][]string{"nil": nil, "empty": {}} {
		if cfg, err := postgres.IsolatedConnConfig("postgres://u:p@127.0.0.1:5432/app?servicefile=/tmp/evil", keys); err == nil || cfg != nil {
			t.Errorf("%s allow list: (%v, %v), want an error", name, cfg, err)
		}
	}
}

// Parameters that pick another server, session or service are refused
// unless the caller allows them.
func TestIsolatedConnConfigRefusesParametersNotAllowed(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"postgres://u:p@127.0.0.1:5432/app?servicefile=/tmp/evil",
		"postgres://u:p@127.0.0.1:5432/app?service=other",
		"postgres://u:p@127.0.0.1:5432/app?options=-c%20search_path%3Devil",
		"postgres://u:p@127.0.0.1:5432/app?hostaddr=10.0.0.1",
	} {
		if cfg, err := postgres.IsolatedConnConfig(raw, adminKeys); err == nil || cfg != nil {
			t.Errorf("%s: (%v, %v), want an error", raw, cfg, err)
		}
	}
	if _, err := postgres.IsolatedConnConfig("postgres://u:p@127.0.0.1:5432/app?sslmode=disable", adminKeys); err != nil {
		t.Fatalf("an allowed URL was refused: %v", err)
	}
}

// essentials is what IsolatedConnConfig's result decides about a connection.
func essentials(cfg *pgx.ConnConfig) map[string]any {
	return map[string]any{
		"host": cfg.Host, "port": cfg.Port, "database": cfg.Database, "user": cfg.User,
		"tls": cfg.TLSConfig != nil, "fallbacks": len(cfg.Fallbacks),
		"runtime": maps.Clone(cfg.RuntimeParams), "timeout": cfg.ConnectTimeout,
	}
}

// The same URL gives the same configuration with and without PG* variables
// in the environment, and a TLS file variable the URL does not name is
// refused rather than ignored or applied.
func TestIsolatedConnConfigIgnoresThePGEnvironment(t *testing.T) {
	const raw = "postgres://u:p@127.0.0.1:5432/app"
	for _, key := range []string{"PGSSLMODE", "PGOPTIONS", "PGAPPNAME", "PGTZ", "PGTARGETSESSIONATTRS", "PGCONNECT_TIMEOUT", "PGUSER", "PGSERVICE", "PGSSLROOTCERT"} {
		t.Setenv(key, "")
	}
	clean, err := postgres.IsolatedConnConfig(raw, adminKeys)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGOPTIONS", "-c forge.tenant_id=00000000-0000-7000-8000-000000000001")
	t.Setenv("PGAPPNAME", "intruder")
	t.Setenv("PGTZ", "Pacific/Apia")
	t.Setenv("PGTARGETSESSIONATTRS", "read-only")
	t.Setenv("PGCONNECT_TIMEOUT", "1")
	t.Setenv("PGUSER", "someone_else")
	dirty, err := postgres.IsolatedConnConfig(raw, adminKeys)
	if err != nil {
		t.Fatal(err)
	}
	want, got := essentials(clean), essentials(dirty)
	for key := range want {
		if !equalValue(want[key], got[key]) {
			t.Errorf("%s with PG* set: %v, without: %v", key, got[key], want[key])
		}
	}
	t.Setenv("PGSSLROOTCERT", "/tmp/somebody-elses-ca.pem")
	var refused *postgres.URLRefusedError
	if _, err := postgres.IsolatedConnConfig(raw, adminKeys); !errors.As(err, &refused) {
		t.Fatalf("PGSSLROOTCERT not named by the URL: %v, want a *URLRefusedError", err)
	}
}

func equalValue(a, b any) bool {
	if left, ok := a.(map[string]string); ok {
		right, _ := b.(map[string]string)
		return maps.Equal(left, right)
	}
	return a == b
}
