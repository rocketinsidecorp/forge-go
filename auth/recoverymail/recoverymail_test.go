package recoverymail_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/auth/recoverymail"
	"github.com/rocketinsidecorp/forge-go/mail"
	"github.com/rocketinsidecorp/forge-go/mail/mailtest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

var fixedNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func newSender(t *testing.T, sender mail.Sender, mutate func(*recoverymail.Config)) *recoverymail.Sender {
	t.Helper()
	cfg := recoverymail.Config{
		From:     mail.Address{Name: "Acme", Email: "no-reply@example.com"},
		AppName:  "Acme",
		ResetURL: "https://app.example.com/reset-password",
		Now:      func() time.Time { return fixedNow },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	adapter, err := recoverymail.New(sender, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func recoveryMessage(t *testing.T) auth.PasswordRecoveryMessage {
	t.Helper()
	token, err := auth.NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	return auth.PasswordRecoveryMessage{Email: "alice@example.org", TenantSlug: "acme", Token: token, ExpiresAt: fixedNow.Add(30 * time.Minute)}
}

func TestSendsRecoveryEmailWithLinkInBothParts(t *testing.T) {
	t.Parallel()
	recorder := mailtest.NewRecorder()
	adapter := newSender(t, recorder, nil)
	message := recoveryMessage(t)
	if err := adapter.SendPasswordRecovery(context.Background(), message); err != nil {
		t.Fatalf("SendPasswordRecovery: %v", err)
	}
	delivery, ok := recorder.Last()
	if !ok {
		t.Fatal("nothing sent")
	}
	msg := delivery.Message
	token := message.Token.Reveal()
	link := "https://app.example.com/reset-password?token=" + token
	if msg.Subject != "Reset your Acme password" || msg.To[0].Email != "alice@example.org" || msg.From.Email != "no-reply@example.com" {
		t.Fatalf("message = %+v", msg)
	}
	if !strings.Contains(msg.Text, link) || !strings.Contains(msg.HTML, `href="`+link+`"`) {
		t.Fatalf("link missing:\n%s\n%s", msg.Text, msg.HTML)
	}
	if !strings.Contains(msg.Text, "expires in 30 minutes") {
		t.Fatalf("expiry missing: %s", msg.Text)
	}
	if msg.Headers["Auto-Submitted"] != "auto-generated" {
		t.Fatalf("headers = %v", msg.Headers)
	}
	parsed, err := delivery.Parse()
	if err != nil || !strings.HasPrefix(parsed.Header.Get("Content-Type"), "multipart/alternative") {
		t.Fatalf("not multipart/alternative: %v %v", parsed, err)
	}
}

func TestTokenInFragment(t *testing.T) {
	t.Parallel()
	recorder := mailtest.NewRecorder()
	adapter := newSender(t, recorder, func(c *recoverymail.Config) {
		c.ResetURL = "https://app.example.com/reset?lang=pt"
		c.TokenInFragment = true
	})
	message := recoveryMessage(t)
	if err := adapter.SendPasswordRecovery(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	delivery, _ := recorder.Last()
	want := "https://app.example.com/reset?lang=pt#token=" + message.Token.Reveal()
	if !strings.Contains(delivery.Message.Text, want) {
		t.Fatalf("text = %s", delivery.Message.Text)
	}
	link := extractLink(t, delivery.Message.Text)
	parsed, _ := url.Parse(link)
	if parsed.RawQuery != "lang=pt" || parsed.Fragment != "token="+message.Token.Reveal() {
		t.Fatalf("link = %s", link)
	}
}

func extractLink(t *testing.T, text string) string {
	t.Helper()
	for _, field := range strings.Fields(text) {
		if strings.HasPrefix(field, "https://") {
			return field
		}
	}
	t.Fatal("no link in text")
	return ""
}

func TestAppNameIsEscapedInHTMLButNotInText(t *testing.T) {
	t.Parallel()
	recorder := mailtest.NewRecorder()
	adapter := newSender(t, recorder, func(c *recoverymail.Config) { c.AppName = `Acme <b>&"Co"` })
	if err := adapter.SendPasswordRecovery(context.Background(), recoveryMessage(t)); err != nil {
		t.Fatal(err)
	}
	delivery, _ := recorder.Last()
	if strings.Contains(delivery.Message.HTML, "<b>") || !strings.Contains(delivery.Message.HTML, "Acme &lt;b&gt;&amp;&#34;Co&#34;") {
		t.Fatalf("html not escaped: %s", delivery.Message.HTML)
	}
	if !strings.Contains(delivery.Message.Text, `Acme <b>&"Co"`) {
		t.Fatalf("text altered: %s", delivery.Message.Text)
	}
}

func TestExpiryMinutesRoundUpAndFloor(t *testing.T) {
	t.Parallel()
	for expires, want := range map[time.Duration]string{
		90 * time.Second:   "expires in 2 minutes",
		10 * time.Second:   "expires in 1 minutes",
		-5 * time.Minute:   "expires in 1 minutes",
		24 * time.Hour:     "expires in 1440 minutes",
		30 * time.Minute:   "expires in 30 minutes",
		61 * time.Minute:   "expires in 61 minutes",
		60*time.Second + 1: "expires in 2 minutes",
	} {
		recorder := mailtest.NewRecorder()
		adapter := newSender(t, recorder, nil)
		message := recoveryMessage(t)
		message.ExpiresAt = fixedNow.Add(expires)
		if err := adapter.SendPasswordRecovery(context.Background(), message); err != nil {
			t.Fatal(err)
		}
		delivery, _ := recorder.Last()
		if !strings.Contains(delivery.Message.Text, want) {
			t.Errorf("%v: text misses %q: %s", expires, want, delivery.Message.Text)
		}
	}
}

func TestRejectsBadRecipientAndTokenWithoutLeaking(t *testing.T) {
	t.Parallel()
	recorder := mailtest.NewRecorder()
	adapter := newSender(t, recorder, nil)
	for _, email := range []string{"", "alice@example.org\r\nBcc: evil@example.net", "not-an-address", "a@b@c"} {
		message := recoveryMessage(t)
		message.Email = email
		err := adapter.SendPasswordRecovery(context.Background(), message)
		if err == nil {
			t.Errorf("email %q accepted", email)
			continue
		}
		if strings.Contains(err.Error(), message.Token.Reveal()) || strings.Contains(err.Error(), "evil") {
			t.Errorf("error leaks: %v", err)
		}
	}
	message := recoveryMessage(t)
	message.Token = auth.RecoveryToken{}
	if err := adapter.SendPasswordRecovery(context.Background(), message); err == nil {
		t.Error("empty token accepted")
	}
	if recorder.Len() != 0 {
		t.Fatalf("%d messages sent for invalid input", recorder.Len())
	}
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()
	recorder := mailtest.NewRecorder()
	good := recoverymail.Config{From: mail.Address{Email: "a@example.com"}, AppName: "Acme", ResetURL: "https://app.example.com/reset", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	bad := map[string]func(*recoverymail.Config){
		"no app name":         func(c *recoverymail.Config) { c.AppName = " " },
		"long app name":       func(c *recoverymail.Config) { c.AppName = strings.Repeat("a", 101) },
		"bad from":            func(c *recoverymail.Config) { c.From = mail.Address{Email: "nope"} },
		"no from":             func(c *recoverymail.Config) { c.From = mail.Address{} },
		"no reset url":        func(c *recoverymail.Config) { c.ResetURL = "" },
		"relative reset url":  func(c *recoverymail.Config) { c.ResetURL = "/reset" },
		"http remote":         func(c *recoverymail.Config) { c.ResetURL = "http://app.example.com/reset" },
		"javascript url":      func(c *recoverymail.Config) { c.ResetURL = "javascript:alert(1)" },
		"credentials in url":  func(c *recoverymail.Config) { c.ResetURL = "https://user:pw@app.example.com/reset" },
		"fragment in url":     func(c *recoverymail.Config) { c.ResetURL = "https://app.example.com/reset#x" },
		"mailto url":          func(c *recoverymail.Config) { c.ResetURL = "mailto:a@example.com" },
		"backslash in host":   func(c *recoverymail.Config) { c.ResetURL = "https://app.example.com\\.evil.example/reset" },
		"backslash userinfo":  func(c *recoverymail.Config) { c.ResetURL = "https://app.example.com\\@evil.example/reset" },
		"http localhost like": func(c *recoverymail.Config) { c.ResetURL = "http://localhost.evil.com/reset" },
	}
	for name, mutate := range bad {
		cfg := good
		mutate(&cfg)
		if _, err := recoverymail.New(recorder, cfg); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := recoverymail.New(nil, good); err == nil {
		t.Error("nil sender accepted")
	}
	for _, reset := range []string{"http://localhost:3000/reset", "http://127.0.0.1/reset", "http://[::1]:8080/reset", "http://app.localhost/reset"} {
		cfg := good
		cfg.ResetURL = reset
		if _, err := recoverymail.New(recorder, cfg); err != nil {
			t.Errorf("%s rejected: %v", reset, err)
		}
	}
}

func TestCustomLinkAndTemplate(t *testing.T) {
	t.Parallel()
	recorder := mailtest.NewRecorder()
	tmpl, err := mail.NewTemplate[recoverymail.Data](fstest.MapFS{
		"custom.text.tmpl":    {Data: []byte("Go: {{.Link}} ({{.AppName}})")},
		"custom.subject.tmpl": {Data: []byte("Senha")},
	}, "custom")
	if err != nil {
		t.Fatal(err)
	}
	adapter := newSender(t, recorder, func(c *recoverymail.Config) {
		c.Template = tmpl
		c.ResetURL = ""
		c.Link = func(m auth.PasswordRecoveryMessage) (string, error) {
			return "https://" + m.TenantSlug + ".example.com/reset#t=" + m.Token.Reveal(), nil
		}
	})
	message := recoveryMessage(t)
	if err := adapter.SendPasswordRecovery(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	delivery, _ := recorder.Last()
	if delivery.Message.Subject != "Senha" || delivery.Message.Text != "Go: https://acme.example.com/reset#t="+message.Token.Reveal()+" (Acme)" || delivery.Message.HTML != "" {
		t.Fatalf("message = %+v", delivery.Message)
	}

	unsafe := newSender(t, recorder, func(c *recoverymail.Config) {
		c.ResetURL = ""
		c.Link = func(auth.PasswordRecoveryMessage) (string, error) { return "javascript:alert(1)", nil }
	})
	if err := unsafe.SendPasswordRecovery(context.Background(), recoveryMessage(t)); err == nil {
		t.Fatal("unsafe custom link accepted")
	}
	failing := newSender(t, recorder, func(c *recoverymail.Config) {
		c.ResetURL = ""
		c.Link = func(auth.PasswordRecoveryMessage) (string, error) { return "", errors.New("boom with token") }
	})
	if err := failing.SendPasswordRecovery(context.Background(), recoveryMessage(t)); err == nil || strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

// memoryStore is a RecoveryStore that records what the service persisted.
type memoryStore struct {
	mu          sync.Mutex
	identity    auth.RecoveryIdentity
	created     []auth.Digest
	invalidated []auth.Digest
}

func (s *memoryStore) LookupRecovery(context.Context, string, string) (auth.RecoveryIdentity, bool, error) {
	return s.identity, true, nil
}

func (s *memoryStore) CreateRecovery(_ context.Context, _ uuid.UUID, digest auth.Digest, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = append(s.created, digest)
	return nil
}

func (s *memoryStore) InvalidateRecovery(_ context.Context, digest auth.Digest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated = append(s.invalidated, digest)
	return nil
}

func (s *memoryStore) ConsumeRecovery(context.Context, auth.Digest, string) (bool, error) {
	return false, nil
}

func TestWorksAsRecoverySenderOfTheRecoveryService(t *testing.T) {
	t.Parallel()
	store := &memoryStore{identity: auth.RecoveryIdentity{SubjectID: uuid.MustNew(), Email: "alice@example.org"}}
	recorder := mailtest.NewRecorder()
	service, err := auth.NewRecoveryService(store, newSender(t, recorder, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Request(context.Background(), auth.RecoveryRequest{Email: "Alice@Example.org", TenantSlug: "acme", Source: "203.0.113.9"}); err != nil {
		t.Fatalf("Request: %v", err)
	}
	delivery, ok := recorder.Last()
	if !ok || delivery.Message.To[0].Email != "alice@example.org" {
		t.Fatalf("delivery = %+v", delivery)
	}
	parsed, err := url.Parse(extractLink(t, delivery.Message.Text))
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.ParseRecoveryToken(parsed.Query().Get("token"))
	if err != nil {
		t.Fatalf("link carries no valid recovery token: %v", err)
	}
	if len(store.created) != 1 || store.created[0] != token.Digest() {
		t.Fatal("the emailed token does not match the persisted digest")
	}
}

func TestDeliveryFailureInvalidatesTokenAndDoesNotLeakIt(t *testing.T) {
	t.Parallel()
	store := &memoryStore{identity: auth.RecoveryIdentity{SubjectID: uuid.MustNew(), Email: "alice@example.org"}}
	recorder := mailtest.NewRecorder()
	recorder.FailWith(mail.ErrUnavailable)
	service, err := auth.NewRecoveryService(store, newSender(t, recorder, nil))
	if err != nil {
		t.Fatal(err)
	}
	err = service.Request(context.Background(), auth.RecoveryRequest{Email: "alice@example.org", TenantSlug: "acme"})
	var delivery *auth.RecoveryDeliveryError
	if !errors.As(err, &delivery) || !errors.Is(err, mail.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if len(store.invalidated) != 1 || store.invalidated[0] != store.created[0] {
		t.Fatal("undelivered token was not invalidated")
	}
}
