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
//
// Parameters with no neutral value are refused instead: a PG* variable naming
// a TLS file (PGSSLROOTCERT, PGSSLCERT, PGSSLKEY, PGSSLPASSWORD) that the URL
// does not name stops Isolate with a [RefusedError]. Pinning them to "" would
// turn "verify against this private CA" into "verify against the system
// roots" and discard the home-directory defaults, so the caller is asked to
// put the parameter in the URL.
package pgurl

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
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

// RefusedError is a URL or environment Isolate will not make safe. Its
// message names variables and parameters, never a value or the URL, so a
// caller may show it.
type RefusedError struct{ msg string }

func (e *RefusedError) Error() string { return e.msg }

func refuse(format string, args ...any) error {
	return &RefusedError{msg: fmt.Sprintf(format, args...)}
}

// variable is one PG* variable pgx reads: the URL parameter it defaults and
// the value that parameter has when nothing sets it (libpq's default). A
// variable with refuse set has no neutral value: it must be named in the URL
// when it is set.
type variable struct {
	env     string
	key     string
	neutral string
	tls     bool
	refuse  bool
}

var variables = []variable{
	{"PGSSLMODE", "sslmode", "prefer", true, false},
	{"PGSSLROOTCERT", "sslrootcert", "", true, true},
	{"PGSSLCERT", "sslcert", "", true, true},
	{"PGSSLKEY", "sslkey", "", true, true},
	{"PGSSLPASSWORD", "sslpassword", "", true, true},
	{"PGSSLSNI", "sslsni", "", true, false},
	{"PGSSLNEGOTIATION", "sslnegotiation", "", true, false},
	{"PGCHANNELBINDING", "channel_binding", "", true, false},
	{"PGREQUIREAUTH", "require_auth", "", true, false},
	{"PGMINPROTOCOLVERSION", "min_protocol_version", "", false, false},
	{"PGMAXPROTOCOLVERSION", "max_protocol_version", "", false, false},
	{"PGUSER", "user", "", false, false},
	{"PGPASSWORD", "password", "", false, false},
	{"PGPASSFILE", "passfile", "", false, false},
	{"PGCONNECT_TIMEOUT", "connect_timeout", "0", false, false},
	{"PGTARGETSESSIONATTRS", "target_session_attrs", "any", false, false},
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
//
// Isolate refuses, with a [RefusedError], a URL that net/url and pgx would
// read differently (a fragment, an "@" in the query or path, whitespace or a
// control character in the query, a repeated parameter), and a set PG* TLS
// file variable that the URL does not name (see the package comment).
func Isolate(raw string, scope Scope, allowed []string) (*Isolated, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return nil, errors.New("not a PostgreSQL URL")
	}
	if u.Fragment != "" || strings.Contains(raw, "#") || strings.Contains(u.RawQuery, "@") || strings.Contains(u.Path, "@") {
		return nil, refuse("the database URL is ambiguous: percent-encode \"@\" and \"#\" in passwords and parameters")
	}
	if strings.IndexFunc(u.RawQuery, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return nil, refuse("the database URL is ambiguous: its parameters hold whitespace or a control character (percent-encode spaces as %%20)")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errors.New("malformed query string")
	}
	named := map[string]bool{}
	for key, values := range query {
		if len(values) > 1 {
			return nil, refuse("the database URL is ambiguous: it repeats a parameter")
		}
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
	// ssl=true is the URL spelling of sslmode=require; any other ssl value is
	// an unrelated session setting.
	if query.Get("ssl") == "true" {
		named["sslmode"] = true
	}

	iso := &Isolated{named: named, cleanup: func() {}}
	var pins []string
	pin := func(key, value string) {
		pins = append(pins, escape(key)+"="+escape(value))
		iso.pinned = append(iso.pinned, key)
	}

	// A service named by the environment is read by pgx, and a service file
	// can set any parameter, TLS files included: it is replaced by an empty
	// one of that name, in both scopes.
	if serviceName := os.Getenv("PGSERVICE"); serviceName != "" && !named["service"] {
		if named["servicefile"] {
			return nil, refuse("PGSERVICE is set and the database URL names a servicefile but no service: name the service in the URL (Forge does not read PG* variables for it)")
		}
		path, cleanup, err := emptyServiceFile(serviceName)
		if err != nil {
			return nil, err
		}
		iso.cleanup = cleanup
		pin("servicefile", path)
	}
	for _, v := range variables {
		if (scope == TLSOnly && !v.tls) || named[v.key] || os.Getenv(v.env) == "" {
			continue
		}
		if v.refuse {
			iso.Close()
			return nil, refuse("%s is set but the database URL has no %s: put %s=... in the URL (Forge does not read PG* TLS variables)", v.env, v.key, v.key)
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

// escape percent-encodes a query component. A space is %20, not the "+" of
// url.QueryEscape, which pgx would keep literally.
func escape(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }

// PinnedKeys lists the parameters Isolate added to the URL.
func (i *Isolated) PinnedKeys() []string { return i.pinned }

// StripSession removes from params, the session settings pgx parsed, every
// setting the URL does not name: PGAPPNAME, PGOPTIONS, PGTZ and whatever a
// service file adds. It works in both scopes: an application's serving pool
// (TLSOnly) uses it so that PGOPTIONS cannot give its sessions a default
// tenant, and forge db (Everything) so that no PG* variable has any effect.
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
