package pgurl_test

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rocketinsidecorp/forge-go/internal/pgurl"
)

func parse(t *testing.T, raw string, scope pgurl.Scope) (*pgconn.Config, *pgurl.Isolated) {
	t.Helper()
	iso, err := pgurl.Isolate(raw, scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(iso.Close)
	cfg, err := pgconn.ParseConfig(iso.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg, iso
}

func TestTLSOnlyIgnoresTLSVariables(t *testing.T) {
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGSSLROOTCERT", "/nonexistent/ca.pem")
	t.Setenv("PGSSLCERT", "/nonexistent/cert.pem")
	t.Setenv("PGSSLKEY", "/nonexistent/key.pem")
	t.Setenv("PGUSER", "from-env")

	// Without sslmode: libpq's default, prefer (TLS first, plaintext second).
	cfg, _ := parse(t, "postgres://u:p@db.internal:5432/app", pgurl.TLSOnly)
	if cfg.TLSConfig == nil || len(cfg.Fallbacks) != 1 || cfg.Fallbacks[0].TLSConfig != nil {
		t.Errorf("PGSSLMODE changed the TLS mode: TLS=%v fallbacks=%d", cfg.TLSConfig != nil, len(cfg.Fallbacks))
	}
	// With sslmode: the URL decides, and PGSSLROOTCERT does not replace its trust anchors.
	cfg, _ = parse(t, "postgres://u:p@db.internal:5432/app?sslmode=verify-full", pgurl.TLSOnly)
	if cfg.TLSConfig == nil || len(cfg.Fallbacks) != 0 || cfg.TLSConfig.InsecureSkipVerify && cfg.TLSConfig.VerifyConnection == nil {
		t.Errorf("verify-full was weakened: %+v", cfg.TLSConfig)
	}
	// Non-TLS defaults stay what pgx makes them: PGUSER still fills a missing user.
	cfg, _ = parse(t, "postgres://db.internal:5432/app", pgurl.TLSOnly)
	if cfg.User != "from-env" {
		t.Errorf("TLSOnly must leave PGUSER as a default, got %q", cfg.User)
	}
}

func TestTLSOnlyPinsEveryTLSParameterWhenAServiceIsNamed(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "svc.conf")
	if err := os.WriteFile(file, []byte("[s]\nsslmode=disable\nsslrootcert=/nonexistent/ca.pem\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGSERVICEFILE", file)
	t.Setenv("PGSERVICE", "s")
	cfg, _ := parse(t, "postgres://u:p@db.internal:5432/app", pgurl.TLSOnly)
	if cfg.TLSConfig == nil {
		t.Error("a service file lowered the URL's TLS mode")
	}
}

func TestEverythingIgnoresEveryVariable(t *testing.T) {
	for name, value := range map[string]string{
		"PGSSLMODE": "disable", "PGOPTIONS": "-c search_path=evil", "PGTARGETSESSIONATTRS": "read-only",
		"PGUSER": "evil", "PGPASSWORD": "evilpw", "PGSSLROOTCERT": "/nonexistent/ca.pem", "PGTZ": "Mars/Base",
		"PGAPPNAME": "evil", "PGCONNECT_TIMEOUT": "99", "PGPASSFILE": "/nonexistent/pgpass", "PGSSLNEGOTIATION": "direct",
		"PGSERVICE": "missing-service", "PGSERVICEFILE": "/nonexistent/pg_service.conf",
	} {
		t.Setenv(name, value)
	}
	cfg, iso := parse(t, "postgres://m:pw@localhost:5432/shop?application_name=forge", pgurl.Everything)
	iso.StripSession(cfg.RuntimeParams)
	if cfg.TLSConfig == nil || len(cfg.Fallbacks) != 1 {
		t.Errorf("sslmode must be prefer, got TLS %v and %d fallbacks", cfg.TLSConfig != nil, len(cfg.Fallbacks))
	}
	if cfg.User != "m" || cfg.Password != "pw" || cfg.ValidateConnect != nil || cfg.ConnectTimeout != 0 || cfg.SSLNegotiation == "direct" {
		t.Errorf("a variable reached the config: user=%q validate=%v timeout=%v negotiation=%q", cfg.User, cfg.ValidateConnect != nil, cfg.ConnectTimeout, cfg.SSLNegotiation)
	}
	if len(cfg.RuntimeParams) != 1 || cfg.RuntimeParams["application_name"] != "forge" {
		t.Errorf("session settings = %v, want only the URL's application_name", cfg.RuntimeParams)
	}
}

func TestEverythingReplacesTheServiceFileAndRemovesIt(t *testing.T) {
	file := filepath.Join(t.TempDir(), "svc.conf")
	if err := os.WriteFile(file, []byte("[s]\nsslmode=disable\noptions=-c search_path=evil\nuser=evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGSERVICEFILE", file)
	t.Setenv("PGSERVICE", "s")
	iso, err := pgurl.Isolate("postgres://m:pw@localhost:5432/shop", pgurl.Everything, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgconn.ParseConfig(iso.URL)
	if err != nil {
		t.Fatal(err)
	}
	iso.StripSession(cfg.RuntimeParams)
	if cfg.User != "m" || cfg.TLSConfig == nil || len(cfg.RuntimeParams) != 0 {
		t.Errorf("the service file reached the config: user=%q tls=%v params=%v", cfg.User, cfg.TLSConfig != nil, cfg.RuntimeParams)
	}
	parsed, err := url.Parse(iso.URL)
	if err != nil {
		t.Fatal(err)
	}
	servicefile := parsed.Query().Get("servicefile")
	if servicefile == "" || strings.Contains(servicefile, "svc.conf") {
		t.Fatalf("the URL must name the empty service file, got %q", servicefile)
	}
	iso.Close()
	iso.Close()
}

func TestIsolateLeavesAnUnaffectedURLAlone(t *testing.T) {
	for _, raw := range []string{
		"postgres://u:p%40w@h:5432/db",
		"postgres://u:p@h/db?sslmode=verify-full&sslrootcert=system",
	} {
		iso, err := pgurl.Isolate(raw, pgurl.Everything, nil)
		if err != nil {
			t.Fatal(err)
		}
		if iso.URL != raw || len(iso.PinnedKeys()) != 0 {
			t.Errorf("URL changed without a variable set: %q -> %q", raw, iso.URL)
		}
	}
}

func TestIsolateKeepsWhatTheURLNames(t *testing.T) {
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGUSER", "evil")
	t.Setenv("PGPASSWORD", "evilpw")
	// The user comes from the userinfo, the password from the environment-free pin.
	iso, err := pgurl.Isolate("postgres://alice@h/db?sslmode=require", pgurl.Everything, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgconn.ParseConfig(iso.URL)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.User != "alice" || cfg.Password != "" || cfg.TLSConfig == nil || len(cfg.Fallbacks) != 0 {
		t.Errorf("user=%q password=%q tls=%v fallbacks=%d", cfg.User, cfg.Password, cfg.TLSConfig != nil, len(cfg.Fallbacks))
	}
	// ssl=true spells sslmode=require: no second sslmode is added.
	iso, err = pgurl.Isolate("postgres://u@h/db?ssl=true", pgurl.TLSOnly, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(iso.URL, "sslmode") {
		t.Errorf("a pin would override ssl=true: %s", iso.URL)
	}
}

func TestIsolateRefusesWhatItCannotMakeSafe(t *testing.T) {
	if _, err := pgurl.Isolate("host=localhost", pgurl.TLSOnly, nil); err == nil {
		t.Error("a keyword/value string was accepted")
	}
	if _, err := pgurl.Isolate("mysql://h/db", pgurl.TLSOnly, nil); err == nil {
		t.Error("a mysql URL was accepted")
	}
	allowed := []string{"database", "sslmode"}
	if _, err := pgurl.Isolate("postgres://h/db?service=x", pgurl.Everything, allowed); err == nil {
		t.Error("a parameter outside the allow-list was accepted")
	}
	if _, err := pgurl.Isolate("postgres://h/db?dbname=other&sslmode=require", pgurl.Everything, allowed); err != nil {
		t.Errorf("dbname is database's alias: %v", err)
	}
	t.Setenv("PGSERVICE", "bad]name")
	if _, err := pgurl.Isolate("postgres://h/db", pgurl.Everything, nil); err == nil {
		t.Error("a service name that would break the service file was accepted")
	}
}

// The point of the package: parsing never edits the process environment, so
// concurrent readers never see it torn.
func TestIsolateDoesNotEditTheEnvironment(t *testing.T) {
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGSERVICE", "svc")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	torn := make(chan string, 1)
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if os.Getenv("PGSSLMODE") != "disable" || os.Getenv("PGSERVICE") != "svc" {
				select {
				case torn <- "a reader saw the environment change":
				default:
				}
				return
			}
		}
	})
	for range 200 {
		iso, err := pgurl.Isolate("postgres://u:p@h:5432/db", pgurl.Everything, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pgconn.ParseConfig(iso.URL); err != nil {
			t.Fatal(err)
		}
		iso.Close()
	}
	close(stop)
	wg.Wait()
	select {
	case msg := <-torn:
		t.Fatal(msg)
	default:
	}
}
