package pgurl_test

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rocketinsidecorp/forge-go/postgres/internal/pgurl"
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
	t.Setenv("PGSSLSNI", "0")
	t.Setenv("PGUSER", "from-env")

	// Without sslmode: libpq's default, prefer (TLS first, plaintext second).
	cfg, _ := parse(t, "postgres://u:p@db.internal:5432/app", pgurl.TLSOnly)
	if cfg.TLSConfig == nil || len(cfg.Fallbacks) != 1 || cfg.Fallbacks[0].TLSConfig != nil {
		t.Errorf("PGSSLMODE changed the TLS mode: TLS=%v fallbacks=%d", cfg.TLSConfig != nil, len(cfg.Fallbacks))
	}
	// With sslmode: the URL decides.
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

// A service named by the environment can set any parameter, TLS files
// included: in both scopes it is replaced by an empty one.
func TestAServiceNamedByTheEnvironmentIsIgnoredInBothScopes(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "svc.conf")
	if err := os.WriteFile(file, []byte("[s]\nsslmode=disable\nsslrootcert=/nonexistent/ca.pem\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGSERVICEFILE", file)
	t.Setenv("PGSERVICE", "s")
	for name, scope := range map[string]pgurl.Scope{"TLSOnly": pgurl.TLSOnly, "Everything": pgurl.Everything} {
		cfg, _ := parse(t, "postgres://u:p@db.internal:5432/app", scope)
		if cfg.TLSConfig == nil {
			t.Errorf("%s: a service file lowered the URL's TLS mode", name)
		}
	}
	// A servicefile in the URL without a service would pair with PGSERVICE.
	if _, err := pgurl.Isolate("postgres://u@h/db?servicefile=/etc/x.conf", pgurl.TLSOnly, nil); !refused(err) {
		t.Errorf("a URL servicefile with PGSERVICE: %v", err)
	}
}

func refused(err error) bool {
	var target *pgurl.RefusedError
	return errors.As(err, &target)
}

// A PG* variable naming a TLS file has no neutral value: pinning it to ""
// would turn "verify against this private CA" into "verify against the
// system roots". It is refused, naming the variable and no value.
func TestTLSFileVariablesTheURLDoesNotNameAreRefused(t *testing.T) {
	for _, tc := range []struct{ env, key string }{
		{"PGSSLROOTCERT", "sslrootcert"}, {"PGSSLCERT", "sslcert"}, {"PGSSLKEY", "sslkey"}, {"PGSSLPASSWORD", "sslpassword"},
	} {
		for name, scope := range map[string]pgurl.Scope{"TLSOnly": pgurl.TLSOnly, "Everything": pgurl.Everything} {
			t.Run(tc.env+"/"+name, func(t *testing.T) {
				t.Setenv(tc.env, "/secret-value-4711/ca.pem")
				raw := "postgres://u:hunter2@db.internal:5432/app?sslmode=verify-ca"
				iso, err := pgurl.Isolate(raw, scope, nil)
				if err == nil {
					iso.Close()
					t.Fatalf("a set %s the URL does not name was accepted", tc.env)
				}
				if !refused(err) || !strings.Contains(err.Error(), tc.env) || !strings.Contains(err.Error(), tc.key+"=") {
					t.Errorf("error = %v, want it to name %s and %s", err, tc.env, tc.key)
				}
				for _, leak := range []string{"secret-value", "hunter2", "db.internal", "://"} {
					if strings.Contains(err.Error(), leak) {
						t.Errorf("the error quotes %q: %v", leak, err)
					}
				}
				// Named in the URL, the variable is irrelevant.
				if iso, err := pgurl.Isolate(raw+"&"+tc.key+"=/ca.pem", scope, nil); err != nil {
					t.Errorf("the URL names %s: %v", tc.key, err)
				} else {
					iso.Close()
				}
			})
		}
	}
}

func TestEverythingIgnoresEveryVariable(t *testing.T) {
	for name, value := range map[string]string{
		"PGSSLMODE": "disable", "PGOPTIONS": "-c search_path=evil", "PGTARGETSESSIONATTRS": "read-only",
		"PGUSER": "evil", "PGPASSWORD": "evilpw", "PGTZ": "Mars/Base",
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

// A temporary directory with a space reaches pgx as %20: url.QueryEscape's
// "+" would be kept literally and break the service file path.
func TestEmptyServiceFileSurvivesASpaceInTheTempDirectory(t *testing.T) {
	spaced := filepath.Join(t.TempDir(), "with space")
	if err := os.Mkdir(spaced, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", spaced)
	t.Setenv("PGSERVICE", "s")
	t.Setenv("PGSSLMODE", "disable")
	cfg, iso := parse(t, "postgres://u:p@db.internal:5432/app", pgurl.Everything)
	if cfg.TLSConfig == nil {
		t.Error("sslmode was not neutralized")
	}
	if strings.Contains(iso.URL, "+") || !strings.Contains(iso.URL, "with%20space") {
		t.Errorf("the service file path is not percent-encoded: %s", iso.URL)
	}
}

// Go and pgx read these URLs differently: the first would let a
// fragment hide the real query, the second reorders sslmode. config.Load
// refuses them; a Database built in code reaches Isolate directly.
func TestIsolateRefusesURLsParsedDifferently(t *testing.T) {
	for name, raw := range map[string]string{
		"fragment":           "postgres://h/db#?sslmode=verify-full",
		"empty fragment":     "postgres://h/db#",
		"@ in the query":     "postgres://h:5432?sslmode=verify-full@x/db",
		"@ in the path":      "postgres://h/d@b",
		"space in the query": "postgres://h/db?application_name=a b",
		"repeated parameter": "postgres://h/db?sslmode=verify-full&sslmode=disable",
	} {
		if _, err := pgurl.Isolate(raw, pgurl.TLSOnly, nil); !refused(err) || !strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("%s: err = %v", name, err)
		} else if strings.Contains(err.Error(), "://") {
			t.Errorf("%s: the error quotes the URL: %v", name, err)
		}
	}
	// An encoded @ in the password is fine.
	if _, err := pgurl.Isolate("postgres://u:p%40w@h/db?sslmode=verify-full", pgurl.TLSOnly, nil); err != nil {
		t.Errorf("an encoded @: %v", err)
	}
}

// Only ssl=true spells sslmode=require; another ssl value is a session
// setting, so sslmode is still pinned when PGSSLMODE is set.
func TestOnlySSLTrueNamesSSLMode(t *testing.T) {
	t.Setenv("PGSSLMODE", "disable")
	iso, err := pgurl.Isolate("postgres://u@h/db?ssl=false", pgurl.TLSOnly, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(iso.URL, "sslmode=prefer") {
		t.Errorf("ssl=false must not stand in for sslmode: %s", iso.URL)
	}
	iso, err = pgurl.Isolate("postgres://u@h/db?ssl=true", pgurl.TLSOnly, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(iso.URL, "sslmode") {
		t.Errorf("a pin would override ssl=true: %s", iso.URL)
	}
}
