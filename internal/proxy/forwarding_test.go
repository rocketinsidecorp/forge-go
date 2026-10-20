package proxy

import (
	"io"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/web"
)

// TestForwardedChainIsReplacedNotAppended: forge-proxy is the internet
// edge, so whatever chain a client sends (several header lines, X-Real-IP)
// is discarded and the application receives exactly one X-Forwarded-For
// entry, the connecting address.
func TestForwardedChainIsReplacedNotAppended(t *testing.T) {
	seen := make(chan http.Header, 1)
	app := newBackend(t, "app", func(_ http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
	})
	srv, front := newTestServer(t, "")
	if _, err := srv.Deploy(t.Context(), fastRequest(app.addr())); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/x", nil)
	req.Host = testHost
	req.Header.Add("X-Forwarded-For", "198.51.100.1, 198.51.100.2")
	req.Header.Add("X-Forwarded-For", "198.51.100.3")
	req.Header.Set("X-Real-IP", "198.51.100.4")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	header := <-seen
	if got := header.Values("X-Forwarded-For"); len(got) != 1 || got[0] != "127.0.0.1" {
		t.Fatalf("X-Forwarded-For = %q, want only the connecting address", got)
	}
	if got := header.Get("X-Real-IP"); got != "" {
		t.Fatalf("client X-Real-IP %q reached the application", got)
	}
}

// loopbackClient dials from source, another loopback address, so the
// client, the proxy (127.0.0.1) and the application can be told apart. It
// skips where the system cannot dial from that address (Linux can).
func loopbackClient(t *testing.T, source string) *http.Client {
	t.Helper()
	dialer := &net.Dialer{Timeout: 5 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(source)}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	probe, err := dialer.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Skipf("cannot dial from %s on this system: %v", source, err)
	}
	_ = probe.Close()
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, DialContext: dialer.DialContext}}
}

// TestApplicationResolvesClientBehindProxy runs the application's resolver
// (web.TrustedProxies, trusting only the proxy's address) behind
// forge-proxy: the application sees the real client even when the client
// spoofs X-Forwarded-For, and a client that bypasses the proxy cannot
// choose its address with the header either.
func TestApplicationResolvesClientBehindProxy(t *testing.T) {
	trusted, err := web.NewTrustedProxies(netip.MustParsePrefix("127.0.0.1/32"))
	if err != nil {
		t.Fatal(err)
	}
	clientIP := trusted.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, web.ClientIP(r))
	}))
	app := newBackend(t, "app", clientIP.ServeHTTP)
	srv, front := newTestServer(t, "")
	if _, err := srv.Deploy(t.Context(), fastRequest(app.addr())); err != nil {
		t.Fatal(err)
	}
	client := loopbackClient(t, "127.0.0.2")

	for _, spoof := range []string{"", "203.0.113.9", "203.0.113.9, 127.0.0.1"} {
		req, _ := http.NewRequest(http.MethodGet, front.URL+"/whoami", nil)
		req.Host = testHost
		if spoof != "" {
			req.Header.Set("X-Forwarded-For", spoof)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != "127.0.0.2" {
			t.Fatalf("through the proxy with X-Forwarded-For %q the application saw %q, want 127.0.0.2", spoof, body)
		}
	}

	// Straight to the application: the peer is not a trusted proxy, so
	// the header is ignored.
	req, _ := http.NewRequest(http.MethodGet, app.server.URL+"/whoami", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "127.0.0.2" {
		t.Fatalf("direct request with a spoofed header resolved to %q, want the peer 127.0.0.2", body)
	}
}
