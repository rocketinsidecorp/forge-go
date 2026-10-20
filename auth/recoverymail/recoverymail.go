// Package recoverymail delivers password-recovery emails: an adapter that
// implements auth.RecoverySender on top of mail.Sender.
//
// It lives apart from both packages so neither core package depends on the
// other, and it adds no third-party dependency.
//
// Wire it like this:
//
//	sender, err := mail.FromConfig(cfg)             // or a queue-backed Sender
//	recovery, err := recoverymail.New(sender, recoverymail.Config{
//	    From:     from,                              // mail.DefaultFrom(cfg)
//	    AppName:  "Acme",
//	    ResetURL: "https://app.example.com/reset-password",
//	    Logger:   logger,                            // delivery failures are logged here
//	})
//	service, err := auth.NewRecoveryService(store, recovery)
//
// Applications without a configured mail URL should not construct a
// recovery service at all (see mail.ErrNotConfigured): a generated app must
// fail closed rather than silently drop reset links.
package recoverymail

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/mail"
)

//go:embed templates
var embedded embed.FS

// Data is what the recovery template receives.
type Data struct {
	AppName string
	// Link is the complete reset URL including the token.
	Link string
	// ExpiresAt is the token expiry in UTC; ExpiresInMinutes is the time
	// left, rounded up to at least one minute.
	ExpiresAt        time.Time
	ExpiresInMinutes int
}

// Config configures the adapter.
type Config struct {
	// From is the sender of the email (required).
	From mail.Address
	// AppName is shown in the subject and body (required).
	AppName string
	// ResetURL is the page that lets the user choose a new password and posts
	// the token to the reset endpoint. It must be https, or http on a
	// loopback host for development, and carry no credentials.
	ResetURL string
	// TokenInFragment puts the token after '#' instead of in the query
	// string. Fragments are never sent to servers, logged by proxies or
	// leaked through Referer, but only client-side script can read them, so
	// choose it for single-page reset forms.
	TokenInFragment bool
	// Link overrides the default link construction (for example to add a
	// tenant or locale). It must return an absolute https URL.
	Link func(auth.PasswordRecoveryMessage) (string, error)
	// Template overrides the built-in English template. It receives Data.
	Template *mail.Template[Data]
	// Logger receives one error line per recovery email that could not be
	// sent, with the fault code and SMTP stage only: never the recipient, the
	// token, the link or the error text. It is required. auth deliberately
	// answers the public request with the same 202 whether delivery worked,
	// so without this line a broken mail setup is invisible. Pass a logger
	// that discards output only if delivery failures are observed elsewhere
	// (for example in a queue-backed Sender).
	Logger *slog.Logger
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// Sender implements auth.RecoverySender.
type Sender struct {
	sender   mail.Sender
	cfg      Config
	template *mail.Template[Data]
	logger   *slog.Logger
	now      func() time.Time
}

var _ auth.RecoverySender = (*Sender)(nil)

// New validates cfg and returns the adapter. sender may be an SMTP sender, or
// a queue-backed mail.Sender so delivery happens off the request path.
func New(sender mail.Sender, cfg Config) (*Sender, error) {
	if sender == nil {
		return nil, errors.New("recoverymail: mail sender is required")
	}
	if err := cfg.From.Validate(); err != nil {
		return nil, errors.New("recoverymail: From must be a valid sender address")
	}
	if cfg.Logger == nil {
		return nil, errors.New("recoverymail: Logger is required: delivery failures are otherwise invisible")
	}
	if strings.TrimSpace(cfg.AppName) == "" || len(cfg.AppName) > 100 || strings.IndexFunc(cfg.AppName, unicode.IsControl) >= 0 {
		return nil, errors.New("recoverymail: AppName is required (at most 100 bytes, no control characters)")
	}
	if cfg.Link == nil {
		if err := validateResetURL(cfg.ResetURL, false); err != nil {
			return nil, err
		}
	}
	tmpl := cfg.Template
	if tmpl == nil {
		sub, err := fs.Sub(embedded, "templates")
		if err != nil {
			return nil, err
		}
		if tmpl, err = mail.NewTemplate[Data](sub, "password_recovery"); err != nil {
			return nil, err
		}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	s := &Sender{sender: sender, cfg: cfg, template: tmpl, logger: cfg.Logger, now: now}
	// Render and validate a probe message now, so a configuration that can
	// never produce a deliverable email (an AppName that breaks the subject
	// header, a template that fails) stops the application at start-up
	// instead of failing every send behind a uniform 202.
	probe, err := s.compose("probe@example.com", "https://probe.example.com/reset?token=probe", now().Add(30*time.Minute))
	if err == nil {
		err = probe.Validate()
	}
	if err != nil {
		return nil, errors.New("recoverymail: configuration does not produce a valid email (check AppName, From and Template)")
	}
	return s, nil
}

func validateResetURL(raw string, allowFragment bool) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Fragment != "" && !allowFragment) || parsed.Opaque != "" {
		return errors.New("recoverymail: ResetURL must be an absolute http(s) URL without credentials or fragment")
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !isLoopback(parsed.Hostname()) {
			return errors.New("recoverymail: ResetURL must use https (http is allowed only for localhost)")
		}
	default:
		return errors.New("recoverymail: ResetURL must use https (http is allowed only for localhost)")
	}
	return nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasSuffix(host, ".localhost")
}

