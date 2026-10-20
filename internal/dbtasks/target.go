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
	"net"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

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
	if err := refuseLocationParams(t.app, "FORGE_DATABASE_URL"); err != nil {
		return nil, err
	}
	t.Name = strings.TrimPrefix(t.app.Path, "/")
	if !databaseName.MatchString(t.Name) {
		return nil, errors.New("the database name in FORGE_DATABASE_URL must be 1-63 letters, digits, underscores, hyphens or dollar signs, and start with a letter, digit or underscore")
	}
	t.priv = t.app
	adminVariable := "FORGE_DATABASE_URL"
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
		adminVariable = EnvPrivilegedURL
		if t.priv, err = parseURL(raw); err != nil {
			return nil, fmt.Errorf("%s: %w", EnvPrivilegedURL, err)
		}
		if err := refuseLocationParams(t.priv, EnvPrivilegedURL); err != nil {
			return nil, err
		}
	}
	if err := refuseSessionParams(t.priv, adminVariable); err != nil {
		return nil, err
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

// locationParams are URL query parameters with which a driver or client
// would pick the server or database instead of the URL's host, port and path:
// pgx (like libpq) lets ?host=, ?port= and ?dbname= override them, and a
// service file can supply them. Local and the reserved-name check read the
// host and path, so a URL carrying any of these is refused rather than
// guarded against an address the commands do not dial. MySQL's names are
// included as defense in depth: its adapter already accepts only tls.
var locationParams = []string{
	"host", "hostaddr", "port", "dbname", "database", "service", "servicefile",
	"socket", "unix_socket", "protocol",
}

// refuseLocationParams fails when u's query names the server or database.
// Whitespace or a control character in the raw query, or in a decoded key,
// is refused too: pgx trims raw spaces around a key ("& host=" is host) and
// net/url does not. The error names the variable and the parameter, never
// the URL.
func refuseLocationParams(u *url.URL, variable string) error {
	unclear := func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }
	if strings.IndexFunc(u.RawQuery, unclear) >= 0 {
		return fmt.Errorf("%s has whitespace or a control character in its parameters: percent-encode spaces as %%20", variable)
	}
	for key := range u.Query() {
		if strings.IndexFunc(key, unclear) >= 0 {
			return fmt.Errorf("%s has a parameter name with whitespace or a control character", variable)
		}
		for _, param := range locationParams {
			if strings.EqualFold(strings.TrimSpace(key), param) {
				return fmt.Errorf("%s sets the %q parameter: forge db commands take the server and database only from the URL's host, port and path", variable, param)
			}
		}
	}
	return nil
}

// refuseSessionParams fails when the administration URL u sets "options",
// with which libpq passes -c session settings (search_path and the like) to
// every connection. The application's own pool may keep using it; forge db
// has no need to, and an admin connection that runs DDL should not depend on
// a setting from the URL. The error names the variable, never the URL.
func refuseSessionParams(u *url.URL, variable string) error {
	for key := range u.Query() {
		if strings.EqualFold(strings.TrimSpace(key), "options") {
			return fmt.Errorf("%s sets the \"options\" parameter: forge db commands do not accept session settings", variable)
		}
	}
	return nil
}

// Local reports whether every server the commands talk to is on this machine:
// a SQLite file, a loopback or localhost address, or a Unix socket path.
func (t *Target) Local() bool {
	if t.Adapter == config.AdapterSQLite {
		return true
	}
	return isLocalHost(t.app) && isLocalHost(t.priv)
}

func isLocalHost(u *url.URL) bool {
	if u == nil {
		return true
	}
	host := u.Hostname()
	if strings.HasPrefix(host, "/") || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ProtectionReason explains why destructive commands need an explicit
// confirmation for this target, or is "" when they do not: only the
// development and test environments on a local database are unguarded.
// FORGE_ENV unset counts as development (the CLI defaults it), so a real
// server is guarded by its address, not by the environment name.
func (t *Target) ProtectionReason() string {
	switch {
	case t.Environment != config.Development && t.Environment != config.Test:
		return fmt.Sprintf("FORGE_ENV is %s", t.Environment)
	case !t.Local():
		return "the database server is not on this machine"
	default:
		return ""
	}
}

// SetLookPath replaces how vendor tools are found, for tests.
func (t *Target) SetLookPath(lookPath func(string) (string, error)) { t.lookPath = lookPath }

// Describe names the database for messages: adapter, name, and for servers
// the host and port the commands talk to, which are the privileged URL's
// when it is set; the application's follow when they differ. It never
// includes a user or password (url.URL.Host holds neither).
func (t *Target) Describe() string {
	if t.Adapter == config.AdapterSQLite {
		return fmt.Sprintf("sqlite database %s", t.Name)
	}
	description := fmt.Sprintf("%s database %q on %s", t.Adapter, t.Name, t.priv.Host)
	if !strings.EqualFold(t.priv.Host, t.app.Host) {
		description += fmt.Sprintf(" (application: %s)", t.app.Host)
	}
	return description
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
