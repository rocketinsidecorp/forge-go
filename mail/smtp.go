package mail

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/fault"
)

const (
	defaultSubmissionPort = 587
	defaultSMTPSPort      = 465
	defaultSMTPTimeout    = 30 * time.Second
	defaultDialTimeout    = 10 * time.Second
	maxSMTPTimeout        = 10 * time.Minute
)

// SMTPConfig configures an SMTPSender.
//
// The sender is secure by default: it requires TLS (STARTTLS on the
// submission port, or implicit TLS with ImplicitTLS), verifies the server
// certificate and host name, and sends credentials only on an encrypted
// connection. There is no option to skip certificate verification; private
// CAs are trusted through RootCAs.
type SMTPConfig struct {
	// Host is the server's DNS name or IP address. The certificate must be
	// valid for it.
	Host string
	// Port defaults to 587, or 465 with ImplicitTLS.
	Port int
	// ImplicitTLS connects with TLS from the first byte (SMTPS, port 465)
	// instead of upgrading with STARTTLS.
	ImplicitTLS bool
	// Username and Password enable AUTH (PLAIN, or LOGIN when PLAIN is not
	// offered). The password is never logged or returned in errors.
	Username string
	Password config.Secret
	// HelloName is announced in EHLO; it defaults to "localhost". Set it to
	// the sending host's name for relays that check it.
	HelloName string
	// RootCAs are the trusted roots; nil means the system pool.
	RootCAs *x509.CertPool
	// AllowPlaintext lets a send proceed when the server does not offer
	// STARTTLS. Use it only on a trusted network or against a local
	// development server. It never allows credentials in clear: AUTH is sent
	// without TLS only to a loopback host (see config.LoopbackHost); to any
	// other host the send fails with ErrAuthFailed before the password
	// leaves the process. When the server does offer STARTTLS the upgrade is
	// still attempted and verified; a failure never falls back to plaintext.
	AllowPlaintext bool
	// Timeout bounds one whole send (connect, handshake, transfer) and is
	// tightened by the context's deadline. It defaults to 30s.
	Timeout time.Duration
	// DialTimeout bounds the TCP connect. It defaults to 10s.
	DialTimeout time.Duration
}

// SMTPSender delivers mail over SMTP. It opens one connection per message
// and closes it; connection pooling is intentionally not offered because a
// stale pooled connection is the usual source of lost or duplicated mail.
// For volume, put a queue in front (docs/MAIL.md). It is safe for concurrent
// use.
type SMTPSender struct {
	cfg SMTPConfig
	// dialer replaces the network dial in tests.
	dialer func(ctx context.Context, network, address string) (net.Conn, error)
}

// NewSMTPSender validates cfg and returns a sender. It performs no network
// I/O.
func NewSMTPSender(cfg SMTPConfig) (*SMTPSender, error) {
	if cfg.Host == "" || strings.IndexFunc(cfg.Host, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r == '/' || r == '@' }) >= 0 {
		return nil, errors.New("mail: SMTP host is required and must be a bare host name or IP")
	}
	if cfg.Port == 0 {
		cfg.Port = defaultSubmissionPort
		if cfg.ImplicitTLS {
			cfg.Port = defaultSMTPSPort
		}
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, errors.New("mail: SMTP port must be between 1 and 65535")
	}
	if cfg.Username == "" && !cfg.Password.IsZero() {
		return nil, errors.New("mail: SMTP password requires a username")
	}
	if hasControl(cfg.Username) || hasControl(cfg.Password.Reveal()) {
		return nil, errors.New("mail: SMTP credentials contain control characters")
	}
	if cfg.HelloName == "" {
		cfg.HelloName = "localhost"
	}
	if !config.ValidMailDomain(cfg.HelloName) && net.ParseIP(strings.Trim(cfg.HelloName, "[]")) == nil {
		return nil, errors.New("mail: SMTP hello name is not a valid host name")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultSMTPTimeout
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	if cfg.Timeout < 0 || cfg.Timeout > maxSMTPTimeout || cfg.DialTimeout < 0 {
		return nil, errors.New("mail: SMTP timeouts must be positive and at most 10 minutes")
	}
	return &SMTPSender{cfg: cfg}, nil
}

// Send validates and encodes msg, then delivers it. Validation happens before
// the connection is opened, so an invalid message never causes network I/O.
// Delivery is all-or-nothing at the envelope: if the server refuses any
// recipient the transaction is aborted before DATA and ErrRejected is
// returned, so a message is never delivered to a subset by surprise.
func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	encoded, err := Encode(msg)
	if err != nil {
		return err
	}
	return s.deliver(ctx, encoded)
}

