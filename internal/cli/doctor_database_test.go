package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
)

// listenForDoctor starts a TCP listener that records what a client sends and
// returns its host:port. A probe that logged in would send bytes; closing the
// connection unread proves it did not.
func listenForDoctor(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	var sent atomic.Bool
	t.Cleanup(func() {
		_ = listener.Close()
		if sent.Load() {
			t.Error("the doctor probe sent bytes to the database server")
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			buf := make([]byte, 16)
			if n, _ := conn.Read(buf); n > 0 {
				sent.Store(true)
			}
			_ = conn.Close()
		}
	}()
	return listener.Addr().String()
}

// closedAddress is a loopback address nothing listens on.
func closedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}

// forge doctor checks that the database server answers (forge-go#90), with
// Forge-Rust's words: a TCP connection within 3 seconds, no login, no secret.
func TestDoctorReportsAServerThatAnswers(t *testing.T) {
	t.Parallel()
	server := listenForDoctor(t)
	for name, scheme := range map[string]string{
		"postgres": "postgres://alice:hunter2@" + server + "/shop?sslmode=disable",
		"mysql":    "mysql://alice:hunter2@" + server + "/shop",
	} {
		var out bytes.Buffer
		if err := runDoctorAt(nil, &out, t.TempDir(), []string{"FORGE_DATABASE_URL=" + scheme}); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out.String())
		}
		if !strings.Contains(out.String(), "database reachability: the server accepts connections\n") || strings.Contains(out.String(), "hunter2") {
			t.Errorf("%s: %q", name, out.String())
		}
	}
}

func TestDoctorFailsOnAClosedPortWithoutLeakingTheURL(t *testing.T) {
	t.Parallel()
	closed := closedAddress(t)
	env := []string{"FORGE_DATABASE_URL=postgres://alice:hunter2@" + closed + "/shop?sslmode=disable"}
	var text bytes.Buffer
	if err := runDoctorAt(nil, &text, t.TempDir(), env); !errors.Is(err, errDoctorUnhealthy) {
		t.Fatalf("text: err = %v, want errDoctorUnhealthy", err)
	}
	for _, want := range []string{
		"database reachability: unreachable (connection refused)\n",
		"error: The configured database server does not answer. Start it (for example `docker compose up -d db`) and check the host and port of FORGE_DATABASE_URL: connection refused.\n",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, text.String())
		}
	}
	var out bytes.Buffer
	if err := runDoctorAt([]string{"--json"}, &out, t.TempDir(), env); !errors.Is(err, errDoctorUnhealthy) {
		t.Fatalf("json: err = %v, want errDoctorUnhealthy", err)
	}
	if strings.Contains(out.String(), "hunter2") || strings.Contains(out.String(), "alice") || strings.Contains(out.String(), closed) {
		t.Fatalf("the report leaks the URL: %s", out.String())
	}
	var report doctorReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != 1 || report.OK || report.Database != "postgres" || len(report.Diagnostics) != 1 ||
		report.Diagnostics[0].Code != "database.unreachable" || report.Diagnostics[0].Severity != "error" ||
		report.Diagnostics[0].Summary != "The configured database server does not answer." {
		t.Fatalf("report = %+v", report)
	}
}

func TestDoctorSQLiteAndSocketsAreNotProbedOverTheNetwork(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	run := func(url string) string {
		t.Helper()
		var out bytes.Buffer
		if err := runDoctorAt(nil, &out, root, []string{"FORGE_DATABASE_URL=" + url}); err != nil {
			t.Fatalf("%s: %v\n%s", url, err, out.String())
		}
		return out.String()
	}
	if got := run("sqlite:storage/dev.db"); !strings.Contains(got, "database reachability: the file does not exist yet (`forge db create` or the first migrate creates it)\n") {
		t.Errorf("a missing SQLite file:\n%s", got)
	}
	if err := os.MkdirAll(filepath.Join(root, "storage"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "storage", "dev.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := run("sqlite:storage/dev.db"); !strings.Contains(got, "database reachability: the file exists\n") {
		t.Errorf("an existing SQLite file:\n%s", got)
	}
	// An absolute path is used as it is, not relative to the application.
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.db")
	if got := run("sqlite://" + elsewhere); !strings.Contains(got, "database reachability: the file does not exist yet") {
		t.Errorf("a missing absolute SQLite file:\n%s", got)
	}
	if err := os.WriteFile(elsewhere, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := run("sqlite://" + elsewhere); !strings.Contains(got, "database reachability: the file exists\n") {
		t.Errorf("an existing absolute SQLite file:\n%s", got)
	}
	if status, _, malformed := probeDatabase(t.Context(), config.Database{URL: config.NewSecret("sqlite::memory:")}, root); malformed || !strings.Contains(status, "not checked") {
		t.Errorf("an in-memory SQLite URL: %q, malformed %v", status, malformed)
	}
	if got := run("postgres://alice@localhost/shop?host=/var/run/postgresql&sslmode=disable"); !strings.Contains(got, "database reachability: not checked (a Unix socket)\n") {
		t.Errorf("a PostgreSQL socket:\n%s", got)
	}
}

// The host and port the driver would use: a host= or port= parameter of the
// PostgreSQL URL overrides the authority, and the engine's default port
// applies when none is given.
func TestProbeDatabaseUsesTheDriversHostAndPort(t *testing.T) {
	t.Parallel()
	server := listenForDoctor(t)
	host, port, _ := net.SplitHostPort(server)
	for name, url := range map[string]string{
		"host and port parameters": "postgres://u:p@192.0.2.1:9/shop?sslmode=disable&host=" + host + "&port=" + port,
		"port parameter only":      "postgres://u:p@127.0.0.1/shop?sslmode=disable&port=" + port,
	} {
		var out bytes.Buffer
		if err := runDoctorAt(nil, &out, t.TempDir(), []string{"FORGE_DATABASE_URL=" + url}); err != nil {
			t.Errorf("%s: %v\n%s", name, err, out.String())
		}
	}
}

// A URL the probe cannot read is Forge-Rust's database.url_malformed error,
// without echoing the URL.
func TestDoctorReportsAMalformedDatabaseURL(t *testing.T) {
	t.Parallel()
	status, _, malformed := probeDatabase(t.Context(), config.Database{URL: config.NewSecret("postgres://u:hunter2@%zz/db")}, t.TempDir())
	if !malformed || status != "not checked (the URL is malformed)" {
		t.Fatalf("status %q, malformed %v", status, malformed)
	}
	diagnostic := doctorDatabaseMalformed()
	if diagnostic.Code != "database.url_malformed" || diagnostic.Severity != "error" || diagnostic.Summary != "FORGE_DATABASE_URL is malformed." || strings.Contains(diagnostic.Remediation, "hunter2") {
		t.Errorf("diagnostic = %+v", diagnostic)
	}
}

func TestDialFailureNamesTheReasonWithoutTheAddress(t *testing.T) {
	t.Parallel()
	for want, err := range map[string]error{
		"the host name does not resolve": &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "db.internal"}},
		"timed out":                      context.DeadlineExceeded,
		"connection refused":             &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)},
		"network is unreachable":         &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ENETUNREACH)},
		"could not connect":              errors.New("dial tcp 10.0.0.1:5432: something else"),
	} {
		if got := dialFailure(err); got != want {
			t.Errorf("dialFailure(%v) = %q, want %q", err, got, want)
		}
	}
}
