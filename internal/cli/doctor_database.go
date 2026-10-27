package cli

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
)

// doctorConnectTimeout is how long the database server gets to accept a TCP
// connection, as in Forge-Rust's `forge doctor`.
const doctorConnectTimeout = 3 * time.Second

// doctorDatabaseUnreachable is the diagnostic of a server that does not
// answer: Forge-Rust's code, summary and remediation word for word.
func doctorDatabaseUnreachable(reason string) doctorDiagnostic {
	return doctorDiagnostic{
		Code:        "database.unreachable",
		Severity:    "error",
		Summary:     "The configured database server does not answer.",
		Remediation: "Start it (for example `docker compose up -d db`) and check the host and port of FORGE_DATABASE_URL: " + reason + ".",
	}
}

// doctorDatabaseMalformed is the diagnostic of a URL the probe cannot read:
// Forge-Rust's code, summary and remediation word for word. The URL is not
// shown, because it holds credentials.
func doctorDatabaseMalformed() doctorDiagnostic {
	return doctorDiagnostic{
		Code:        "database.url_malformed",
		Severity:    "error",
		Summary:     "FORGE_DATABASE_URL is malformed.",
		Remediation: "Percent-encode `/`, `?`, `#` and `@` in the user name and password (for example `/` as %2F), and give a `host=` parameter a plain host name or an absolute socket directory; the URL is not shown because it holds credentials.",
	}
}

// probeDatabase says whether the configured database server answers, in the
// words of Forge-Rust's `forge doctor`. A server database gets a TCP
// connection to its host and port within doctorConnectTimeout; the probe never
// logs in, so no credential leaves the machine, and a failure names a reason
// but never the host, user or password. SQLite has no server: the file either
// exists or the first migrate creates it. A PostgreSQL Unix socket is not
// checked. A non-empty reason means the server does not answer, and malformed
// that the URL cannot be read at all.
func probeDatabase(ctx context.Context, database config.Database, root string) (status, reason string, malformed bool) {
	adapter, port := database.Adapter(), ""
	raw := database.URL.Reveal()
	parsed, err := url.Parse(raw)
	if err != nil || adapter == "" {
		return "not checked (the URL is malformed)", "", true
	}
	switch adapter {
	case config.AdapterSQLite:
		file := database.SQLitePath()
		if file == "" || file == ":memory:" {
			return "not checked (no database file)", "", false
		}
		if !filepath.IsAbs(file) {
			file = filepath.Join(root, file)
		}
		if _, err := os.Stat(file); err == nil {
			return "the file exists", "", false
		}
		return "the file does not exist yet (`forge db create` or the first migrate creates it)", "", false
	case config.AdapterPostgres:
		port = "5432"
	case config.AdapterMySQL:
		port = "3306"
	default:
		return "not checked (no database)", "", false
	}
	host := parsed.Hostname()
	if override := parsed.Query().Get("host"); adapter == config.AdapterPostgres && override != "" {
		host = override
	}
	if strings.HasPrefix(host, "/") {
		return "not checked (a Unix socket)", "", false
	}
	if value := parsed.Port(); value != "" {
		port = value
	}
	if value, err := strconv.Atoi(parsed.Query().Get("port")); adapter == config.AdapterPostgres && err == nil && value > 0 && value < 65536 {
		port = strconv.Itoa(value)
	}
	ctx, cancel := context.WithTimeout(ctx, doctorConnectTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return "", dialFailure(err), false
	}
	_ = conn.Close()
	return "the server accepts connections", "", false
}

// dialFailure names why a dial failed without echoing the address, which the
// net package puts in its errors.
func dialFailure(err error) string {
	var dns *net.DNSError
	var errno syscall.Errno
	switch {
	case errors.As(err, &dns):
		return "the host name does not resolve"
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded):
		return "timed out"
	case errors.As(err, &errno):
		return strings.ToLower(errno.Error())
	default:
		return "could not connect"
	}
}
