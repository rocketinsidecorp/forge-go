// Package config loads and validates Forge runtime configuration.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/i18n"
)

// Environment is the deployment environment selected by FORGE_ENV.
type Environment string

// The supported environments. Production and staging are stricter about transport security and
// fallbacks.
const (
	Development Environment = "development"
	Test        Environment = "test"
	Staging     Environment = "staging"
	Production  Environment = "production"
)

// TransportMode is how the server terminates HTTP, selected by FORGE_HTTP_TRANSPORT.
type TransportMode string

// The supported transports: plain HTTP, TLS terminated by the server, and plain HTTP behind a
// trusted TLS-terminating proxy.
const (
	TransportPlain        TransportMode = "plain"
	TransportTLS          TransportMode = "tls"
	TransportTrustedProxy TransportMode = "trusted-proxy"
)

// HTTP configures the HTTP server: address, transport, timeouts and size limits.
type HTTP struct {
	Address           string
	Transport         TransportMode
	TLSCertFile       string
	TLSKeyFile        string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	MaxHeaderBytes    int
	MaxBodyBytes      int64
	// TrustedProxies are the networks of the reverse proxies whose
	// X-Forwarded-For header is believed when resolving the client address
	// (FORGE_HTTP_TRUSTED_PROXIES, ADR 0037). Empty, the default, trusts
	// nobody: the client address is the TCP peer.
	TrustedProxies []netip.Prefix
}

// Auth contains optional secrets for identity features. Zero values keep the
// corresponding feature disabled rather than inventing insecure defaults.
type Auth struct {
	// MFAKey is the canonical unpadded base64url encoding of 32 random bytes
	// used by the built-in AES-256-GCM TOTP secret cipher.
	MFAKey Secret
	// PasswordResetURL is the absolute URL of the page password-reset emails
	// link to, such as https://app.example.com/passwords/edit
	// (FORGE_AUTH_PASSWORD_RESET_URL). It must be https, or http on a
	// loopback host for development. Empty disables reset emails.
	PasswordResetURL string
	// SessionIdleTimeout ends a session nobody used for this long, before
	// its absolute expiry (FORGE_AUTH_SESSION_IDLE_TIMEOUT, default 2h,
	// between 5m and 720h). auth/postgres.WithSessionIdleTimeout applies it.
	SessionIdleTimeout time.Duration
}

// Bounds of Auth.SessionIdleTimeout: at least five minutes, at most the
// longest session lifetime (30 days), where it no longer shortens anything.
const (
	defaultSessionIdleTimeout = 2 * time.Hour
	minSessionIdleTimeout     = 5 * time.Minute
	maxSessionIdleTimeout     = 30 * 24 * time.Hour
)

func (a Auth) validate() error {
	if a.SessionIdleTimeout < minSessionIdleTimeout || a.SessionIdleTimeout > maxSessionIdleTimeout {
		return errors.New("FORGE_AUTH_SESSION_IDLE_TIMEOUT must be between 5m and 720h")
	}
	if a.PasswordResetURL == "" {
		return nil
	}
	parsed, err := url.Parse(a.PasswordResetURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" || parsed.Fragment != "" || parsed.RawQuery != "" {
		return errors.New("FORGE_AUTH_PASSWORD_RESET_URL must be an absolute URL without credentials, query or fragment")
	}
	host := parsed.Hostname()
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasSuffix(host, ".localhost")
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !loopback) {
		return errors.New("FORGE_AUTH_PASSWORD_RESET_URL must use https (http is allowed only for localhost)")
	}
	return nil
}

// Database configures the database connection pool. The URL's scheme selects
// the adapter (see Adapter). An empty URL means the application runs without
// a database.
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
	// Environment is the environment the pool serves in. Outside
	// development and test, postgres.Open refuses a role that could bypass
	// row-level security unless postgres.AllowRowSecurityBypass is passed.
	// Load and forge.New copy Config.Environment into it. Default leaves it
	// empty, like a Database built in code: the empty value is neither
	// development nor test, so such a pool is checked (fail closed) until
	// the environment is set explicitly.
	Environment Environment
}

