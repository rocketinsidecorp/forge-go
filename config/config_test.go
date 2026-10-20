package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
)

func TestLoadWithLookup(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"FORGE_ENV":                      "staging",
		"FORGE_HTTP_ADDR":                "0.0.0.0:9000",
		"FORGE_HTTP_TRANSPORT":           "trusted-proxy",
		"FORGE_HTTP_READ_HEADER_TIMEOUT": "3s",
		"FORGE_HTTP_MAX_BODY_BYTES":      "2048",
		"FORGE_AUTH_MFA_KEY":             "example-mfa-key",
	}
	cfg, err := config.LoadWithLookup(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Environment != config.Staging || cfg.HTTP.Address != "0.0.0.0:9000" || cfg.HTTP.ReadHeaderTimeout != 3*time.Second || cfg.HTTP.MaxBodyBytes != 2048 {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if cfg.Auth.MFAKey.Reveal() != "example-mfa-key" || strings.Contains(cfg.Auth.MFAKey.String(), "example-mfa-key") {
		t.Fatalf("MFA key was not loaded/redacted")
	}
}

func TestConfigValidateProductionTransport(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Environment = config.Production
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected plain production transport to fail")
	}
	cfg.HTTP.Transport = config.TransportTrustedProxy
	if err := cfg.Validate(); err != nil {
		t.Fatalf("trusted proxy production transport: %v", err)
	}
}

