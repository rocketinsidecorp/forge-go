package mail_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/fault"
	forgemail "github.com/rocketinsidecorp/forge-go/mail"
)

const (
	testUser = "mailer"
	testPass = "s3cret-pass"
)

func newSender(t *testing.T, server *fakeSMTP, pool *x509.CertPool, mutate func(*forgemail.SMTPConfig)) *forgemail.SMTPSender {
	t.Helper()
	cfg := forgemail.SMTPConfig{
		Host:     server.host(),
		Port:     server.port(),
		Username: testUser,
		Password: config.NewSecret(testPass),
		RootCAs:  pool,
		Timeout:  5 * time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	sender, err := forgemail.NewSMTPSender(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return sender
}

func sendMessage() forgemail.Message {
	msg := baseMessage()
	msg.Bcc = []forgemail.Address{{Email: "hidden@example.net"}}
	msg.Subject = "Olá"
	return msg
}

func assertNoSecrets(t *testing.T, err error) {
	t.Helper()
	text := fmt.Sprintf("%v | %+v | %#v | %s", err, err, err, fault.From(err).Message)
	for _, secret := range []string{testPass, testUser, "alice@example.org", "hidden@example.net", "app@example.com"} {
		if strings.Contains(text, secret) {
			t.Fatalf("error leaks %q: %s", secret, text)
		}
	}
}

func TestSMTPSendsOverSTARTTLSAndAuthenticatesAfterTLS(t *testing.T) {
	t.Parallel()
	cert, pool := testCert(t)
	// The server advertises AUTH even before TLS, as a hostile or sloppy
	// server would; the client must still upgrade first.
	server := newFakeSMTPWithCert(t, cert, func(s *fakeSMTP) { s.authOnPlain = true })
	sender := newSender(t, server, pool, func(c *forgemail.SMTPConfig) { c.HelloName = "app.example.com" })

	if err := sender.Send(context.Background(), sendMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	sessions := server.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d", len(sessions))
	}
	session := sessions[0]
	if !session.TLS || session.AuthBeforeTLS || !session.AuthSucceeded {
		t.Fatalf("session = %+v: credentials must be sent only after STARTTLS", session)
	}
	startTLS, auth := indexOf(session.Commands, "STARTTLS"), indexOf(session.Commands, "AUTH PLAIN ...")
	if startTLS < 0 || auth < startTLS {
		t.Fatalf("command order = %v", session.Commands)
	}
	if indexOf(session.Commands, "EHLO app.example.com") < 0 {
		t.Fatalf("hello name not used: %v", session.Commands)
	}
	if session.Sender != "app@example.com" {
		t.Fatalf("sender = %q", session.Sender)
	}
	if strings.Join(session.Recipients, ",") != "alice@example.org,hidden@example.net" {
		t.Fatalf("recipients = %v", session.Recipients)
	}
	parsed, err := mail.ReadMessage(strings.NewReader(session.Data))
	if err != nil {
		t.Fatalf("server received unparseable data: %v", err)
	}
	if parsed.Header.Get("Bcc") != "" || strings.Contains(session.Data, "hidden@example.net") {
		t.Fatal("Bcc leaked into the transmitted message")
	}
	if !strings.HasPrefix(parsed.Header.Get("Subject"), "=?utf-8?q?Ol=C3=A1") {
		t.Fatalf("subject = %q", parsed.Header.Get("Subject"))
	}
	if indexOf(session.Commands, "QUIT") < 0 {
		t.Fatalf("connection not closed politely: %v", session.Commands)
	}
}

func indexOf(list []string, want string) int {
	for i, item := range list {
		if item == want {
			return i
		}
	}
	return -1
}

func TestSMTPImplicitTLS(t *testing.T) {
	t.Parallel()
	cert, pool := testCert(t)
	server := newFakeSMTPWithCert(t, cert, func(s *fakeSMTP) { s.implicitTLS = true })
	sender := newSender(t, server, pool, func(c *forgemail.SMTPConfig) { c.ImplicitTLS = true })
	if err := sender.Send(context.Background(), sendMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	session := server.Sessions()[0]
	if !session.TLS || !session.AuthSucceeded || indexOf(session.Commands, "STARTTLS") >= 0 {
		t.Fatalf("session = %+v", session)
	}
}

func TestSMTPImplicitTLSAgainstPlaintextServerFails(t *testing.T) {
	t.Parallel()
	server := newFakeSMTP(t, nil) // speaks plaintext on connect
	_, pool := testCert(t)
	sender := newSender(t, server, pool, func(c *forgemail.SMTPConfig) { c.ImplicitTLS = true; c.Timeout = 2 * time.Second })
	err := sender.Send(context.Background(), sendMessage())
	if !errors.Is(err, forgemail.ErrTLSFailed) {
		t.Fatalf("err = %v, want ErrTLSFailed", err)
	}
	assertNoSecrets(t, err)
}

func TestSMTPAuthLoginFallback(t *testing.T) {
	t.Parallel()
	cert, pool := testCert(t)
	server := newFakeSMTPWithCert(t, cert, func(s *fakeSMTP) { s.authMechanisms = "LOGIN" })
	if err := newSender(t, server, pool, nil).Send(context.Background(), sendMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !server.Sessions()[0].AuthSucceeded {
		t.Fatal("LOGIN not used")
	}
}

func TestSMTPLegacyAuthEqualsAdvertisement(t *testing.T) {
	t.Parallel()
	// Another mechanism first, as `250-AUTH=CRAM-MD5 PLAIN LOGIN` servers
	// announce; the rest of the line is still read.
	for _, mechanisms := range []string{"PLAIN LOGIN", "LOGIN", "CRAM-MD5 PLAIN LOGIN", "XOAUTH2 LOGIN", "SCRAM-SHA-256 PLAIN"} {
		cert, pool := testCert(t)
		server := newFakeSMTPWithCert(t, cert, func(s *fakeSMTP) { s.authMechanisms = mechanisms; s.legacyAuth = true })
		if err := newSender(t, server, pool, nil).Send(context.Background(), sendMessage()); err != nil {
			t.Fatalf("%q: Send: %v", mechanisms, err)
		}
		if !server.Sessions()[0].AuthSucceeded {
			t.Fatalf("%q: authentication did not run", mechanisms)
		}
	}
}

func TestSMTPUnsupportedLegacyAuthFailsClosed(t *testing.T) {
	t.Parallel()
	cert, pool := testCert(t)
	server := newFakeSMTPWithCert(t, cert, func(s *fakeSMTP) { s.authMechanisms = "CRAM-MD5"; s.legacyAuth = true })
	err := newSender(t, server, pool, nil).Send(context.Background(), sendMessage())
	if !errors.Is(err, forgemail.ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
	for _, command := range server.Sessions()[0].Commands {
		if strings.HasPrefix(command, "MAIL") {
			t.Fatalf("sent %q after refusing every mechanism", command)
		}
	}
}

func TestSMTPRefusesWhenServerLacksSTARTTLS(t *testing.T) {
	t.Parallel()
	server := newFakeSMTP(t, func(s *fakeSMTP) { s.offerSTARTTLS = false; s.authOnPlain = true })
	sender := newSender(t, server, nil, nil)
	err := sender.Send(context.Background(), sendMessage())
	if !errors.Is(err, forgemail.ErrTLSRequired) {
		t.Fatalf("err = %v, want ErrTLSRequired", err)
	}
	assertNoSecrets(t, err)
	for _, command := range server.Sessions()[0].Commands {
		for _, forbidden := range []string{"AUTH", "MAIL", "RCPT", "DATA"} {
			if strings.HasPrefix(command, forbidden) {
				t.Fatalf("sent %q to a server without TLS: %v", command, server.Sessions()[0].Commands)
			}
		}
	}
}

func TestSMTPPlaintextOnlyWhenExplicitlyAllowed(t *testing.T) {
	t.Parallel()
	server := newFakeSMTP(t, func(s *fakeSMTP) { s.offerSTARTTLS = false; s.authOnPlain = true })
	sender := newSender(t, server, nil, func(c *forgemail.SMTPConfig) { c.AllowPlaintext = true })
	if err := sender.Send(context.Background(), sendMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	session := server.Sessions()[0]
	if session.TLS || !session.AuthSucceeded || session.Data == "" {
		t.Fatalf("session = %+v", session)
	}
}

func TestSMTPAllowPlaintextNeverDowngradesAfterTLSFailure(t *testing.T) {
	t.Parallel()
	server := newFakeSMTP(t, func(s *fakeSMTP) { s.authOnPlain = true })
	// Untrusted certificate with AllowPlaintext: the STARTTLS attempt fails
	// and the client must not retry in clear.
	sender := newSender(t, server, nil, func(c *forgemail.SMTPConfig) { c.AllowPlaintext = true })
	err := sender.Send(context.Background(), sendMessage())
	if !errors.Is(err, forgemail.ErrTLSFailed) {
		t.Fatalf("err = %v, want ErrTLSFailed", err)
	}
	for _, session := range server.Sessions() {
		if session.AuthBeforeTLS || session.AuthSucceeded || session.Data != "" || session.Sender != "" {
			t.Fatalf("downgraded: %+v", session)
		}
	}
}

func TestSMTPCertificateVerification(t *testing.T) {
	t.Parallel()
	cert, pool := testCert(t)
	_, otherPool := testCert(t)
	wrongHostCert, wrongHostPool := testCertFor(t, []string{"other.example"}, nil)

	cases := map[string]struct {
		cert tls.Certificate
		pool *x509.CertPool
	}{
		"unknown authority": {cert, nil},
		"untrusted root":    {cert, otherPool},
		"host mismatch":     {wrongHostCert, wrongHostPool},
	}
	for name, tc := range cases {
		server := newFakeSMTPWithCert(t, tc.cert, nil)
		sender := newSender(t, server, tc.pool, nil)
		err := sender.Send(context.Background(), sendMessage())
		if !errors.Is(err, forgemail.ErrTLSFailed) {
			t.Errorf("%s: err = %v, want ErrTLSFailed", name, err)
			continue
		}
		assertNoSecrets(t, err)
		for _, session := range server.Sessions() {
			if session.AuthSucceeded || session.Sender != "" || session.Data != "" {
				t.Errorf("%s: proceeded after certificate failure: %+v", name, session)
			}
		}
	}
	// The same server with the right pool works, proving the failures above
	// come from verification and not from the fixture.
	server := newFakeSMTPWithCert(t, cert, nil)
	if err := newSender(t, server, pool, nil).Send(context.Background(), sendMessage()); err != nil {
		t.Fatalf("trusted certificate rejected: %v", err)
	}
}

func TestSMTPServerErrorsMapToFaultsWithoutLeaks(t *testing.T) {
	t.Parallel()
	cert, pool := testCert(t)
	cases := []struct {
		name     string
		replies  map[string]string
		mutate   func(*fakeSMTP)
		want     *fault.Error
		code     string
		stage    string
		wantData bool
	}{
		{name: "wrong password", mutate: func(s *fakeSMTP) { s.password = "different" }, want: forgemail.ErrAuthFailed, code: "535", stage: "auth"},
		{name: "recipient refused", replies: map[string]string{"RCPT": "550 5.1.1 <alice@example.org> user unknown; also hidden@example.net"}, want: forgemail.ErrRejected, code: "550", stage: "rcpt"},
		{name: "recipient greylisted", replies: map[string]string{"RCPT": "451 4.7.1 try later alice@example.org"}, want: forgemail.ErrUnavailable, code: "451", stage: "rcpt"},
		{name: "sender refused", replies: map[string]string{"MAIL": "553 5.1.8 app@example.com not owned by user mailer"}, want: forgemail.ErrRejected, code: "553", stage: "mail"},
		{name: "data refused", replies: map[string]string{"DATA": "554 5.3.0 no"}, want: forgemail.ErrRejected, code: "554", stage: "data"},
	}
	for _, tc := range cases {
		server := newFakeSMTPWithCert(t, cert, func(s *fakeSMTP) {
			for verb, reply := range tc.replies {
				s.replies[verb] = reply
			}
			if tc.mutate != nil {
				tc.mutate(s)
			}
		})
		err := newSender(t, server, pool, nil).Send(context.Background(), sendMessage())
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %s", tc.name, err, tc.want.Code)
			continue
		}
		assertNoSecrets(t, err)
		f := fault.From(err)
		if f.Metadata["smtp_code"] != tc.code || f.Metadata["stage"] != tc.stage {
			t.Errorf("%s: metadata = %v", tc.name, f.Metadata)
		}
		if f.Retryable != (tc.want == forgemail.ErrUnavailable) {
			t.Errorf("%s: retryable = %v", tc.name, f.Retryable)
		}
		session := server.Sessions()[0]
		if tc.stage == "rcpt" && indexOf(session.Commands, "DATA") >= 0 {
			t.Errorf("%s: DATA sent after a refused recipient; delivery must be all-or-nothing", tc.name)
		}
	}
}

func TestSMTPTransientGreetingAndMailFailures(t *testing.T) {
	t.Parallel()
	cert, pool := testCert(t)
	server := newFakeSMTPWithCert(t, cert, func(s *fakeSMTP) { s.replies["MAIL"] = "421 4.3.2 shutting down" })
	err := newSender(t, server, pool, nil).Send(context.Background(), sendMessage())
	if !errors.Is(err, forgemail.ErrUnavailable) || !fault.From(err).Retryable {
		t.Fatalf("err = %v", err)
	}
}

func TestSMTPConnectionRefusedIsUnavailable(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	sender, err := forgemail.NewSMTPSender(forgemail.SMTPConfig{Host: "127.0.0.1", Port: port, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Send(context.Background(), sendMessage())
	if !errors.Is(err, forgemail.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	assertNoSecrets(t, err)
}

func TestSMTPContextCancellationAndTimeouts(t *testing.T) {
	t.Parallel()
	_, pool := testCert(t)
	server := newFakeSMTP(t, func(s *fakeSMTP) { s.stall = true })

	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(150*time.Millisecond, cancel)
		started := time.Now()
		err := newSender(t, server, pool, func(c *forgemail.SMTPConfig) { c.Timeout = time.Minute }).Send(ctx, sendMessage())
		if !errors.Is(err, forgemail.ErrUnavailable) || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want unavailable wrapping context.Canceled", err)
		}
		if elapsed := time.Since(started); elapsed > 3*time.Second {
			t.Fatalf("cancellation took %v", elapsed)
		}
	})
	t.Run("context deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		err := newSender(t, server, pool, func(c *forgemail.SMTPConfig) { c.Timeout = time.Minute }).Send(ctx, sendMessage())
		if !errors.Is(err, forgemail.ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("configured timeout", func(t *testing.T) {
		started := time.Now()
		err := newSender(t, server, pool, func(c *forgemail.SMTPConfig) { c.Timeout = 200 * time.Millisecond }).Send(context.Background(), sendMessage())
		if !errors.Is(err, forgemail.ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
		if elapsed := time.Since(started); elapsed > 3*time.Second {
			t.Fatalf("timeout took %v", elapsed)
		}
	})
	t.Run("already cancelled", func(t *testing.T) {
		quiet := newFakeSMTP(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := newSender(t, quiet, pool, nil).Send(ctx, sendMessage())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if len(quiet.Sessions()) != 0 {
			t.Fatal("connected with a cancelled context")
		}
	})
}

func TestSMTPInvalidMessageNeverConnects(t *testing.T) {
	t.Parallel()
	server := newFakeSMTP(t, nil)
	msg := sendMessage()
	msg.Subject = "hi\r\nBcc: evil@example.net"
	err := newSender(t, server, nil, nil).Send(context.Background(), msg)
	if !errors.Is(err, forgemail.ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	time.Sleep(50 * time.Millisecond) // real-clock: negative check: no connection may be made, and one would take real time to appear
	if len(server.Sessions()) != 0 {
		t.Fatal("connected before validating the message")
	}
}

func TestSMTPConcurrentSends(t *testing.T) {
	t.Parallel()
	cert, pool := testCert(t)
	server := newFakeSMTPWithCert(t, cert, nil)
	sender := newSender(t, server, pool, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- sender.Send(context.Background(), sendMessage())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := len(server.Sessions()); got != 8 {
		t.Fatalf("sessions = %d, want one connection per send", got)
	}
}

func TestSMTPLargeAttachmentRoundTrip(t *testing.T) {
	t.Parallel()
	cert, pool := testCert(t)
	server := newFakeSMTPWithCert(t, cert, nil)
	msg := sendMessage()
	msg.Attachments = []forgemail.Attachment{{Filename: "big.bin", Data: bytes.Repeat([]byte{7, 8, 9, 0xff}, 200_000)}}
	if err := newSender(t, server, pool, nil).Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := len(server.Sessions()[0].Data); got < 1_000_000 {
		t.Fatalf("data = %d bytes", got)
	}
}

func TestNewSMTPSenderValidation(t *testing.T) {
	t.Parallel()
	bad := map[string]forgemail.SMTPConfig{
		"no host":            {},
		"host with slash":    {Host: "smtp.example.com/x"},
		"host with at":       {Host: "user@smtp.example.com"},
		"host with space":    {Host: "smtp .example.com"},
		"port too high":      {Host: "h", Port: 70000},
		"negative port":      {Host: "h", Port: -1},
		"password no user":   {Host: "h", Password: config.NewSecret("x")},
		"username with crlf": {Host: "h", Username: "u\r\nQUIT", Password: config.NewSecret("x")},
		"username with tab":  {Host: "h", Username: "u\tx", Password: config.NewSecret("x")},
		"password with crlf": {Host: "h", Username: "u", Password: config.NewSecret("x\r\nQUIT")},
		"bad hello":          {Host: "h", HelloName: "bad host!"},
		"negative timeout":   {Host: "h", Timeout: -time.Second},
		"huge timeout":       {Host: "h", Timeout: time.Hour},
	}
	for name, cfg := range bad {
		if _, err := forgemail.NewSMTPSender(cfg); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, cfg := range []forgemail.SMTPConfig{{Host: "smtp.example.com"}, {Host: "::1", ImplicitTLS: true}, {Host: "127.0.0.1", HelloName: "[127.0.0.1]"}} {
		if _, err := forgemail.NewSMTPSender(cfg); err != nil {
			t.Errorf("%+v rejected: %v", cfg, err)
		}
	}
	// The password must not surface through formatting of the config.
	cfg := forgemail.SMTPConfig{Host: "h", Username: "u", Password: config.NewSecret("hunter2")}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg), "hunter2") {
		t.Fatal("SMTPConfig formatting leaks the password")
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// The connection deadline and the context deadline are the same instant, so
// the I/O timeout can surface before ctx.Err() does. Both orders must report
// context.DeadlineExceeded.
func TestSMTPIOTimeoutBeforeContextExpiryReportsDeadlineExceeded(t *testing.T) {
	t.Parallel()
	sender, err := forgemail.NewSMTPSender(forgemail.SMTPConfig{Host: "mail.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	err = forgemail.ClassifyForTest(sender, context.Background(), "data", timeoutError{})
	if !errors.Is(err, forgemail.ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}
