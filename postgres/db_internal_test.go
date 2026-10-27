package postgres

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/postgres/internal/pgurl"
)

// encodeQuery percent-encodes a space as %20: pgx keeps the "+" of
// url.Values.Encode literally.
func encodeQuery(query url.Values) string { return strings.ReplaceAll(query.Encode(), "+", "%20") }

func databaseAt(raw string) config.Database {
	cfg := config.Default().Database
	cfg.URL = config.NewSecret(raw)
	return cfg
}

// writeCA writes a self-signed CA certificate, as a private CA's file would
// be, and returns its path and pool.
func writeCA(t *testing.T) (string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "forge test private CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "private-ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return path, pool
}

// pgx reads PGSSLMODE and the other TLS variables as defaults for what the
// URL leaves out. They must not decide the TLS of a URL that is parsed, and
// parsing must not touch the process environment.
func TestParseConfigIgnoresPGTLSModeVariables(t *testing.T) {
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGCHANNELBINDING", "disable")

	// No sslmode in the URL: libpq's default, prefer, not PGSSLMODE=disable.
	pool, err := parseConfig(databaseAt("postgres://app:pw@db.internal:5432/app"), stripEnvironmentSession)
	if err != nil {
		t.Fatal(err)
	}
	conn := pool.ConnConfig.Config
	if conn.TLSConfig == nil || len(conn.Fallbacks) != 1 {
		t.Errorf("PGSSLMODE changed the mode: TLS=%v fallbacks=%d", conn.TLSConfig != nil, len(conn.Fallbacks))
	}
	if conn.ChannelBinding != "prefer" {
		t.Errorf("PGCHANNELBINDING changed channel binding to %q", conn.ChannelBinding)
	}

	// sslmode=require in the URL keeps TLS on every attempt, and pool
	// parameters survive.
	pool, err = parseConfig(databaseAt("postgres://app:pw@db.internal:5432/app?sslmode=require&pool_max_conns=3"), stripEnvironmentSession)
	if err != nil {
		t.Fatal(err)
	}
	if conn = pool.ConnConfig.Config; conn.TLSConfig == nil || len(conn.Fallbacks) != 0 {
		t.Errorf("sslmode=require lost TLS: TLS=%v fallbacks=%d", conn.TLSConfig != nil, len(conn.Fallbacks))
	}
	if pool.MaxConns != 3 {
		t.Errorf("pool parameters must survive: max conns %d", pool.MaxConns)
	}
	if os.Getenv("PGSSLMODE") != "disable" {
		t.Error("PGSSLMODE was removed from the process environment")
	}
}

// Regression: pinning PGSSLROOTCERT to "" made verify-ca (and require) verify
// against the system roots, with no host name check. The variable is now
// refused when the URL does not name sslrootcert, whatever the sslmode, and
// when the URL names it the variable is irrelevant: the private CA's file is
// the only trust anchor.
func TestParseConfigRefusesAPGRootCertTheURLDoesNotRepeat(t *testing.T) {
	caPath, caPool := writeCA(t)
	t.Setenv("PGSSLROOTCERT", caPath)

	for _, mode := range []string{"verify-ca", "verify-full", "require"} {
		raw := "postgres://app:s3cret@db.internal:5432/app?sslmode=" + mode
		_, err := parseConfig(databaseAt(raw), stripEnvironmentSession)
		var refused *pgurl.RefusedError
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), "PGSSLROOTCERT") || !strings.Contains(err.Error(), "sslrootcert=") {
			t.Fatalf("sslmode=%s: err = %v, want a refusal naming PGSSLROOTCERT and sslrootcert", mode, err)
		}
		for _, leak := range []string{caPath, "s3cret", "db.internal", "://"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("sslmode=%s: the error quotes %q: %v", mode, leak, err)
			}
		}
	}

	// Without sslmode the default (prefer) is not a verification mode, but
	// the variable is refused all the same: the URL must say what it trusts.
	if _, err := parseConfig(databaseAt("postgres://app:pw@db.internal:5432/app"), stripEnvironmentSession); err == nil {
		t.Error("a PGSSLROOTCERT the URL does not repeat was accepted without sslmode")
	}

	// Named in the URL: verify-ca trusts that file alone, whatever PGSSLROOTCERT holds.
	other, _ := writeCA(t)
	t.Setenv("PGSSLROOTCERT", other)
	query := url.Values{"sslmode": {"verify-ca"}, "sslrootcert": {caPath}}
	pool, err := parseConfig(databaseAt("postgres://app:pw@db.internal:5432/app?"+encodeQuery(query)), stripEnvironmentSession)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig := pool.ConnConfig.TLSConfig
	if tlsConfig == nil || tlsConfig.RootCAs == nil || !tlsConfig.RootCAs.Equal(caPool) {
		t.Fatalf("verify-ca must trust only the URL's sslrootcert file: %+v", tlsConfig)
	}
	query.Set("sslmode", "require")
	pool, err = parseConfig(databaseAt("postgres://app:pw@db.internal:5432/app?"+encodeQuery(query)), stripEnvironmentSession)
	if err != nil {
		t.Fatal(err)
	}
	if pool.ConnConfig.TLSConfig == nil {
		t.Error("require with sslrootcert lost TLS")
	}
}

