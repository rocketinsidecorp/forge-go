// Package pgurl makes a PostgreSQL URL independent of the process
// environment before pgx parses it.
//
// pgx (like libpq) fills every parameter a URL leaves out from PG* variables
// (PGSSLMODE, PGSSLROOTCERT, PGOPTIONS, PGSERVICE, ...) and from the home
// directory, and v5 offers no hook to turn that off. Unsetting the variables
// around the parse would edit the environment of the whole process, which any
// concurrent os.Getenv would observe torn. Instead, Isolate leaves the
// environment alone and writes the libpq default of each such parameter into
// the URL itself: a parameter the URL names beats the environment in pgx, so
// the parse no longer depends on it.
package pgurl

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Scope selects which environment-supplied parameters Isolate pins.
type Scope int

const (
	// TLSOnly pins the parameters that decide whether and how the connection
	// is encrypted and authenticated (sslmode, the certificate files,
	// channel_binding, require_auth). Everything else, such as PGUSER or
	// PGPASSWORD, stays a default for what the URL leaves out.
	TLSOnly Scope = iota
	// Everything pins every parameter pgx reads from the environment, so
	// no PG* variable has any effect, and an environment-named service file
	// is replaced by an empty one.
	Everything
)

// variable is one PG* variable pgx reads: the URL parameter it defaults and
// the value that parameter has when nothing sets it (libpq's default).
type variable struct {
	env     string
	key     string
	neutral string
	tls     bool
}

var variables = []variable{
	{"PGSSLMODE", "sslmode", "prefer", true},
	{"PGSSLROOTCERT", "sslrootcert", "", true},
	{"PGSSLCERT", "sslcert", "", true},
	{"PGSSLKEY", "sslkey", "", true},
	{"PGSSLPASSWORD", "sslpassword", "", true},
	{"PGSSLSNI", "sslsni", "", true},
	{"PGSSLNEGOTIATION", "sslnegotiation", "", true},
	{"PGCHANNELBINDING", "channel_binding", "", true},
	{"PGREQUIREAUTH", "require_auth", "", true},
	{"PGMINPROTOCOLVERSION", "min_protocol_version", "", false},
	{"PGMAXPROTOCOLVERSION", "max_protocol_version", "", false},
	{"PGUSER", "user", "", false},
	{"PGPASSWORD", "password", "", false},
	{"PGPASSFILE", "passfile", "", false},
	{"PGCONNECT_TIMEOUT", "connect_timeout", "0", false},
	{"PGTARGETSESSIONATTRS", "target_session_attrs", "any", false},
}

// Isolated is a URL made independent of the environment.
type Isolated struct {
	// URL is the URL to give pgx.
	URL string

	named   map[string]bool
	pinned  []string
	cleanup func()
}

// Isolate returns raw with the parameters of scope pinned. raw must be a
// postgres:// or postgresql:// URL. When allowed is not nil, every query
// parameter of raw must be in it (dbname is accepted wherever database is
// listed, as pgx treats them alike), and the parameters Isolate adds are
// reported by PinnedKeys so the caller can allow them to pgx. Close the result
// when the parse is done.
func Isolate(raw string, scope Scope, allowed []string) (*Isolated, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return nil, errors.New("not a PostgreSQL URL")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errors.New("malformed query string")
	}
	named := map[string]bool{}
	for key := range query {
		named[key] = true
		// dbname is database's alias for pgx.
		listed := slices.Contains(allowed, key) || (key == "dbname" && slices.Contains(allowed, "database"))
		if allowed != nil && !listed {
			return nil, fmt.Errorf("parameter %q is not accepted", key)
		}
	}
	if u.User != nil {
		named["user"] = true
		if _, ok := u.User.Password(); ok {
			named["password"] = true
		}
	}
	// ssl=true is the URL spelling of sslmode=require.
	if named["ssl"] {
		named["sslmode"] = true
	}

	iso := &Isolated{named: named, cleanup: func() {}}
	var pins []string
	pin := func(key, value string) {
		pins = append(pins, url.QueryEscape(key)+"="+url.QueryEscape(value))
		iso.pinned = append(iso.pinned, key)
	}

	// A service named by the environment is read by pgx, and a service file
	// can set any parameter. In Everything scope it is replaced by an empty
	// one of that name; in TLSOnly scope every TLS parameter is pinned.
	serviceName := os.Getenv("PGSERVICE")
	serviceRead := serviceName != "" && !named["service"]
	if serviceRead && scope == Everything && !named["servicefile"] {
		path, cleanup, err := emptyServiceFile(serviceName)
		if err != nil {
			return nil, err
		}
		iso.cleanup = cleanup
		pin("servicefile", path)
		serviceRead = false
	}
	for _, v := range variables {
		if scope == TLSOnly && !v.tls {
			continue
		}
		if named[v.key] || (os.Getenv(v.env) == "" && !serviceRead) {
			continue
		}
		pin(v.key, v.neutral)
	}
	iso.URL = raw
	if len(pins) > 0 {
		// Appended to the text, not re-encoded from u, so that what the
		// caller wrote reaches pgx byte for byte.
		separator := "?"
		if strings.Contains(raw, "?") {
			separator = "&"
			if strings.HasSuffix(raw, "?") || strings.HasSuffix(raw, "&") {
				separator = ""
			}
		}
		iso.URL = raw + separator + strings.Join(pins, "&")
	}
	return iso, nil
}

// PinnedKeys lists the parameters Isolate added to the URL.
func (i *Isolated) PinnedKeys() []string { return i.pinned }

// StripSession removes from params, the session settings pgx parsed, every
// setting the URL does not name: PGAPPNAME, PGOPTIONS, PGTZ and whatever a
// service file adds. It is meant for Everything scope.
func (i *Isolated) StripSession(params map[string]string) {
	for key := range params {
		if !i.named[key] {
			delete(params, key)
		}
	}
}

// Close removes what Isolate created. It is safe to call more than once.
func (i *Isolated) Close() {
	if i.cleanup != nil {
		i.cleanup()
		i.cleanup = nil
	}
}

// emptyServiceFile writes a service file holding only an empty section named
// name, so that pgx finds the service the environment names and gets nothing
// from it.
func emptyServiceFile(name string) (path string, cleanup func(), err error) {
	if strings.ContainsAny(name, "[]\r\n\x00") {
		return "", nil, errors.New("PGSERVICE holds a character a service name cannot contain")
	}
	dir, err := os.MkdirTemp("", "forge-pgservice-")
	if err != nil {
		return "", nil, fmt.Errorf("create an empty PostgreSQL service file: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	path = filepath.Join(dir, "pg_service.conf")
	if err := os.WriteFile(path, []byte("["+name+"]\n"), 0o600); err != nil { // #nosec G703 -- a fixed file name in the directory created above.
		cleanup()
		return "", nil, fmt.Errorf("create an empty PostgreSQL service file: %w", err)
	}
	return path, cleanup, nil
}
