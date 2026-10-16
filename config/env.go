package config

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
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
		prefixes, err := parseTrustedProxyCIDRs(value)
		if err != nil {
			return Config{}, invalidValue("FORGE_HTTP_TRUSTED_PROXIES", "comma-separated IPv4 or IPv6 CIDRs")
		}
		cfg.HTTP.TrustedProxyCIDRs = prefixes
	}
	if value, ok := lookup("FORGE_AUTH_MFA_KEY"); ok {
		cfg.Auth.MFAKey = NewSecret(value)
	}
	if value, ok := lookup("FORGE_DATABASE_URL"); ok {
		cfg.Database.URL = NewSecret(value)
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
	if value, ok := lookup("FORGE_DATABASE_ALLOW_PLAINTEXT"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, invalidValue("FORGE_DATABASE_ALLOW_PLAINTEXT", "true or false")
		}
		cfg.Database.AllowPlaintext = parsed
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

func parseTrustedProxyCIDRs(value string) ([]netip.Prefix, error) {
	parts := strings.Split(value, ",")
	prefixes := make([]netip.Prefix, 0, len(parts))
	for _, part := range parts {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
		prefix = prefix.Masked()
		if !prefix.IsValid() || prefix.Bits() == 0 || prefix.Addr().Is4In6() {
			return nil, fmt.Errorf("invalid trusted proxy CIDR")
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}