// Config is the application's configuration. Default returns safe defaults; Load reads FORGE_*
// variables over them.
type Config struct {
	Environment Environment
	HTTP        HTTP
	Auth        Auth
	Database    Database
	Mail        Mail
	RateLimit   RateLimit
	Cache       Cache
	Storage     Storage
	Telemetry   Telemetry
	I18n        I18n
	OpenAPI     OpenAPI
}

// OpenAPI configures the GET /openapi.json route (package openapi, ADR 0043).
type OpenAPI struct {
	// Enabled is FORGE_OPENAPI_ENABLED. Nil (unset) serves the document in
	// development and test only: it lists every documented route,
	// including internal ones, so staging and production publish it only
	// when it is set to true.
	Enabled *bool
}

// ServesOpenAPI reports whether the application serves its OpenAPI document
// at GET /openapi.json.
func (c Config) ServesOpenAPI() bool {
	if c.OpenAPI.Enabled != nil {
		return *c.OpenAPI.Enabled
	}
	return c.Environment == Development || c.Environment == Test
}

// I18n configures translations (package i18n, ADR 0042).
type I18n struct {
	// DefaultLocale is the locale requests fall back to
	// (FORGE_I18N_DEFAULT_LOCALE), such as "en" or "pt-BR". Empty lets the
	// application choose; generated applications use the locale given to
	// `forge new --locale`, "en" by default.
	DefaultLocale string
}

func (i I18n) validate() error {
	if i.DefaultLocale == "" {
		return nil
	}
	canonical, err := i18n.CanonicalLocale(i.DefaultLocale)
	if err != nil {
		return errors.New("FORGE_I18N_DEFAULT_LOCALE must be a locale such as en or pt-BR (language[-Script][-REGION])")
	}
	if canonical != i.DefaultLocale {
		return fmt.Errorf("FORGE_I18N_DEFAULT_LOCALE must be spelled %s", canonical)
	}
	return nil
}

// ShowsMissingTranslations reports whether missing translations are shown
// as "translation missing: LOCALE.KEY" (development and test) instead of
// falling back to the default locale (staging and production).
func (c Config) ShowsMissingTranslations() bool {
	return c.Environment == Development || c.Environment == Test
}

// Default returns the defaults: the development environment, plain HTTP on 127.0.0.1:8080,
// conservative timeouts and size limits, and no database.
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
		Auth:      Auth{SessionIdleTimeout: defaultSessionIdleTimeout},
		RateLimit: RateLimit{MaxKeys: defaultRateLimitMax},
		Cache: Cache{
			MaxEntries:    10_000,
			MaxBytes:      64 << 20,
			MaxValueBytes: 1 << 20,
			DefaultTTL:    time.Hour,
		},
		Storage:   Storage{MaxUploadBytes: defaultStorageBytes, LinkTTL: defaultStorageTTL},
		Telemetry: defaultTelemetry(),
	}
}

// Validate reports the first invalid or unsafe setting, such as an unknown environment, plain HTTP
// in production or a missing TLS certificate.
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
	for i, prefix := range c.HTTP.TrustedProxies {
		if err := CheckTrustedProxy(prefix); err != nil {
			return fmt.Errorf("invalid FORGE_HTTP_TRUSTED_PROXIES: entry %d %w", i+1, err)
		}
	}
	if err := c.Database.validate(c.Environment); err != nil {
		return err
	}
	if err := c.Auth.validate(); err != nil {
		return err
	}
	if err := c.Mail.validate(c.Environment); err != nil {
		return err
	}
	if err := c.RateLimit.validate(); err != nil {
		return err
	}
	if err := c.Cache.validate(); err != nil {
		return err
	}
	if err := c.Storage.validate(c.Environment, c.HTTP.MaxBodyBytes); err != nil {
		return err
	}
	if err := c.I18n.validate(); err != nil {
		return err
	}
	return c.Telemetry.validate(c.Environment, c.HTTP.Address)
}