func (s *SMTPSender) deliver(ctx context.Context, encoded Encoded) (err error) {
	if err := ctx.Err(); err != nil {
		return transient("connect", 0, err)
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	stage := "connect"
	conn, err := s.dial(ctx)
	if err != nil {
		return s.classify(ctx, stage, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// Cancelling the context closes the connection, which unblocks any read
	// or write in progress.
	stopWatch := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopWatch()
	defer func() { _ = conn.Close() }()

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return s.classify(ctx, "greeting", err)
	}
	defer func() {
		if err != nil {
			_ = client.Close()
		}
	}()

	stage = "ehlo"
	if err := client.Hello(s.cfg.HelloName); err != nil {
		return s.classify(ctx, stage, err)
	}
	secure := s.cfg.ImplicitTLS
	if !secure {
		stage = "starttls"
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(s.tlsConfig()); err != nil {
				return s.classify(ctx, stage, err)
			}
			secure = true
		} else if !s.cfg.AllowPlaintext {
			return withFault(ErrTLSRequired, stage, 0, nil)
		}
	}
	if s.cfg.Username != "" {
		stage = "auth"
		if err := s.authenticate(client, secure); err != nil {
			return s.classify(ctx, stage, err)
		}
	}

	stage = "mail"
	if err := client.Mail(encoded.Sender); err != nil {
		return s.classify(ctx, stage, err)
	}
	stage = "rcpt"
	for _, recipient := range encoded.Recipients {
		if err := client.Rcpt(recipient); err != nil {
			// A refused recipient aborts the transaction: no partial send.
			return s.classify(ctx, stage, err)
		}
	}
	stage = "data"
	writer, err := client.Data()
	if err != nil {
		return s.classify(ctx, stage, err)
	}
	if _, err := writer.Write(encoded.Data); err != nil {
		_ = writer.Close()
		return s.classify(ctx, stage, err)
	}
	if err := writer.Close(); err != nil {
		return s.classify(ctx, stage, err)
	}
	// The message is accepted at this point; a failing QUIT must not turn
	// success into an error that would cause a duplicate on retry.
	_ = client.Quit()
	return nil
}

func (s *SMTPSender) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: s.cfg.Host,
		RootCAs:    s.cfg.RootCAs,
	}
}

func (s *SMTPSender) dial(ctx context.Context) (net.Conn, error) {
	address := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	dialer := &net.Dialer{Timeout: s.cfg.DialTimeout}
	if s.dialer != nil {
		return s.dialer(ctx, "tcp", address)
	}
	if s.cfg.ImplicitTLS {
		return (&tls.Dialer{NetDialer: dialer, Config: s.tlsConfig()}).DialContext(ctx, "tcp", address)
	}
	return dialer.DialContext(ctx, "tcp", address)
}

func (s *SMTPSender) authenticate(client *smtp.Client, secure bool) error {
	// Credentials leave the machine only over verified TLS. AllowPlaintext
	// relaxes that for the mail itself, never for the password: a network
	// attacker who strips the STARTTLS advertisement would otherwise capture
	// it. The one exception is a loopback server (a local relay, Mailpit).
	clearAuthOK := s.cfg.AllowPlaintext && config.LoopbackHost(s.cfg.Host)
	if !secure && !clearAuthOK {
		return errAuthPlaintext
	}
	mechanisms := authMechanisms(client)
	if len(mechanisms) == 0 {
		return errAuthUnsupported
	}
	password := s.cfg.Password.Reveal()
	switch {
	case containsString(mechanisms, "PLAIN"):
		return client.Auth(&tlsOnlyAuth{inner: plainAuth{username: s.cfg.Username, password: password}, allowPlaintext: clearAuthOK})
	case containsString(mechanisms, "LOGIN"):
		return client.Auth(&tlsOnlyAuth{inner: loginAuth{username: s.cfg.Username, password: password}, allowPlaintext: clearAuthOK})
	default:
		return errAuthUnsupported
	}
}

