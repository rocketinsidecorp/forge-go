package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestUpgradeThroughProxy: a WebSocket-style upgrade (101 and a hijacked,
// bidirectional connection) works through forge-proxy and its access-log
// response recorder.
func TestUpgradeThroughProxy(t *testing.T) {
	app := newBackend(t, "app", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" || r.Header.Get("X-Forwarded-For") == "" {
			http.Error(w, "upgrade expected", http.StatusBadRequest)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
		for {
			line, err := rw.ReadString('\n')
			if err != nil {
				return
			}
			_, _ = rw.WriteString("echo " + line)
			_ = rw.Flush()
		}
	})
	srv, front := newTestServer(t, "")
	if _, err := srv.Deploy(t.Context(), fastRequest(app.addr())); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(front.URL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, err = io.WriteString(conn, "GET /cable HTTP/1.1\r\nHost: "+testHost+"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Upgrade") != "websocket" {
		t.Fatalf("upgrade answered %d %v", resp.StatusCode, resp.Header)
	}
	for _, message := range []string{"ping\n", "second\n"} {
		if _, err := io.WriteString(conn, message); err != nil {
			t.Fatal(err)
		}
		got, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if got != "echo "+message {
			t.Fatalf("echo = %q, want %q", got, "echo "+message)
		}
	}
}
