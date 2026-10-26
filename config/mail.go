package config

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// MailTransport identifies how FORGE_MAIL_URL's scheme delivers mail.
type MailTransport string

const (
	// MailDisabled means no mail URL is configured.
	MailDisabled MailTransport = ""
	// MailSTARTTLS is smtp://: connect, then upgrade with STARTTLS (port 587).
	MailSTARTTLS MailTransport = "smtp"
	// MailImplicitTLS is smtps://: TLS from the first byte (port 465).
	MailImplicitTLS MailTransport = "smtps"
	// MailFile is file://: the development sender writing .eml files. It is
	// refused in production.
	MailFile MailTransport = "file"
)

const (
	defaultMailTimeout = 30 * time.Second
	maxMailTimeout     = 10 * time.Minute
)

// Mail configures outgoing email. An empty URL means the application sends no
// mail (features that need it, such as password recovery, fail closed).
type Mail struct {
	// URL selects and configures the transport and usually embeds a
	// password:
	//
	//	smtp://user:password@smtp.example.com:587   STARTTLS (default port 587)
	//	smtps://user:password@smtp.example.com:465  implicit TLS (default 465)
	//	file:///var/tmp/forge-mail                  development .eml files
	//
	// Percent-encode "@", ":" and "/" inside the user name and password.
	URL Secret
	// From is the default sender, "name@example.com" or
	// "Name <name@example.com>". It is required when URL is set.
	From string
	// AllowPlaintext lets the SMTP sender proceed when the server does not
	// offer STARTTLS, including authentication over that connection. Use only
	// on a trusted private network or against a local development server.
	AllowPlaintext bool
	// Timeout bounds one delivery attempt. Zero means 30 seconds.
	Timeout time.Duration
	// Preview enables the development mail preview handler
	// (FORGE_MAIL_PREVIEW=true). It must be set deliberately: an unset
	// FORGE_ENV means development, so the environment alone is not a safe
	// gate. Only valid in development and test.
	Preview bool
	// CAFile is a PEM bundle of extra trusted roots for a private mail relay.
	// There is no setting to skip certificate verification.
	CAFile string
}

// MailEndpoint is the SMTP server described by Mail.URL.
type MailEndpoint struct {
	Host     string
	Port     int
	Username string
	Password Secret
	// ImplicitTLS is true for smtps://.
	ImplicitTLS bool
}

// Transport reports the transport the URL selects, or MailDisabled.
func (m Mail) Transport() MailTransport {
	transport, _ := parseMailURL(m.URL.Reveal())
	return transport
}

// SMTP returns the server of an smtp:// or smtps:// URL.
func (m Mail) SMTP() (MailEndpoint, bool) {
	transport, parsed := parseMailURL(m.URL.Reveal())
	if parsed == nil || (transport != MailSTARTTLS && transport != MailImplicitTLS) {
		return MailEndpoint{}, false
	}
	endpoint := MailEndpoint{Host: parsed.Hostname(), ImplicitTLS: transport == MailImplicitTLS}
	endpoint.Port = 587
	if endpoint.ImplicitTLS {
		endpoint.Port = 465
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil {
			return MailEndpoint{}, false
		}
		endpoint.Port = value
	}
	if parsed.User != nil {
		endpoint.Username = parsed.User.Username()
		password, _ := parsed.User.Password()
		endpoint.Password = NewSecret(password)
	}
	return endpoint, true
}

// FilePath returns the directory of a file:// URL, or "".
func (m Mail) FilePath() string {
	transport, parsed := parseMailURL(m.URL.Reveal())
	if transport != MailFile {
		return ""
	}
	if parsed.Opaque != "" { // file:tmp/mails
		path, err := url.PathUnescape(parsed.Opaque)
		if err != nil {
			return ""
		}
		return path
	}
	return parsed.Path // the directory of a URL such as file:///var/tmp/forge-mail
}

// EffectiveTimeout returns Timeout, or the 30 second default when unset.
func (m Mail) EffectiveTimeout() time.Duration {
	if m.Timeout == 0 {
		return defaultMailTimeout
	}
	return m.Timeout
}

