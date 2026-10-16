// Package config loads and validates Forge runtime configuration.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"time"
)

type Environment string

const (
	Development Environment = "development"
	Test        Environment = "test"
	Staging     Environment = "staging"
	Production  Environment = "production"
)

type TransportMode string

const (
	TransportPlain        TransportMode = "plain"
	TransportTLS          TransportMode = "tls"
	TransportTrustedProxy TransportMode = "trusted-proxy"
)

type HTTP struct {
	Address           string
	Transport         TransportMode
	TrustedProxyCIDRs []netip.Prefix
	TLSCertFile       string
	TLSKeyFile        string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	MaxHeaderBytes    int
	MaxBodyBytes      int64
}

// Database configures the database connection pool. The URL's scheme selects
// the adapter (see Adapter). An empty URL means the application runs without
// a database.
// Auth contains optional secrets for identity features. Zero values keep the
// corresponding feature disabled rather than inventing insecure defaults.
type Auth struct {
	// MFAKey is the canonical unpadded base64url encoding of 32 random bytes
	// used by the built-in AES-256-GCM TOTP secret cipher.
	MFAKey Secret
}

type Database struct {
	// URL is a postgres://, mysql:// or sqlite: URL. It usually embeds a
	// password.
	URL             Secret
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ConnectTimeout  time.Duration
	// StatementTimeout bounds statements server-side: every statement on
	// PostgreSQL and MariaDB, SELECT only on MySQL. SQLite has no server; its
	// queries stop through context cancellation.
	StatementTimeout time.Duration
	// AllowPlaintext permits production connections without TLS, for
	// databases reachable only over a trusted private network.
	AllowPlaintext bool
}

type Config struct {
	Environment Environment
	HTTP        HTTP
	Auth        Auth
	Database    Database
}

func Default() Config {
	return Config{
		Environment: Development,
		HTTP: HTTP{
			Address:           "127.0.0.1:8080",
			Transport:         TransportPlain,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
			ShutdownTimeout:   30 * time.Second,
			MaxHeaderBytes:    1 << 20,
			MaxBodyBytes:      1 << 20,
		},
		Database: Database{
			MaxConns:         10,
			MinConns:         0,
			MaxConnLifetime:  30 * time.Minute,
			MaxConnIdleTime:  5 * time.Minute,
			ConnectTimeout:   5 * time.Second,
			StatementTimeout: 30 * time.Second,
		},
	}
}

func (c Config) Validate() error {
	switch c.Environment {
	case Development, Test, Staging, Production:
	default:
		return fmt.Errorf("unsupported FORGE_ENV %q", c.Environment)
	}
	switch c.HTTP.Transport {
	case TransportPlain, TransportTLS, TransportTrustedProxy:
	default:
		return fmt.Errorf("unsupported FORGE_HTTP_TRANSPORT %q", c.HTTP.Transport)
	}
	if c.Environment == Production && c.HTTP.Transport == TransportPlain {
		return errors.New("production requires TLS or explicitly trusted proxy TLS termination")
	}
	if c.HTTP.Transport == TransportTLS && (c.HTTP.TLSCertFile == "" || c.HTTP.TLSKeyFile == "") {
		return errors.New("TLS transport requires certificate and key files")
	}
	for _, prefix := range c.HTTP.TrustedProxyCIDRs {
		if !prefix.IsValid() || prefix.Bits() == 0 || prefix.Addr().Is4In6() {
			return errors.New("trusted proxy CIDRs must be valid, specific IPv4 or IPv6 prefixes")
		}
	}
	if c.HTTP.Address == "" {
		return errors.New("HTTP address is required")
	}
	if c.HTTP.ReadHeaderTimeout <= 0 || c.HTTP.ReadTimeout <= 0 || c.HTTP.WriteTimeout <= 0 ||
		c.HTTP.IdleTimeout <= 0 || c.HTTP.ShutdownTimeout <= 0 {
		return errors.New("HTTP timeouts must be positive")
	}
	if c.HTTP.MaxHeaderBytes <= 0 || c.HTTP.MaxBodyBytes <= 0 {
		return errors.New("HTTP size limits must be positive")
	}
	return c.Database.validate(c.Environment)
}
