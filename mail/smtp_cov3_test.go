package mail_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/fault"
	forgemail "github.com/rocketinsidecorp/forge-go/mail"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func sentCommand(commands []string, verb string) bool {
	for _, command := range commands {
		if strings.HasPrefix(strings.ToUpper(command), verb) {
			return true
		}
	}
	return false
}

// A server that misbehaves during authentication fails the send before any
// mail is offered, and nothing of the exchange reaches the error.
func TestSMTPAuthenticationExchangesThatGoWrongFailBeforeMail(t *testing.T) {
	t.Parallel()
	cert, pool := testCert(t)
	cases := map[string]func(*fakeSMTP){
		"no mechanism advertised": func(s *fakeSMTP) { s.authMechanisms = "" },
		"PLAIN answered with a challenge": func(s *fakeSMTP) {
			s.authMechanisms = "PLAIN"
			s.replies["AUTH"] = "334 " + b64("what is your password")
		},
		"LOGIN with a prompt it does not know": func(s *fakeSMTP) {
			s.authMechanisms = "LOGIN"
			s.replies["AUTH"] = "334 " + b64("Frobnicate:")
		},
		// Some servers encode the prompt twice; the second layer is read too,
		// then the (unscripted) reply to the username ends the exchange.
		"LOGIN with a prompt encoded twice": func(s *fakeSMTP) {
			s.authMechanisms = "LOGIN"
			s.replies["AUTH"] = "334 " + b64(b64("Username:"))
		},
	}
	for name, configure := range cases {
		server := newFakeSMTPWithCert(t, cert, configure)
		err := newSender(t, server, pool, nil).Send(context.Background(), sendMessage())
		if err == nil {
			t.Errorf("%s: the send succeeded", name)
			continue
		}
		assertNoSecrets(t, err)
		if fault.From(err) == nil {
			t.Errorf("%s: %v is not a fault", name, err)
		}
		if sentCommand(server.Sessions()[0].Commands, "MAIL") {
			t.Errorf("%s: mail was offered after a failed authentication", name)
		}
	}
}

// rawSMTP serves one connection with a script: for each client line it
// reads, the next scripted reply is written ("" closes the connection).
func rawSMTP(t *testing.T, greeting string, replies ...string) forgemail.SMTPConfig {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if greeting == "" {
			return
		}
		_, _ = conn.Write([]byte(greeting + "\r\n"))
		reader := bufio.NewReader(conn)
		inData := false
		for _, reply := range replies {
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if inData {
					if line == ".\r\n" {
						inData = false
						break
					}
					continue
				}
				break
			}
			if reply == "" {
				return
			}
			_, _ = conn.Write([]byte(reply + "\r\n"))
			if strings.HasPrefix(reply, "354") {
				inData = true
			}
		}
	}()
	return forgemail.SMTPConfig{
		Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port,
		AllowPlaintext: true, Timeout: 5 * time.Second,
		Password: config.NewSecret(""),
	}
}

func TestSMTPReportsAServerThatHangsUpOrRefusesAtTheGreeting(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]forgemail.SMTPConfig{
		"closes at once":         rawSMTP(t, ""),
		"refuses the connection": rawSMTP(t, "554 5.3.2 no service for you"),
	} {
		sender, err := forgemail.NewSMTPSender(cfg)
		if err != nil {
			t.Fatal(err)
		}
		err = sender.Send(context.Background(), sendMessage())
		if err == nil || fault.From(err) == nil {
			t.Errorf("%s: err = %v, want a fault", name, err)
			continue
		}
		assertNoSecrets(t, err)
	}
}

func TestSMTPReportsAServerThatDiesWhileTheMessageIsSent(t *testing.T) {
	t.Parallel()
	// The server accepts everything up to DATA, takes the message and hangs
	// up without ever saying it was queued: the message may or may not have
	// been delivered, which is a retryable failure, never a success.
	cfg := rawSMTP(t, "220 raw ready",
		"250 raw greets you", // EHLO
		"250 ok",             // MAIL
		"250 ok",             // RCPT (alice)
		"250 ok",             // RCPT (hidden)
		"354 go ahead",       // DATA
		"",                   // the end of the message: no reply
	)
	sender, err := forgemail.NewSMTPSender(cfg)
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Send(context.Background(), sendMessage())
	if !errors.Is(err, forgemail.ErrUnavailable) || !fault.From(err).Retryable {
		t.Fatalf("Send = %v, want a retryable ErrUnavailable", err)
	}
}
