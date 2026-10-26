package mail_test

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

// testCert returns a self-signed certificate valid for 127.0.0.1 and
// localhost, generated at runtime, and a pool trusting it.
func testCert(t testing.TB) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	return testCertFor(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
}

func testCertFor(t testing.TB, dnsNames []string, ips []net.IP) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "forge fake smtp"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// fakeSMTP is a minimal in-process SMTP server for tests.
type fakeSMTP struct {
	t        testing.TB
	listener net.Listener
	cert     tls.Certificate

	// Behaviour, set before the first connection.
	implicitTLS    bool
	offerSTARTTLS  bool
	authOnPlain    bool // advertise and accept AUTH before TLS
	authMechanisms string
	legacyAuth     bool // advertise "AUTH=PLAIN LOGIN" instead of "AUTH PLAIN LOGIN"
	username       string
	password       string
	replies        map[string]string // verb -> full reply line(s) replacing the default
	stall          bool              // never answer after the greeting

	mu       sync.Mutex
	sessions []*smtpSession
}

type smtpSession struct {
	TLS            bool
	Commands       []string
	AuthBeforeTLS  bool
	AuthSucceeded  bool
	Sender         string
	Recipients     []string
	Data           string
	StartTLSFailed bool
}

func newFakeSMTP(t testing.TB, configure func(*fakeSMTP)) *fakeSMTP {
	t.Helper()
	cert, _ := testCert(t)
	return newFakeSMTPWithCert(t, cert, configure)
}

func newFakeSMTPWithCert(t testing.TB, cert tls.Certificate, configure func(*fakeSMTP)) *fakeSMTP {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeSMTP{
		t: t, listener: listener, cert: cert,
		offerSTARTTLS: true, authMechanisms: "PLAIN LOGIN",
		username: "mailer", password: "s3cret-pass",
		replies: map[string]string{},
	}
	if configure != nil {
		configure(server)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go server.serve()
	return server
}

func (s *fakeSMTP) host() string { return "127.0.0.1" }

func (s *fakeSMTP) port() int { return s.listener.Addr().(*net.TCPAddr).Port }

func (s *fakeSMTP) Sessions() []smtpSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]smtpSession, len(s.sessions))
	for i, session := range s.sessions {
		out[i] = *session
	}
	return out
}

func (s *fakeSMTP) tlsConfig() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{s.cert}, MinVersion: tls.VersionTLS12}
}

func (s *fakeSMTP) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeSMTP) handle(raw net.Conn) {
	defer func() { _ = raw.Close() }()
	session := &smtpSession{}
	s.mu.Lock()
	s.sessions = append(s.sessions, session)
	s.mu.Unlock()
	record := func(f func()) {
		s.mu.Lock()
		defer s.mu.Unlock()
		f()
	}

	conn := raw
	if s.implicitTLS {
		tlsConn := tls.Server(raw, s.tlsConfig())
		if err := tlsConn.Handshake(); err != nil {
			record(func() { session.StartTLSFailed = true })
			return
		}
		conn = tlsConn
		record(func() { session.TLS = true })
	}
	reader := textproto.NewReader(bufio.NewReader(conn))
	say := func(format string, args ...any) {
		_, _ = fmt.Fprintf(conn, format+"\r\n", args...)
	}
	say("220 fake.example ESMTP ready")
	if s.stall {
		// Say nothing more, swallow what the client sends and wait for it to
		// give up and close the connection.
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	for {
		line, err := reader.ReadLine()
		if err != nil {
			return
		}
		verb := strings.ToUpper(strings.Fields(line + " x")[0])
		record(func() { session.Commands = append(session.Commands, redactCommand(line)) })
		if reply, ok := s.replies[verb]; ok {
			say("%s", reply)
			if strings.HasPrefix(reply, "221") || strings.HasPrefix(reply, "421") {
				return
			}
			continue
		}
		switch verb {
		case "EHLO", "HELO":
			say("250-fake.example greets you")
			say("250-8BITMIME")
			if s.offerSTARTTLS && !isTLS(conn) {
				say("250-STARTTLS")
			}
			if isTLS(conn) || s.authOnPlain {
				if s.legacyAuth {
					say("250-AUTH=%s", s.authMechanisms)
				} else {
					say("250-AUTH %s", s.authMechanisms)
				}
			}
			say("250 SIZE 10485760")
		case "STARTTLS":
			if !s.offerSTARTTLS {
				say("502 not implemented")
				continue
			}
			say("220 go ahead")
			tlsConn := tls.Server(conn, s.tlsConfig())
			if err := tlsConn.Handshake(); err != nil {
				record(func() { session.StartTLSFailed = true })
				return
			}
			conn = tlsConn
			reader = textproto.NewReader(bufio.NewReader(conn))
			record(func() { session.TLS = true })
		case "AUTH":
			s.auth(conn, reader, session, record, line, say)
		case "MAIL":
			record(func() { session.Sender = between(line) })
			say("250 2.1.0 ok")
		case "RCPT":
			record(func() { session.Recipients = append(session.Recipients, between(line)) })
			say("250 2.1.5 ok")
		case "DATA":
			say("354 end with <CRLF>.<CRLF>")
			data, err := io.ReadAll(reader.DotReader())
			if err != nil {
				return
			}
			record(func() { session.Data = string(data) })
			say("250 2.0.0 queued as ABC123")
		case "RSET", "NOOP":
			say("250 ok")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("500 5.5.1 unknown command")
		}
	}
}

func (s *fakeSMTP) auth(conn net.Conn, reader *textproto.Reader, session *smtpSession, record func(func()), line string, say func(string, ...any)) {
	if !isTLS(conn) {
		record(func() { session.AuthBeforeTLS = true })
		if !s.authOnPlain {
			say("538 5.7.11 encryption required")
			return
		}
	}
	fields := strings.Fields(line)
	var user, pass string
	switch strings.ToUpper(fields[1]) {
	case "PLAIN":
		var encoded string
		if len(fields) > 2 {
			encoded = fields[2]
		} else {
			say("334 ")
			encoded, _ = reader.ReadLine()
		}
		raw, _ := base64.StdEncoding.DecodeString(encoded)
		parts := strings.Split(string(raw), "\x00")
		if len(parts) == 3 {
			user, pass = parts[1], parts[2]
		}
	case "LOGIN":
		say("334 %s", base64.StdEncoding.EncodeToString([]byte("Username:")))
		u, _ := reader.ReadLine()
		say("334 %s", base64.StdEncoding.EncodeToString([]byte("Password:")))
		p, _ := reader.ReadLine()
		ub, _ := base64.StdEncoding.DecodeString(u)
		pb, _ := base64.StdEncoding.DecodeString(p)
		user, pass = string(ub), string(pb)
	default:
		say("504 unrecognized mechanism")
		return
	}
	if user == s.username && pass == s.password {
		record(func() { session.AuthSucceeded = true })
		say("235 2.7.0 authenticated")
		return
	}
	say("535 5.7.8 authentication failed for %s with password %s", user, pass) // hostile: echoes secrets
}

func isTLS(conn net.Conn) bool {
	_, ok := conn.(*tls.Conn)
	return ok
}

func between(line string) string {
	start, end := strings.Index(line, "<"), strings.LastIndex(line, ">")
	if start < 0 || end < start {
		return ""
	}
	return line[start+1 : end]
}

// redactCommand keeps AUTH payloads out of the transcript.
func redactCommand(line string) string {
	if strings.HasPrefix(strings.ToUpper(line), "AUTH ") {
		return strings.Join(strings.Fields(line)[:2], " ") + " ..."
	}
	return line
}
