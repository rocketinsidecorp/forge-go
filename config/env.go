package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

func Load() (Config, error) { return LoadWithLookup(os.LookupEnv) }

func LoadWithLookup(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, fmt.Errorf("environment lookup is required")
	}
	cfg := Default()
	setString(lookup, "FORGE_HTTP_ADDR", &cfg.HTTP.Address)
	setString(lookup, "FORGE_HTTP_TLS_CERT_FILE", &cfg.HTTP.TLSCertFile)
	setString(lookup, "FORGE_HTTP_TLS_KEY_FILE", &cfg.HTTP.TLSKeyFile)
	if value, ok := lookup("FORGE_ENV"); ok {
		cfg.Environment = Environment(value)
	}
	if value, ok := lookup("FORGE_HTTP_TRANSPORT"); ok {
		cfg.HTTP.Transport = TransportMode(value)
	}
	if value, ok := lookup("FORGE_HTTP_TRUSTED_PROXIES"); ok {
		prefixes, err := ParseTrustedProxies(value)
		if err != nil {
			return Config{}, err
		}
		cfg.HTTP.TrustedProxies = prefixes
	}
	if value, ok := lookup("FORGE_AUTH_MFA_KEY"); ok {
		cfg.Auth.MFAKey = NewSecret(value)
	}
	setString(lookup, "FORGE_AUTH_PASSWORD_RESET_URL", &cfg.Auth.PasswordResetURL)
	if value, ok := lookup("FORGE_DATABASE_URL"); ok {
		cfg.Database.URL = NewSecret(value)
	}
	if value, ok := lookup("FORGE_MAIL_URL"); ok {
		cfg.Mail.URL = NewSecret(value)
	}
	setString(lookup, "FORGE_MAIL_FROM", &cfg.Mail.From)
	setString(lookup, "FORGE_MAIL_TLS_CA_FILE", &cfg.Mail.CAFile)
	setString(lookup, "FORGE_STORAGE_ROOT", &cfg.Storage.Root)
	setString(lookup, "FORGE_I18N_DEFAULT_LOCALE", &cfg.I18n.DefaultLocale)
	if value, ok := lookup("FORGE_STORAGE_SIGNING_KEY"); ok {
		cfg.Storage.SigningKey = NewSecret(value)
	}

	durations := []struct {
		key    string
		target *time.Duration
	}{
		{"FORGE_HTTP_READ_HEADER_TIMEOUT", &cfg.HTTP.ReadHeaderTimeout},
		{"FORGE_HTTP_READ_TIMEOUT", &cfg.HTTP.ReadTimeout},
		{"FORGE_HTTP_WRITE_TIMEOUT", &cfg.HTTP.WriteTimeout},
		{"FORGE_HTTP_IDLE_TIMEOUT", &cfg.HTTP.IdleTimeout},
		{"FORGE_HTTP_SHUTDOWN_TIMEOUT", &cfg.HTTP.ShutdownTimeout},
		{"FORGE_DATABASE_MAX_CONN_LIFETIME", &cfg.Database.MaxConnLifetime},
		{"FORGE_DATABASE_MAX_CONN_IDLE_TIME", &cfg.Database.MaxConnIdleTime},
		{"FORGE_DATABASE_CONNECT_TIMEOUT", &cfg.Database.ConnectTimeout},
		{"FORGE_DATABASE_STATEMENT_TIMEOUT", &cfg.Database.StatementTimeout},
		{"FORGE_MAIL_TIMEOUT", &cfg.Mail.Timeout},
		{"FORGE_CACHE_DEFAULT_TTL", &cfg.Cache.DefaultTTL},
		{"FORGE_STORAGE_LINK_TTL", &cfg.Storage.LinkTTL},
	}
	for _, item := range durations {
		if value, ok := lookup(item.key); ok {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return Config{}, invalidValue(item.key, "a duration such as 15s")
			}
			*item.target = parsed
		}
	}
	if value, ok := lookup("FORGE_HTTP_MAX_HEADER_BYTES"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return Config{}, invalidValue("FORGE_HTTP_MAX_HEADER_BYTES", "an integer")
		}
		cfg.HTTP.MaxHeaderBytes = parsed
	}
	if value, ok := lookup("FORGE_HTTP_MAX_BODY_BYTES"); ok {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return Config{}, invalidValue("FORGE_HTTP_MAX_BODY_BYTES", "an integer")
		}
		cfg.HTTP.MaxBodyBytes = parsed
	}
	for key, target := range map[string]*int32{
		"FORGE_DATABASE_MAX_CONNS": &cfg.Database.MaxConns,
		"FORGE_DATABASE_MIN_CONNS": &cfg.Database.MinConns,
	} {
		if value, ok := lookup(key); ok {
			parsed, err := strconv.ParseInt(value, 10, 32)
			if err != nil {
				return Config{}, invalidValue(key, "an integer")
			}
			*target = int32(parsed)
		}
	}
	positive := []struct {
		key    string
		target any
	}{
		{"FORGE_RATE_LIMIT_MAX_KEYS", &cfg.RateLimit.MaxKeys},
		{"FORGE_CACHE_MAX_ENTRIES", &cfg.Cache.MaxEntries},
		{"FORGE_CACHE_MAX_VALUE_BYTES", &cfg.Cache.MaxValueBytes},
		{"FORGE_CACHE_MAX_BYTES", &cfg.Cache.MaxBytes},
		{"FORGE_STORAGE_MAX_UPLOAD_BYTES", &cfg.Storage.MaxUploadBytes},
	}
	for _, item := range positive {
		value, ok := lookup(item.key)
		if !ok {
			continue
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 1 {
			return Config{}, invalidValue(item.key, "a positive integer")
		}
		switch target := item.target.(type) {
		case *int:
			if int64(int(parsed)) != parsed {
				return Config{}, invalidValue(item.key, "a positive integer")
			}
			*target = int(parsed)
		case *int64:
			*target = parsed
		}
	}
	if value, ok := lookup("FORGE_DATABASE_ALLOW_PLAINTEXT"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, invalidValue("FORGE_DATABASE_ALLOW_PLAINTEXT", "true or false")
		}
		cfg.Database.AllowPlaintext = parsed
	}
	if value, ok := lookup("FORGE_MAIL_ALLOW_PLAINTEXT"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, invalidValue("FORGE_MAIL_ALLOW_PLAINTEXT", "true or false")
		}
		cfg.Mail.AllowPlaintext = parsed
	}
	if value, ok := lookup("FORGE_MAIL_PREVIEW"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, invalidValue("FORGE_MAIL_PREVIEW", "true or false")
		}
		cfg.Mail.Preview = parsed
	}
	if value, ok := lookup("FORGE_OPENAPI_ENABLED"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, invalidValue("FORGE_OPENAPI_ENABLED", "true or false")
		}
		cfg.OpenAPI.Enabled = &parsed
	}
	if err := loadTelemetry(lookup, &cfg.Telemetry); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func setString(lookup func(string) (string, bool), key string, target *string) {
	if value, ok := lookup(key); ok {
		*target = value
	}
}

// invalidValue reports a malformed variable without echoing its value, which
// may be a misplaced secret.
func invalidValue(key, expected string) error {
	return fmt.Errorf("invalid %s: expected %s", key, expected)
}
