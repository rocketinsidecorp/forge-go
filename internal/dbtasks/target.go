// Package dbtasks implements the database lifecycle behind `forge db`:
// creating and dropping the application's database, dumping and loading its
// schema, and opening the database's own console (ADR 0035).
//
// It talks to the server directly (pgx, the MySQL adapter's driver
// configuration) or runs the vendor's tools (pg_dump, psql, mysqldump,
// mysql, sqlite3). Credentials reach those tools through environment
// variables or a 0600 option file, never through arguments, and nothing here
// prints a connection URL.
package dbtasks

import (
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
)

// EnvPrivilegedURL names an optional privileged connection URL for
// administration (create, drop, dump, load, console): another role on the
// same server, typically the migration/owner role. Its database is ignored;
// the target database always comes from FORGE_DATABASE_URL.
const EnvPrivilegedURL = "FORGE_MIGRATION_DATABASE_URL"

// databaseName is what Forge accepts as a PostgreSQL or MySQL database name:
// safe in every tool, unquoted or quoted, and short enough that no server
// truncates it into a different name.
var databaseName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_$-]{0,62}$`)

// reserved databases are never created, dropped or reset.
var reserved = map[string]bool{
	"postgres": true, "template0": true, "template1": true,
	"mysql": true, "information_schema": true, "performance_schema": true, "sys": true,
}

// Target is the application's database and the means to administer it.
type Target struct {
	// Adapter is the database family.
	Adapter config.Adapter
	// Name is the database name, or the absolute file of a SQLite database.
	Name string
	// Environment is the resolved FORGE_ENV.
	Environment config.Environment

	root string
	cfg  config.Database
	app  *url.URL // nil for SQLite
	priv *url.URL // administration credentials: the privileged URL or the app's

	// lookPath finds vendor tools; tests replace it.
	lookPath func(string) (string, error)
}

// NewTarget resolves the target from the process environment as a lookup
// (process variables over .env files). root is the application directory,
// against which a relative SQLite path resolves.
func NewTarget(root string, lookup func(string) (string, bool)) (*Target, error) {
	cfg, err := config.LoadWithLookup(lookup)
	if err != nil {
		return nil, fmt.Errorf("configuration invalid: %w", err)
	}
	if cfg.Database.URL.IsZero() {
		return nil, errors.New("database is not configured: set FORGE_DATABASE_URL")
	}
	t := &Target{Adapter: cfg.Database.Adapter(), Environment: cfg.Environment, root: root, cfg: cfg.Database, lookPath: exec.LookPath}
	if t.Adapter == config.AdapterSQLite {
		path := cfg.Database.SQLitePath()
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		t.Name = filepath.Clean(path)
		return t, nil
	}
	if t.app, err = parseURL(cfg.Database.URL.Reveal()); err != nil {
		return nil, err
	}
	t.Name = strings.TrimPrefix(t.app.Path, "/")
	if !databaseName.MatchString(t.Name) {
		return nil, errors.New("the database name in FORGE_DATABASE_URL must be 1-63 letters, digits, underscores, hyphens or dollar signs, and start with a letter, digit or underscore")
	}
	t.priv = t.app
	if raw, ok := lookup(EnvPrivilegedURL); ok && raw != "" {
		// The same validation FORGE_DATABASE_URL gets, including production
		// TLS rules, by loading it in its place.
		replaced := func(key string) (string, bool) {
			if key == "FORGE_DATABASE_URL" {
				return raw, true
			}
			return lookup(key)
		}
		privileged, err := config.LoadWithLookup(replaced)
		if err != nil {
			return nil, fmt.Errorf("%s is invalid: %w", EnvPrivilegedURL, err)
		}
		if privileged.Database.Adapter() != t.Adapter {
			return nil, fmt.Errorf("%s selects the %s adapter, but FORGE_DATABASE_URL selects %s", EnvPrivilegedURL, privileged.Database.Adapter(), t.Adapter)
		}
		if t.priv, err = parseURL(raw); err != nil {
			return nil, fmt.Errorf("%s: %w", EnvPrivilegedURL, err)
		}
	}
	return t, nil
}

// parseURL parses without ever quoting the URL in an error: it can hold a
// password.
func parseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("the database URL is not valid")
	}
	return parsed, nil
}

// SetLookPath replaces how vendor tools are found, for tests.
func (t *Target) SetLookPath(lookPath func(string) (string, error)) { t.lookPath = lookPath }

// Describe names the database for messages: adapter, name, and for servers
// the host and port. It never includes a user or password.
func (t *Target) Describe() string {
	if t.Adapter == config.AdapterSQLite {
		return fmt.Sprintf("sqlite database %s", t.Name)
	}
	return fmt.Sprintf("%s database %q on %s", t.Adapter, t.Name, t.app.Host)
}

func (t *Target) connectTimeout() time.Duration {
	if t.cfg.ConnectTimeout > 0 {
		return t.cfg.ConnectTimeout
	}
	return config.Default().Database.ConnectTimeout
}

func (t *Target) checkMutable() error {
	if reserved[strings.ToLower(t.Name)] && t.Adapter != config.AdapterSQLite {
		return fmt.Errorf("refusing to touch the %q system database", t.Name)
	}
	return nil
}

// credentials is the part of a server URL that tools need.
type credentials struct {
	host, port, user, password string
	params                     url.Values
}

func credentialsOf(u *url.URL) credentials {
	c := credentials{host: u.Hostname(), port: u.Port(), params: u.Query()}
	if u.User != nil {
		c.user = u.User.Username()
		c.password, _ = u.User.Password()
	}
	return c
}