// SendPasswordRecovery renders and sends the reset email. The plaintext token
// appears only in the link of the message body; it is never logged and does
// not appear in returned errors. A failure is logged once (fault code and
// SMTP stage only) before it is returned, because the HTTP layer answers the
// public request identically either way.
func (s *Sender) SendPasswordRecovery(ctx context.Context, message auth.PasswordRecoveryMessage) error {
	if _, err := mail.NewAddress("", message.Email); err != nil {
		// Includes mail.ErrAddressUnsupported: auth stores only ASCII
		// logins (auth.CanonicalLogin), but an account provisioned before
		// that rule can still hold an internationalised address.
		return s.fail(ctx, "recipient", err, errors.New("recoverymail: recipient address cannot be used"))
	}
	token := message.Token.Reveal()
	if token == "" {
		return s.fail(ctx, "token", nil, errors.New("recoverymail: recovery token is required"))
	}
	link, err := s.link(message, token)
	if err != nil {
		return s.fail(ctx, "link", nil, err)
	}
	msg, err := s.compose(message.Email, link, message.ExpiresAt)
	if err != nil {
		return s.fail(ctx, "render", err, err)
	}
	if err := s.sender.Send(ctx, msg); err != nil {
		return s.fail(ctx, "send", err, err)
	}
	return nil
}

// compose renders the message for one recipient.
func (s *Sender) compose(email, link string, expiresAt time.Time) (mail.Message, error) {
	to, err := mail.NewAddress("", email)
	if err != nil {
		return mail.Message{}, err
	}
	remaining := int((s.expiresIn(expiresAt) + time.Minute - 1) / time.Minute)
	data := Data{
		AppName:          s.cfg.AppName,
		Link:             link,
		ExpiresAt:        expiresAt.UTC(),
		ExpiresInMinutes: max(remaining, 1),
	}
	base := mail.Message{
		From:    s.cfg.From,
		To:      []mail.Address{to},
		Headers: map[string]string{"Auto-Submitted": "auto-generated"},
	}
	return s.template.Message(base, data)
}

// fail logs the reason a recovery email was not sent and returns result. Only
// the fault code and the SMTP stage are logged; err's text can quote
// addresses or server replies.
func (s *Sender) fail(ctx context.Context, step string, err, result error) error {
	code, stage := "recoverymail_"+step, ""
	if f := fault.From(err); f != nil {
		code, stage = f.Code, f.Metadata["stage"]
	}
	s.logger.ErrorContext(ctx, "password recovery email was not sent", "step", step, "code", code, "stage", stage)
	return result
}

func (s *Sender) expiresIn(expiresAt time.Time) time.Duration {
	if remaining := expiresAt.Sub(s.now()); remaining > 0 {
		return remaining
	}
	return 0
}

func (s *Sender) link(message auth.PasswordRecoveryMessage, token string) (string, error) {
	if s.cfg.Link != nil {
		link, err := s.cfg.Link(message)
		if err != nil {
			return "", errors.New("recoverymail: building the reset link failed")
		}
		if err := validateResetURL(link, true); err != nil {
			return "", errors.New("recoverymail: custom Link must return an absolute https URL")
		}
		return link, nil
	}
	parsed, err := url.Parse(s.cfg.ResetURL)
	if err != nil {
		return "", errors.New("recoverymail: ResetURL is invalid")
	}
	if s.cfg.TokenInFragment {
		parsed.Fragment = "token=" + url.QueryEscape(token)
		return parsed.String(), nil
	}
	query := parsed.Query()
	query.Set("token", token)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
