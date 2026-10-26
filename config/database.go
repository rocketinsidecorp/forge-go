package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// Adapter identifies the database adapter selected by FORGE_DATABASE_URL's
// scheme.
type Adapter string

// The database adapters.
const (
	AdapterPostgres Adapter = "postgres"
	// AdapterMySQL serves both MySQL and MariaDB.
	AdapterMySQL  Adapter = "mysql"
	AdapterSQLite Adapter = "sqlite"
)

const maxDatabaseConns = 1000

// verifiedSSLModes encrypt the connection and authenticate the server.
// "require" encrypts without checking the certificate, so it can be
// intercepted; libpq's default, "prefer", silently falls back to plaintext.
// Private CAs are configured with sslrootcert, which verify-ca requires in
// production: without it pgx trusts the system roots and checks no host name.
var verifiedSSLModes = map[string]bool{"verify-ca": true, "verify-full": true}

// Adapter reports the adapter the URL selects, or "" when no database is
// configured. The URL has been validated, so any other value cannot occur.
func (d Database) Adapter() Adapter {
	adapter, _ := parseDatabaseURL(d.URL.Reveal())
	return adapter
}

// SQLitePath returns the database file of a sqlite: URL, or "".
func (d Database) SQLitePath() string {
	parsed, err := url.Parse(d.URL.Reveal())
	if err != nil || parsed.Scheme != "sqlite" {
		return ""
	}
	if parsed.Opaque != "" {
		// sqlite:storage/app.db; decoded like the absolute form below.
		path, err := url.PathUnescape(parsed.Opaque)
		if err != nil {
			return ""
		}
		return path
	}
	return parsed.Path // the file of a URL such as sqlite:///var/lib/app/app.db
}

func parseDatabaseURL(raw string) (Adapter, *url.URL) {
	// Never surface url.Parse errors: they quote the URL, password included.
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", nil
	}
	switch parsed.Scheme {
	case "postgres", "postgresql":
		return AdapterPostgres, parsed
	case "mysql":
		return AdapterMySQL, parsed
	case "sqlite":
		return AdapterSQLite, parsed
	default:
		return "", nil
	}
}

// rejectAmbiguous refuses URLs that drivers may read differently from
// net/url, which validated them: a repeated parameter (net/url keeps the
// first, libpq-style parsers the last), an "@" after the host (pgx ends the
// userinfo at the last "@" before the path, even inside the query), a
// fragment, which net/url drops silently, and whitespace or a control
// character in the query (net/url keeps "& host=" as the key " host", pgx
// trims it to "host"; pgx refuses a raw space anywhere else, so no valid
// URL needs one). Any of these could make the connection differ from the one
// checked here, TLS mode included.
func rejectAmbiguous(parsed *url.URL) error {
	if parsed.Fragment != "" || strings.Contains(parsed.RawQuery, "@") || strings.Contains(parsed.Path, "@") {
		return errors.New("FORGE_DATABASE_URL is ambiguous: percent-encode \"@\" and \"#\" in passwords and parameters")
	}
	if strings.IndexFunc(parsed.RawQuery, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return errors.New("FORGE_DATABASE_URL is ambiguous: its parameters hold whitespace or a control character (percent-encode spaces as %20)")
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return errors.New("FORGE_DATABASE_URL has a malformed query string")
	}
	for _, values := range query {
		if len(values) > 1 {
			return errors.New("FORGE_DATABASE_URL repeats a parameter")
		}
	}
	return nil
}