func parseMailURL(raw string) (MailTransport, *url.URL) {
	// Never surface url.Parse errors: they quote the URL, password included.
	parsed, err := url.Parse(raw)
	if err != nil {
		return MailDisabled, nil
	}
	switch parsed.Scheme {
	case "smtp":
		return MailSTARTTLS, parsed
	case "smtps":
		return MailImplicitTLS, parsed
	case "file":
		return MailFile, parsed
	default:
		return MailDisabled, nil
	}
}

func (m Mail) validate(environment Environment) error {
	if m.Preview && environment != Development && environment != Test {
		return errors.New("FORGE_MAIL_PREVIEW is only allowed when FORGE_ENV is development or test")
	}
	if m.Timeout < 0 || m.Timeout > maxMailTimeout {
		return errors.New("FORGE_MAIL_TIMEOUT must be positive and at most 10m")
	}
	if strings.ContainsRune(m.CAFile, 0) {
		return errors.New("FORGE_MAIL_TLS_CA_FILE is invalid")
	}
	if m.URL.IsZero() {
		return nil
	}
	transport, parsed := parseMailURL(m.URL.Reveal())
	if parsed == nil {
		return errors.New("FORGE_MAIL_URL must be smtp://user:password@host:587, smtps://user:password@host:465 or file:///path")
	}
	if parsed.Fragment != "" || strings.Contains(parsed.RawQuery, "@") || strings.Contains(parsed.Path, "@") {
		return errors.New("FORGE_MAIL_URL is ambiguous: percent-encode \"@\" and \"#\" in passwords")
	}
	if parsed.RawQuery != "" {
		return errors.New("FORGE_MAIL_URL takes no parameters: TLS is always required and verified")
	}
	if m.From == "" {
		return errors.New("FORGE_MAIL_FROM is required when FORGE_MAIL_URL is set")
	}
	// The same rules the mail package applies to every address, so a value
	// that passes here cannot fail on every send later.
	if _, _, err := ParseMailbox(m.From); err != nil {
		return errors.New("FORGE_MAIL_FROM must be a plain ASCII email address such as \"App <app@example.com>\"")
	}
	switch transport {
	case MailSTARTTLS, MailImplicitTLS:
		if parsed.Hostname() == "" || strings.Contains(parsed.Host, ",") {
			return errors.New("FORGE_MAIL_URL must be smtp://user:password@host:port (one host)")
		}
		if path := strings.Trim(parsed.Path, "/"); path != "" || parsed.Opaque != "" {
			return errors.New("FORGE_MAIL_URL must not have a path")
		}
		if port := parsed.Port(); port != "" {
			value, err := strconv.Atoi(port)
			if err != nil || value < 1 || value > 65535 {
				return errors.New("FORGE_MAIL_URL has an invalid port")
			}
		}
		if environment == Production && m.AllowPlaintext && parsed.User != nil && !LoopbackHost(parsed.Hostname()) {
			// SMTP AUTH PLAIN/LOGIN reveals the password to anyone who can
			// strip STARTTLS from the exchange, unlike a database login
			// that uses a challenge-response scheme. The sender refuses to
			// authenticate in clear to a non-loopback host regardless, so
			// this combination could only ever fail; reject it at start-up.
			return errors.New("production cannot combine FORGE_MAIL_ALLOW_PLAINTEXT with SMTP credentials for a remote host: " +
				"credentials are only sent over verified TLS")
		}
		if parsed.User != nil {
			password, hasPassword := parsed.User.Password()
			if parsed.User.Username() == "" || !hasPassword || password == "" {
				return errors.New("FORGE_MAIL_URL credentials need both a user name and a password")
			}
			if hasControl(parsed.User.Username()) || hasControl(password) {
				return errors.New("FORGE_MAIL_URL credentials contain control characters")
			}
		}
	case MailFile:
		if environment == Production {
			return errors.New("production cannot use the development file mail sender: " +
				"set FORGE_MAIL_URL to an smtp:// or smtps:// server")
		}
		if parsed.Host != "" || m.FilePath() == "" || m.FilePath() == "/" {
			return errors.New("FORGE_MAIL_URL for files must be file:///absolute/dir or file:relative/dir")
		}
	}
	return nil
}

// hasControl reports whether s contains a control character, which a
// percent-encoded %0d%0a could smuggle into an SMTP dialogue.
func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

// LoopbackHost reports whether host is "localhost", a 127.0.0.0/8 address or
// ::1: a destination the traffic never leaves the machine for. Host names
// other than "localhost" are not resolved, so they are never loopback here.
func LoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