// authMechanisms lists what the server offers in its "AUTH PLAIN LOGIN"
// advertisement and in the legacy "AUTH=PLAIN LOGIN" one, which net/smtp
// reports as the extension "AUTH=PLAIN" with the rest as its parameters.
// Only the mechanisms Forge speaks are looked for in the legacy form.
func authMechanisms(client *smtp.Client) []string {
	var mechanisms []string
	if ok, params := client.Extension("AUTH"); ok {
		mechanisms = append(mechanisms, strings.Fields(strings.ToUpper(params))...)
	}
	for _, mechanism := range []string{"PLAIN", "LOGIN"} {
		if ok, params := client.Extension("AUTH=" + mechanism); ok {
			mechanisms = append(mechanisms, mechanism)
			mechanisms = append(mechanisms, strings.Fields(strings.ToUpper(params))...)
		}
	}
	return mechanisms
}

var (
	errAuthPlaintext   = errors.New("refusing to authenticate without TLS")
	errAuthUnsupported = errors.New("no supported authentication mechanism")
)

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// tlsOnlyAuth guards a mechanism so credentials are produced only on an
// encrypted connection, even if a caller of net/smtp changes the order of
// steps. net/smtp's own PlainAuth would also send them to localhost in clear.
type tlsOnlyAuth struct {
	inner          smtp.Auth
	allowPlaintext bool
}

func (a *tlsOnlyAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS && !a.allowPlaintext {
		return "", nil, errAuthPlaintext
	}
	return a.inner.Start(server)
}

func (a *tlsOnlyAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	return a.inner.Next(fromServer, more)
}

type plainAuth struct{ username, password string }

func (a plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.username + "\x00" + a.password), nil
}

func (plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected server challenge")
	}
	return nil, nil
}

type loginAuth struct{ username, password string }

func (a loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}

func (a loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	prompt := strings.ToLower(strings.TrimSpace(string(fromServer)))
	if decoded, err := base64.StdEncoding.DecodeString(prompt); err == nil {
		prompt = strings.ToLower(strings.TrimSpace(string(decoded)))
	}
	switch {
	case strings.HasPrefix(prompt, "username"):
		return []byte(a.username), nil
	case strings.HasPrefix(prompt, "password"):
		return []byte(a.password), nil
	default:
		return nil, errors.New("unexpected server challenge")
	}
}

// classify maps a transport error to a fault. It deliberately drops the
// server's reply text, which echoes recipients ("550 <bob@example.com> no
// such user") and sometimes credentials, keeping the numeric code only.
func (s *SMTPSender) classify(ctx context.Context, stage string, err error) error {
	if ctx.Err() != nil {
		return transient(stage, 0, ctx.Err())
	}
	var reply *textproto.Error
	if errors.As(err, &reply) {
		return s.fromReply(stage, reply)
	}
	switch {
	case errors.Is(err, errAuthPlaintext), errors.Is(err, errAuthUnsupported):
		return withFault(ErrAuthFailed, stage, 0, err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		// The connection deadline equals the context deadline, so the I/O
		// error can be observed a moment before ctx.Err() flips. A timeout
		// is a deadline expiry either way; report it consistently.
		return transient(stage, 0, errors.Join(context.DeadlineExceeded, err))
	}
	if stage == "starttls" || isTLSError(err) {
		return withFault(ErrTLSFailed, stage, 0, err)
	}
	return transient(stage, 0, err)
}

func (s *SMTPSender) fromReply(stage string, reply *textproto.Error) error {
	cause := fmt.Errorf("smtp %s: reply %d", stage, reply.Code)
	switch {
	case reply.Code == 530 || reply.Code == 535 || (stage == "auth" && (reply.Code == 534 || reply.Code == 538)):
		// 530 (authentication required) and 535 (credentials refused) can be
		// returned by servers at any command, not only during AUTH.
		return withFault(ErrAuthFailed, stage, reply.Code, cause)
	case reply.Code >= 400 && reply.Code < 500:
		return transient(stage, reply.Code, cause)
	default:
		return withFault(ErrRejected, stage, reply.Code, cause)
	}
}

func isTLSError(err error) bool {
	var verify *tls.CertificateVerificationError
	var header tls.RecordHeaderError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &verify) || errors.As(err, &header) || errors.As(err, &unknown) ||
		errors.As(err, &hostname) || errors.As(err, &invalid)
}

func transient(stage string, code int, cause error) error {
	return withFault(ErrUnavailable, stage, code, cause)
}

func withFault(base *fault.Error, stage string, code int, cause error) error {
	out := base.WithCause(cause)
	out.Metadata = map[string]string{"stage": stage}
	if code != 0 {
		out.Metadata["smtp_code"] = strconv.Itoa(code)
	}
	return out
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}