func (d Database) validate(environment Environment) error {
	if d.MaxConns < 1 || d.MaxConns > maxDatabaseConns {
		return fmt.Errorf("FORGE_DATABASE_MAX_CONNS must be between 1 and %d", maxDatabaseConns)
	}
	if d.MinConns < 0 || d.MinConns > d.MaxConns {
		return errors.New("FORGE_DATABASE_MIN_CONNS must be between 0 and FORGE_DATABASE_MAX_CONNS")
	}
	if d.MaxConnLifetime <= 0 || d.MaxConnIdleTime <= 0 || d.ConnectTimeout <= 0 {
		return errors.New("database timeouts and lifetimes must be positive")
	}
	// Databases take milliseconds and treat 0 as "no timeout": a
	// sub-millisecond value would silently disable it.
	if d.StatementTimeout < time.Millisecond {
		return errors.New("FORGE_DATABASE_STATEMENT_TIMEOUT must be at least 1ms")
	}
	if d.URL.IsZero() {
		return nil
	}
	adapter, parsed := parseDatabaseURL(d.URL.Reveal())
	if parsed != nil {
		if err := rejectAmbiguous(parsed); err != nil {
			return err
		}
	}
	production := environment == Production && !d.AllowPlaintext
	switch adapter {
	case AdapterPostgres:
		if parsed.Host == "" || strings.Contains(parsed.Host, ",") {
			return errors.New("FORGE_DATABASE_URL must be postgres://user:password@host:port/database (one host)")
		}
		if production && !verifiedSSLModes[parsed.Query().Get("sslmode")] {
			return errors.New("production requires FORGE_DATABASE_URL with sslmode=verify-full or " +
				"verify-ca plus sslrootcert (FORGE_DATABASE_ALLOW_PLAINTEXT=true accepts unverified or " +
				"plaintext connections, only for a trusted private network)")
		}
		// verify-ca checks the chain only. Without sslrootcert pgx trusts the
		// system roots, and a certificate any public CA issued for any name
		// would pass; sslrootcert limits trust to the file named.
		if production && parsed.Query().Get("sslmode") == "verify-ca" && parsed.Query().Get("sslrootcert") == "" {
			return errors.New("production with sslmode=verify-ca requires sslrootcert in FORGE_DATABASE_URL " +
				"(the CA file the server's certificate must chain to; sslmode=verify-full also checks the host name " +
				"and needs none; FORGE_DATABASE_ALLOW_PLAINTEXT=true accepts unverified connections, only for a trusted private network)")
		}
	case AdapterMySQL:
		if parsed.Host == "" || strings.Trim(parsed.Path, "/") == "" {
			return errors.New("FORGE_DATABASE_URL must be mysql://user:password@host:port/database")
		}
		// tls=true verifies the server certificate; skip-verify and
		// preferred can be intercepted.
		if production && parsed.Query().Get("tls") != "true" {
			return errors.New("production requires FORGE_DATABASE_URL with tls=true " +
				"(set FORGE_DATABASE_ALLOW_PLAINTEXT=true only for a trusted private network)")
		}
	case AdapterSQLite:
		path := d.SQLitePath()
		if parsed.Host != "" || path == "" || strings.Contains(path, ":memory:") || strings.HasPrefix(path, "file:") {
			return errors.New("FORGE_DATABASE_URL must be sqlite:relative/path.db or sqlite:///absolute/path.db")
		}
		if parsed.RawQuery != "" {
			return errors.New("FORGE_DATABASE_URL for SQLite takes no parameters: Forge sets the connection pragmas")
		}
	default:
		return errors.New("FORGE_DATABASE_URL must use the postgres, mysql or sqlite scheme")
	}
	return nil
}

// EnvMigrationDatabaseURL names the connection URL of the migration role:
// the role that owns the schema and applies migrations, separate from the
// less-privileged role FORGE_DATABASE_URL serves with.
const EnvMigrationDatabaseURL = "FORGE_MIGRATION_DATABASE_URL"

// databaseSelectingParams are query parameters with which a driver picks
// the database instead of the URL's path (pgx, like libpq, honours dbname
// and a service file).
var databaseSelectingParams = []string{"dbname", "database", "service", "servicefile"}

// MigrationDatabase returns the connection the migrate and rollback commands
// use. When FORGE_MIGRATION_DATABASE_URL is set (read through lookup), it
// supplies the role: its user, password, host, port and parameters, validated
// like FORGE_DATABASE_URL (production TLS rules included) and of the same
// adapter. Its database is ignored: migrations always change the database
// FORGE_DATABASE_URL names, the one the application serves. Without it, or
// for SQLite, which has no roles, the result is c.Database unchanged.
func (c Config) MigrationDatabase(lookup func(string) (string, bool)) (Database, error) {
	raw, ok := lookup(EnvMigrationDatabaseURL)
	adapter, served := parseDatabaseURL(c.Database.URL.Reveal())
	if !ok || raw == "" || served == nil || adapter == AdapterSQLite {
		return c.Database, nil
	}
	migration := c.Database
	migration.URL = NewSecret(raw)
	if err := migration.validate(c.Environment); err != nil {
		return Database{}, fmt.Errorf("%s is invalid: %w", EnvMigrationDatabaseURL, err)
	}
	if got := migration.Adapter(); got != adapter {
		return Database{}, fmt.Errorf("%s selects the %s adapter, but FORGE_DATABASE_URL selects %s", EnvMigrationDatabaseURL, got, adapter)
	}
	_, parsed := parseDatabaseURL(raw)
	for key := range parsed.Query() {
		for _, param := range databaseSelectingParams {
			if strings.EqualFold(key, param) {
				return Database{}, fmt.Errorf("%s sets the %q parameter: migrations always change the database in FORGE_DATABASE_URL", EnvMigrationDatabaseURL, param)
			}
		}
	}
	parsed.Path, parsed.RawPath = served.Path, served.RawPath
	migration.URL = NewSecret(parsed.String())
	return migration, nil
}
