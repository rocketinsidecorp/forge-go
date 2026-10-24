package mail

import (
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/rocketinsidecorp/forge-go/config"
)

// ErrNotConfigured is returned by FromConfig when FORGE_MAIL_URL is empty.
// Applications treat mail as optional by checking errors.Is(err,
// ErrNotConfigured); features that need it (password recovery) should then
// stay disabled rather than pretend to send.
var ErrNotConfigured = errors.New("mail: FORGE_MAIL_URL is not configured")

// FromConfigOption customises FromConfig.
type FromConfigOption func(*fromConfig)

type fromConfig struct {
	logger    *slog.Logger
	helloName string
}

// WithLogger sets the logger used by the development file sender.
func WithLogger(logger *slog.Logger) FromConfigOption {
	return func(c *fromConfig) { c.logger = logger }
}

// WithHelloName sets the name announced in EHLO.
func WithHelloName(name string) FromConfigOption {
	return func(c *fromConfig) { c.helloName = name }
}

// FromConfig builds the Sender selected by cfg.Mail: an SMTP sender for
// smtp:// (STARTTLS) and smtps:// (implicit TLS), the development file sender
// for file://. It repeats the production rules of config validation so a
// hand-built Config cannot bypass them: the file sender is refused in
// production, and the SMTP sender requires verified TLS unless
// AllowPlaintext is set. Production with AllowPlaintext and no credentials
// is accepted (an internal relay) but logs a warning: nothing then stops a
// network attacker from stripping STARTTLS, so the mail travels in clear.
func FromConfig(cfg config.Config, options ...FromConfigOption) (Sender, error) {
	var settings fromConfig
	for _, option := range options {
		option(&settings)
	}
	switch cfg.Mail.Transport() {
	case config.MailDisabled:
		if cfg.Mail.URL.IsZero() {
			return nil, ErrNotConfigured
		}
		return nil, errors.New("mail: FORGE_MAIL_URL is invalid")
	case config.MailFile:
		if cfg.Environment == config.Production {
			return nil, errors.New("mail: the development file sender cannot be used in production")
		}
		return NewFileSender(cfg.Mail.FilePath(), settings.logger)
	default:
		endpoint, ok := cfg.Mail.SMTP()
		if !ok {
			return nil, errors.New("mail: FORGE_MAIL_URL is invalid")
		}
		pool, err := loadRootCAs(cfg.Mail.CAFile)
		if err != nil {
			return nil, err
		}
		if plaintextWithoutCredentialsInProduction(cfg, endpoint) {
			logger := settings.logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("FORGE_MAIL_ALLOW_PLAINTEXT=true in production without credentials: mail may be sent unencrypted, and a network attacker can strip STARTTLS")
		}
		return NewSMTPSender(SMTPConfig{
			Host:           endpoint.Host,
			Port:           endpoint.Port,
			ImplicitTLS:    endpoint.ImplicitTLS,
			Username:       endpoint.Username,
			Password:       endpoint.Password,
			HelloName:      settings.helloName,
			RootCAs:        pool,
			AllowPlaintext: cfg.Mail.AllowPlaintext,
			Timeout:        cfg.Mail.EffectiveTimeout(),
		})
	}
}

func plaintextWithoutCredentialsInProduction(cfg config.Config, endpoint config.MailEndpoint) bool {
	return cfg.Environment == config.Production && cfg.Mail.AllowPlaintext && endpoint.Username == ""
}

// DefaultFrom parses FORGE_MAIL_FROM into an Address.
func DefaultFrom(cfg config.Config) (Address, error) {
	if cfg.Mail.From == "" {
		return Address{}, errors.New("mail: FORGE_MAIL_FROM is not configured")
	}
	address, err := ParseAddress(cfg.Mail.From)
	if err != nil {
		return Address{}, errors.New("mail: FORGE_MAIL_FROM is not a valid sender address")
	}
	return address, nil
}

// loadRootCAs returns the system roots extended with the PEM bundle at path.
// An empty path means the system roots alone (nil pool).
func loadRootCAs(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	pem, err := os.ReadFile(path) // #nosec G304 -- operator-configured trust bundle
	if err != nil {
		return nil, fmt.Errorf("mail: read FORGE_MAIL_TLS_CA_FILE: %w", err)
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("mail: FORGE_MAIL_TLS_CA_FILE contains no PEM certificates")
	}
	return pool, nil
}