func TestLoadRejectsInvalidValuesWithoutSecretLeak(t *testing.T) {
	t.Parallel()
	secret := "super-secret-value"
	_, err := config.LoadWithLookup(func(key string) (string, bool) {
		if key == "FORGE_HTTP_READ_TIMEOUT" {
			return secret, true
		}
		return "", false
	})
	if err == nil {
		t.Fatal("expected invalid duration error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("configuration error disclosed value")
	}
}

func TestDatabaseConfiguration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{name: "no database", env: map[string]string{}},
		{name: "development plaintext", env: map[string]string{"FORGE_DATABASE_URL": "postgres://app:pw@localhost:5432/app?sslmode=disable"}},
		{name: "production verify-full", env: map[string]string{"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_DATABASE_URL": "postgres://app:pw@db:5432/app?sslmode=verify-full"}},
		{name: "production verify-ca with private CA", env: map[string]string{"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_DATABASE_URL": "postgres://app:pw@db:5432/app?sslmode=verify-ca&sslrootcert=/etc/ssl/db-ca.pem"}},
		{name: "production require is unverified", env: map[string]string{"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_DATABASE_URL": "postgres://app:pw@db:5432/app?sslmode=require"}, wantErr: true},
		{name: "production default sslmode", env: map[string]string{"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_DATABASE_URL": "postgres://app:pw@db:5432/app"}, wantErr: true},
		{name: "production prefer", env: map[string]string{"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_DATABASE_URL": "postgres://app:pw@db/app?sslmode=prefer"}, wantErr: true},
		{name: "production explicit plaintext", env: map[string]string{"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_DATABASE_URL": "postgres://app:pw@db/app?sslmode=disable", "FORGE_DATABASE_ALLOW_PLAINTEXT": "true"}},
		{name: "unsupported scheme", env: map[string]string{"FORGE_DATABASE_URL": "oracle://app:pw@db/app"}, wantErr: true},
		{name: "mysql development", env: map[string]string{"FORGE_DATABASE_URL": "mysql://app:pw@localhost:3306/app"}},
		{name: "mysql production tls", env: map[string]string{"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_DATABASE_URL": "mysql://app:pw@db:3306/app?tls=true"}},
		{name: "mysql production skip-verify", env: map[string]string{"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_DATABASE_URL": "mysql://app:pw@db:3306/app?tls=skip-verify"}, wantErr: true},
		{name: "mysql production plaintext", env: map[string]string{"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_DATABASE_URL": "mysql://app:pw@db:3306/app"}, wantErr: true},
		{name: "mysql without database", env: map[string]string{"FORGE_DATABASE_URL": "mysql://app:pw@db:3306/"}, wantErr: true},
		{name: "sqlite relative", env: map[string]string{"FORGE_DATABASE_URL": "sqlite:storage/development.db"}},
		{name: "sqlite absolute in production", env: map[string]string{"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_DATABASE_URL": "sqlite:///app/storage/production.db"}},
		{name: "sqlite memory", env: map[string]string{"FORGE_DATABASE_URL": "sqlite::memory:"}, wantErr: true},
		{name: "sqlite pragmas", env: map[string]string{"FORGE_DATABASE_URL": "sqlite:app.db?_pragma=foreign_keys(0)"}, wantErr: true},
		{name: "sqlite with host", env: map[string]string{"FORGE_DATABASE_URL": "sqlite://host/app.db"}, wantErr: true},
		{name: "missing host", env: map[string]string{"FORGE_DATABASE_URL": "postgres:///app"}, wantErr: true},
		{name: "pool bounds", env: map[string]string{"FORGE_DATABASE_MAX_CONNS": "0"}, wantErr: true},
		{name: "min above max", env: map[string]string{"FORGE_DATABASE_MAX_CONNS": "2", "FORGE_DATABASE_MIN_CONNS": "3"}, wantErr: true},
		{name: "int32 overflow", env: map[string]string{"FORGE_DATABASE_MAX_CONNS": "99999999999"}, wantErr: true},
		{name: "bad bool", env: map[string]string{"FORGE_DATABASE_ALLOW_PLAINTEXT": "yes please"}, wantErr: true},
		{name: "statement timeout", env: map[string]string{"FORGE_DATABASE_STATEMENT_TIMEOUT": "0s"}, wantErr: true},
		{name: "sub-millisecond statement timeout", env: map[string]string{"FORGE_DATABASE_STATEMENT_TIMEOUT": "500us"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.LoadWithLookup(func(key string) (string, bool) { value, ok := test.env[key]; return value, ok })
			if (err != nil) != test.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestMalformedDatabaseURLDoesNotLeakPassword(t *testing.T) {
	t.Parallel()
	// %zz is an invalid escape: url.Parse fails and its error quotes the input.
	_, err := config.LoadWithLookup(func(key string) (string, bool) {
		if key == "FORGE_DATABASE_URL" {
			return "postgres://app:hunter2%zz@db/app", true
		}
		return "", false
	})
	if err == nil {
		t.Fatal("expected malformed URL to fail")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaked the password: %v", err)
	}
}

func TestDatabaseAdapterSelection(t *testing.T) {
	t.Parallel()
	tests := map[string]config.Adapter{
		"":                                    "",
		"postgres://app:pw@db/app":            config.AdapterPostgres,
		"postgresql://app:pw@db/app":          config.AdapterPostgres,
		"mysql://app:pw@db:3306/app":          config.AdapterMySQL,
		"sqlite:storage/development.db":       config.AdapterSQLite,
		"sqlite:///app/storage/production.db": config.AdapterSQLite,
	}
	for raw, want := range tests {
		database := config.Default().Database
		database.URL = config.NewSecret(raw)
		if got := database.Adapter(); got != want {
			t.Errorf("%q: adapter=%q want %q", raw, got, want)
		}
	}
	database := config.Default().Database
	for raw, want := range map[string]string{
		"sqlite:storage/development.db":       "storage/development.db",
		"sqlite:///app/storage/production.db": "/app/storage/production.db",
		"sqlite:my%20app.db":                  "my app.db",
		"sqlite:///data/my%20app.db":          "/data/my app.db",
	} {
		database.URL = config.NewSecret(raw)
		if got := database.SQLitePath(); got != want {
			t.Errorf("%q: path=%q want %q", raw, got, want)
		}
	}
}

// TestAmbiguousDatabaseURLsAreRejected is a regression test: net/url and
// pgx read these URLs differently (first vs last repeated key, "@" in the
// query ending the userinfo for pgx), which let a plaintext connection pass
// the production TLS check.
func TestAmbiguousDatabaseURLsAreRejected(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"postgres://app:pw@db:5432/app?sslmode=verify-full&sslmode=disable",
		"postgres://db.internal?sslmode=verify-full&x=@10.0.0.9:5432/app",
		"postgres://app:pw@db1,db2:5432/app?sslmode=verify-full",
		"postgres://app:pw@db:5432/app?sslmode=verify-full#frag",
		"mysql://app:pw@db:3306/app?tls=true#frag",
		"sqlite:///x/d.db#_foreign_keys=0",
		// net/url keeps the spaces in the key; pgx trims them and obeys it.
		"postgres://app:pw@db:5432/app?sslmode=verify-full& host=10.0.0.9",
		"postgres://app:pw@db:5432/app?sslmode=verify-full&host =10.0.0.9",
		"postgres://app:pw@db:5432/app?sslmode=verify-full& sslmode=disable",
		"postgres://app:pw@db:5432/app?sslmode=verify-full&\u00a0port=6543",
		"mysql://app:pw@db:3306/app?tls=true ",
	} {
		_, err := config.LoadWithLookup(func(key string) (string, bool) {
			switch key {
			case "FORGE_ENV":
				return "production", true
			case "FORGE_HTTP_TRANSPORT":
				return "trusted-proxy", true
			case "FORGE_DATABASE_URL":
				return raw, true
			}
			return "", false
		})
		if err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

func TestPasswordResetURLIsValidated(t *testing.T) {
	t.Parallel()
	for value, valid := range map[string]bool{
		"":                                       true,
		"https://app.example.com/passwords/edit": true,
		"http://localhost:8080/passwords/edit":   true,
		"http://app.example.com/passwords/edit":  false,
		"https://user:pw@app.example.com/x":      false,
		"https://app.example.com/x?token=1":      false,
		"https://app.example.com/x#token":        false,
		"/passwords/edit":                        false,
		"javascript:alert(1)":                    false,
	} {
		cfg, err := config.LoadWithLookup(func(key string) (string, bool) {
			if key == "FORGE_AUTH_PASSWORD_RESET_URL" {
				return value, true
			}
			return "", false
		})
		if (err == nil) != valid {
			t.Errorf("FORGE_AUTH_PASSWORD_RESET_URL=%q: err=%v, want valid=%v", value, err, valid)
		}
		if err == nil && cfg.Auth.PasswordResetURL != value {
			t.Errorf("FORGE_AUTH_PASSWORD_RESET_URL=%q loaded as %q", value, cfg.Auth.PasswordResetURL)
		}
	}
}
