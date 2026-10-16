package recoverymail_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/auth/recoverymail"
	"github.com/rocketinsidecorp/forge-go/mail"
	"github.com/rocketinsidecorp/forge-go/mail/mailtest"
)

func configWith(logger *slog.Logger, mutate func(*recoverymail.Config)) recoverymail.Config {
	cfg := recoverymail.Config{
		From:     mail.Address{Email: "no-reply@example.com"},
		AppName:  "Acme",
		ResetURL: "https://app.example.com/reset-password",
		Logger:   logger,
		Now:      func() time.Time { return fixedNow },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

func TestLoggerIsRequired(t *testing.T) {
	t.Parallel()
	if _, err := recoverymail.New(mailtest.NewRecorder(), configWith(nil, nil)); err == nil {
		t.Fatal("a nil Logger was accepted: delivery failures would be invisible")
	}
}

// A misconfigured AppName must stop the application at start-up. Otherwise
// every send fails behind the uniform 202 the HTTP layer returns.
func TestMisconfigurationFailsAtStartup(t *testing.T) {
	t.Parallel()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, appName := range map[string]string{
		"newline":          "Acme\nBcc: evil@example.net",
		"crlf":             "Acme\r\nX: y",
		"nul":              "Acme\x00",
		"tab":              "Acme\tCo",
		"bidi override":    "Acme\u202eCo",
		"line separator":   "Acme\u2028Co",
		"too long":         strings.Repeat("a", 101),
		"blank":            "   ",
		"invalid utf8":     "Acme\xff",
		"c1 control (NEL)": "Acme\u0085Co",
	} {
		if _, err := recoverymail.New(mailtest.NewRecorder(), configWith(discard, func(c *recoverymail.Config) { c.AppName = appName })); err == nil {
			t.Errorf("%s: AppName %q accepted", name, appName)
		}
	}
	// The probe also catches a template that renders an undeliverable email.
	broken, err := mail.NewTemplate[recoverymail.Data](fstest.MapFS{
		"x.subject.tmpl": {Data: []byte("Hello\nBcc: evil@example.net")},
		"x.text.tmpl":    {Data: []byte("body")},
	}, "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recoverymail.New(mailtest.NewRecorder(), configWith(discard, func(c *recoverymail.Config) { c.Template = broken })); err == nil {
		t.Error("a template producing an invalid subject was accepted")
	}
}

func newLogged(t *testing.T, sender mail.Sender, mutate func(*recoverymail.Config)) (*recoverymail.Sender, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	adapter, err := recoverymail.New(sender, configWith(slog.New(slog.NewTextHandler(&logs, nil)), mutate))
	if err != nil {
		t.Fatal(err)
	}
	return adapter, &logs
}

func TestDeliveryFailureIsLoggedWithCodeAndStageOnly(t *testing.T) {
	t.Parallel()
	recorder := mailtest.NewRecorder()
	rejected := mail.ErrRejected.WithCause(nil)
	rejected.Metadata = map[string]string{"stage": "rcpt", "smtp_code": "550"}
	recorder.FailWith(rejected)
	adapter, logs := newLogged(t, recorder, nil)

	message := recoveryMessage(t)
	if err := adapter.SendPasswordRecovery(context.Background(), message); err == nil {
		t.Fatal("failure was swallowed")
	}
	out := logs.String()
	for _, want := range []string{"level=ERROR", "password recovery email was not sent", "code=mail_rejected", "stage=rcpt"} {
		if !strings.Contains(out, want) {
			t.Errorf("log misses %q: %s", want, out)
		}
	}
	for _, secret := range []string{message.Token.Reveal(), "alice@example.org", "alice", "https://", "reset-password", "token=", "example.org"} {
		if strings.Contains(out, secret) {
			t.Errorf("log leaks %q: %s", secret, out)
		}
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("want exactly one log line, got %q", out)
	}
}

func TestUnsupportedRecipientIsLoggedDistinctly(t *testing.T) {
	t.Parallel()
	recorder := mailtest.NewRecorder()
	adapter, logs := newLogged(t, recorder, nil)
	message := recoveryMessage(t)
	message.Email = "jo\u00e3o@example.org"
	if err := adapter.SendPasswordRecovery(context.Background(), message); err == nil {
		t.Fatal("an address Forge cannot mail was accepted")
	}
	if !strings.Contains(logs.String(), "code=mail_address_unsupported") {
		t.Fatalf("log = %q", logs.String())
	}
	if strings.Contains(logs.String(), "jo") && strings.Contains(logs.String(), "example.org") {
		t.Fatalf("log leaks the address: %q", logs.String())
	}
	if recorder.Len() != 0 {
		t.Fatal("a message was sent")
	}
}

func TestOtherFailuresAreLoggedAndSuccessIsQuiet(t *testing.T) {
	t.Parallel()
	recorder := mailtest.NewRecorder()
	adapter, logs := newLogged(t, recorder, nil)

	if err := adapter.SendPasswordRecovery(context.Background(), recoveryMessage(t)); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Fatalf("success logged: %q", logs.String())
	}

	bad := recoveryMessage(t)
	bad.Email = "not-an-address"
	_ = adapter.SendPasswordRecovery(context.Background(), bad)
	empty := recoveryMessage(t)
	empty.Token = auth.RecoveryToken{}
	_ = adapter.SendPasswordRecovery(context.Background(), empty)
	if got := strings.Count(logs.String(), "level=ERROR"); got != 2 {
		t.Fatalf("logged %d failures, want 2: %s", got, logs.String())
	}
	if strings.Contains(logs.String(), "not-an-address") {
		t.Fatal("log leaks the rejected address")
	}
}
