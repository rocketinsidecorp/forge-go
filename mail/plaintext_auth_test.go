package mail_test

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
	forgemail "github.com/rocketinsidecorp/forge-go/mail"
)

// sendViaFake sends to a fake plaintext server while presenting host as the
// destination, so the non-loopback rules can be tested locally.
func sendViaFake(t *testing.T, server *fakeSMTP, host string, mutate func(*forgemail.SMTPConfig)) error {
	t.Helper()
	cfg := forgemail.SMTPConfig{
		Host:           host,
		Port:           25,
		Username:       testUser,
		Password:       config.NewSecret(testPass),
		AllowPlaintext: true,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	sender, err := forgemail.NewSMTPSender(cfg)
	if err != nil {
		t.Fatal(err)
	}
	forgemail.SetDialerForTest(sender, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(server.host(), strconv.Itoa(server.port())))
	})
	return sender.Send(context.Background(), sendMessage())
}

// A network attacker can strip the STARTTLS advertisement. With
// AllowPlaintext set, the sender must still never put the password on a
// non-loopback connection in clear.
func TestPlaintextAllowedNeverSendsCredentialsToRemoteHosts(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"mail.example.com", "10.0.0.5", "192.168.1.10", "smtp.internal", "localhost.example.com"} {
		server := newFakeSMTP(t, func(s *fakeSMTP) { s.offerSTARTTLS = false; s.authOnPlain = true })
		err := sendViaFake(t, server, host, nil)
		if !errors.Is(err, forgemail.ErrAuthFailed) {
			t.Errorf("%s: err = %v, want ErrAuthFailed", host, err)
			continue
		}
		assertNoSecrets(t, err)
		for _, session := range server.Sessions() {
			if session.AuthBeforeTLS || session.AuthSucceeded || session.Sender != "" || session.Data != "" {
				t.Errorf("%s: credentials or mail sent in clear: %+v", host, session)
			}
			for _, command := range session.Commands {
				if strings.HasPrefix(command, "AUTH") {
					t.Errorf("%s: AUTH command sent: %v", host, session.Commands)
				}
			}
		}
	}
}

func TestPlaintextWithoutCredentialsStillWorksOnTrustedRemoteNetwork(t *testing.T) {
	t.Parallel()
	server := newFakeSMTP(t, func(s *fakeSMTP) { s.offerSTARTTLS = false })
	err := sendViaFake(t, server, "relay.internal", func(c *forgemail.SMTPConfig) { c.Username, c.Password = "", config.Secret{} })
	if err != nil {
		t.Fatalf("unauthenticated relay on a trusted network: %v", err)
	}
	if session := server.Sessions()[0]; session.Data == "" || session.AuthSucceeded {
		t.Fatalf("session = %+v", session)
	}
}

func TestPlaintextCredentialsAreAllowedOnlyToLoopbackWhenExplicit(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"localhost", "127.0.0.1", "127.0.0.9"} {
		server := newFakeSMTP(t, func(s *fakeSMTP) { s.offerSTARTTLS = false; s.authOnPlain = true })
		if err := sendViaFake(t, server, host, nil); err != nil {
			t.Errorf("%s: %v", host, err)
		}
	}
	// Without the explicit flag even loopback refuses (no STARTTLS).
	server := newFakeSMTP(t, func(s *fakeSMTP) { s.offerSTARTTLS = false; s.authOnPlain = true })
	err := sendViaFake(t, server, "127.0.0.1", func(c *forgemail.SMTPConfig) { c.AllowPlaintext = false })
	if !errors.Is(err, forgemail.ErrTLSRequired) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoopbackHost(t *testing.T) {
	t.Parallel()
	for host, want := range map[string]bool{
		"localhost": true, "LOCALHOST": true, "localhost.": true, "127.0.0.1": true, "127.255.255.254": true, "::1": true, "[::1]": true,
		"0.0.0.0": false, "10.0.0.1": false, "192.168.0.1": false, "128.0.0.1": false, "::": false, "example.com": false,
		"localhost.example.com": false, "127.0.0.1.example.com": false, "": false, "::ffff:10.0.0.1": false,
	} {
		if got := config.LoopbackHost(host); got != want {
			t.Errorf("LoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}