// config.Load refuses these URLs; a Database built in code reaches
// postgres.Open directly and gets the same refusal.
func TestParseConfigRefusesURLsGoAndPgxReadDifferently(t *testing.T) {
	for name, raw := range map[string]string{
		"fragment":       "postgres://app:pw@db.internal/app#?sslmode=verify-full",
		"@ in the query": "postgres://db.internal:5432?sslmode=verify-full@x/app",
	} {
		if _, err := parseConfig(databaseAt(raw), stripEnvironmentSession); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// TestServingPoolRefusesPGOptions: PGOPTIONS reaches the migrator, as for
// psql, but a serving pool refuses to be built while it is set, since a
// PGOPTIONS='-c forge.tenant_id=...' would give every session a default
// tenant. The error names the variable and never its value. Blank is no
// setting.
func TestServingPoolRefusesPGOptions(t *testing.T) {
	const tenant = "0190d6c5-5f1a-7d0e-8b1a-6a3c1f2e9d4b"
	t.Setenv("PGOPTIONS", "-c forge.tenant_id="+tenant)
	raw := "postgres://app:pw@db.internal:5432/app"
	for _, url := range []string{raw, raw + "?options=-c%20lock_timeout%3D5s"} {
		_, err := parseConfig(databaseAt(url), stripEnvironmentSession)
		if err == nil || !strings.Contains(err.Error(), "PGOPTIONS is set") {
			t.Fatalf("a serving pool opened with PGOPTIONS set: %v", err)
		}
		if strings.Contains(err.Error(), tenant) {
			t.Errorf("the refusal echoes the value: %v", err)
		}
	}
	migrator, err := parseConfig(databaseAt(raw), keepEnvironmentSession)
	if err != nil {
		t.Fatal(err)
	}
	if got := migrator.ConnConfig.RuntimeParams["options"]; !strings.Contains(got, "forge.tenant_id") {
		t.Errorf("the migrator lost PGOPTIONS: %q", got)
	}
	for _, blank := range []string{"", "  "} {
		t.Setenv("PGOPTIONS", blank)
		pool, err := parseConfig(databaseAt(raw), stripEnvironmentSession)
		if err != nil {
			t.Fatalf("a blank PGOPTIONS %q is no setting: %v", blank, err)
		}
		if value, ok := pool.ConnConfig.RuntimeParams["options"]; ok {
			t.Errorf("a blank PGOPTIONS reached the pool: %q", value)
		}
	}
}

// TestServingPoolIgnoresPGTZAndPGAppName: PGTZ and PGAPPNAME reach the
// migrator but are ignored by the serving pool, which keeps the settings the
// URL names.
func TestServingPoolIgnoresPGTZAndPGAppName(t *testing.T) {
	t.Setenv("PGOPTIONS", "")
	t.Setenv("PGTZ", "America/Sao_Paulo")
	t.Setenv("PGAPPNAME", "from-the-environment")
	pool, err := parseConfig(databaseAt("postgres://app:pw@db.internal:5432/app"), stripEnvironmentSession)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"options", "timezone", "application_name"} {
		if value, ok := pool.ConnConfig.RuntimeParams[key]; ok {
			t.Errorf("the serving pool took %s=%q from the environment", key, value)
		}
	}
	migrator, err := parseConfig(databaseAt("postgres://app:pw@db.internal:5432/app"), keepEnvironmentSession)
	if err != nil {
		t.Fatal(err)
	}
	if got := migrator.ConnConfig.RuntimeParams["timezone"]; got != "America/Sao_Paulo" {
		t.Errorf("the migrator lost PGTZ: %q", got)
	}
	named, err := parseConfig(databaseAt("postgres://app:pw@db.internal:5432/app?application_name=billing&options=-c%20lock_timeout%3D5s"), stripEnvironmentSession)
	if err != nil {
		t.Fatal(err)
	}
	params := named.ConnConfig.RuntimeParams
	if params["application_name"] != "billing" || params["options"] != "-c lock_timeout=5s" {
		t.Errorf("settings the URL names were dropped: %v", params)
	}
	if _, ok := params["timezone"]; ok {
		t.Error("PGTZ reached a pool whose URL names other settings")
	}
}

// A service file the URL names (service=, servicefile=) can set session
// settings too; the serving pool keeps only those the URL itself names.
func TestServingPoolTakesNoSessionSettingFromANamedServiceFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "pg_service.conf")
	content := "[billing]\napplication_name=from-the-service-file\noptions=-c forge.tenant_id=0190d6c5-5f1a-7d0e-8b1a-6a3c1f2e9d4b\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := "postgres://app:pw@db.internal:5432/app?service=billing&servicefile=" + url.QueryEscape(path)
	pool, err := parseConfig(databaseAt(raw), stripEnvironmentSession)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"options", "application_name"} {
		if value, ok := pool.ConnConfig.RuntimeParams[key]; ok {
			t.Errorf("the serving pool took %s=%q from the service file", key, value)
		}
	}
	migrator, err := parseConfig(databaseAt(raw), keepEnvironmentSession)
	if err != nil {
		t.Fatal(err)
	}
	if got := migrator.ConnConfig.RuntimeParams["options"]; !strings.Contains(got, "forge.tenant_id") {
		t.Errorf("the migrator lost the service file's options: %q", got)
	}
}
