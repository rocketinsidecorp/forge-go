package web

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func mustTrust(t *testing.T, prefixes ...string) TrustedProxies {
	t.Helper()
	parsed := make([]netip.Prefix, 0, len(prefixes))
	for _, p := range prefixes {
		parsed = append(parsed, netip.MustParsePrefix(p))
	}
	trusted, err := NewTrustedProxies(parsed...)
	if err != nil {
		t.Fatal(err)
	}
	return trusted
}

func requestFrom(remote string, xff ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	for _, value := range xff {
		r.Header.Add("X-Forwarded-For", value)
	}
	return r
}

func TestTrustedProxiesResolve(t *testing.T) {
	// The proxy network of a forge deployment, an IPv6 proxy network and a
	// second, inner load balancer.
	trusted := mustTrust(t, "172.18.0.0/16", "fd00:1::/64", "10.0.0.7/32")
	cases := []struct {
		name   string
		remote string
		xff    []string
		want   string
	}{
		// Negative: the peer is not a trusted proxy, every header is ignored.
		{"untrusted peer spoofing", "198.51.100.7:4000", []string{"203.0.113.9"}, "198.51.100.7"},
		{"untrusted peer spoofing a trusted address", "198.51.100.7:4000", []string{"172.18.0.2"}, "198.51.100.7"},
		{"untrusted peer, no header", "198.51.100.7:4000", nil, "198.51.100.7"},
		// The trusted proxy's entry is the client.
		{"trusted peer", "172.18.0.2:5000", []string{"203.0.113.9"}, "203.0.113.9"},
		{"trusted peer, spaces", "172.18.0.2:5000", []string{"  203.0.113.9  "}, "203.0.113.9"},
		// An appending proxy: the client's own entries are left of the one
		// the proxy wrote and are never reached.
		{"client-supplied prefix ignored", "172.18.0.2:5000", []string{"1.2.3.4, 203.0.113.9"}, "203.0.113.9"},
		{"client-supplied trusted-looking prefix", "172.18.0.2:5000", []string{"172.18.0.9, 203.0.113.9"}, "203.0.113.9"},
		// Several trusted hops are skipped.
		{"trusted chain", "172.18.0.2:5000", []string{"203.0.113.9, 10.0.0.7"}, "203.0.113.9"},
		{"trusted chain over header lines", "172.18.0.2:5000", []string{"1.2.3.4, 203.0.113.9", "10.0.0.7"}, "203.0.113.9"},
		{"every hop trusted", "172.18.0.2:5000", []string{"10.0.0.7, 172.18.0.3"}, "10.0.0.7"},
		{"trusted peer, no header", "172.18.0.2:5000", nil, "172.18.0.2"},
		// Malformed entries on the walk fall back to the peer, never to a
		// value left of them.
		{"empty header", "172.18.0.2:5000", []string{""}, "172.18.0.2"},
		{"garbage", "172.18.0.2:5000", []string{"not-an-ip"}, "172.18.0.2"},
		{"trailing comma", "172.18.0.2:5000", []string{"203.0.113.9,"}, "172.18.0.2"},
		{"empty element between trusted hops", "172.18.0.2:5000", []string{"203.0.113.9,,10.0.0.7"}, "172.18.0.2"},
		{"port in entry", "172.18.0.2:5000", []string{"203.0.113.9:1234"}, "172.18.0.2"},
		{"bracketed v6", "172.18.0.2:5000", []string{"[2001:db8::1]"}, "172.18.0.2"},
		{"garbage after the client is never read", "172.18.0.2:5000", []string{"garbage, 203.0.113.9"}, "203.0.113.9"},
		{"Forwarded-style value", "172.18.0.2:5000", []string{"for=203.0.113.9"}, "172.18.0.2"},
		// IPv6.
		{"v6 trusted peer, v6 client", "[fd00:1::2]:5000", []string{"2001:db8:1:2::abcd"}, "2001:db8:1:2::abcd"},
		{"v6 untrusted peer", "[2001:db8::5]:5000", []string{"203.0.113.9"}, "2001:db8::5"},
		{"v6 zone stripped", "[fd00:1::2]:5000", []string{"fe80::1%eth0"}, "fe80::1"},
		{"v4-mapped peer is its v4 form", "[::ffff:172.18.0.2]:5000", []string{"203.0.113.9"}, "203.0.113.9"},
		{"v4-mapped client is its v4 form", "172.18.0.2:5000", []string{"::ffff:203.0.113.9"}, "203.0.113.9"},
		{"v4-mapped untrusted peer", "[::ffff:198.51.100.7]:5000", nil, "198.51.100.7"},
		// Not an IP peer (Unix socket, tests).
		{"non-IP peer", "pipe", []string{"203.0.113.9"}, "pipe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := trusted.Resolve(requestFrom(tc.remote, tc.xff...)); got != tc.want {
				t.Fatalf("Resolve(%s, %q) = %q, want %q", tc.remote, tc.xff, got, tc.want)
			}
		})
	}
}

func TestEmptyTrustedProxiesUseThePeer(t *testing.T) {
	for _, trusted := range []TrustedProxies{{}, mustTrust(t)} {
		r := requestFrom("172.18.0.2:5000", "203.0.113.9")
		r.Header.Set("X-Real-IP", "203.0.113.10")
		r.Header.Set("Forwarded", "for=203.0.113.11")
		if got := trusted.Resolve(r); got != "172.18.0.2" {
			t.Fatalf("trusting nobody resolved %q, want the peer", got)
		}
		if !trusted.Empty() {
			t.Fatal("Empty() = false")
		}
	}
}

func TestOtherForwardingHeadersAreNeverRead(t *testing.T) {
	trusted := mustTrust(t, "172.18.0.0/16")
	r := requestFrom("172.18.0.2:5000")
	r.Header.Set("X-Real-IP", "203.0.113.10")
	r.Header.Set("Forwarded", "for=203.0.113.11")
	if got := trusted.Resolve(r); got != "172.18.0.2" {
		t.Fatalf("resolved %q from X-Real-IP or Forwarded", got)
	}
}

func TestOverlongTrustedChainFallsBackToPeer(t *testing.T) {
	trusted := mustTrust(t, "10.0.0.0/8")
	hops := make([]string, maxForwardedHops+1)
	for i := range hops {
		hops[i] = "10.0.0.1"
	}
	if got := trusted.Resolve(requestFrom("10.9.9.9:1", strings.Join(hops, ","))); got != "10.9.9.9" {
		t.Fatalf("overlong chain resolved %q, want the peer", got)
	}
}

func TestNewTrustedProxiesRefusesUnsafePrefixes(t *testing.T) {
	for name, prefix := range map[string]netip.Prefix{
		"invalid":       {},
		"v4 any":        netip.MustParsePrefix("0.0.0.0/0"),
		"v6 any":        netip.MustParsePrefix("::/0"),
		"v4-mapped":     netip.MustParsePrefix("::ffff:10.0.0.0/104"),
		"bits too long": netip.PrefixFrom(netip.MustParseAddr("10.0.0.1"), 33),
	} {
		if _, err := NewTrustedProxies(prefix); err == nil {
			t.Errorf("%s: %v accepted", name, prefix)
		}
	}
	trusted, err := NewTrustedProxies(netip.MustParsePrefix("172.18.5.9/16"))
	if err != nil {
		t.Fatal(err)
	}
	if got := trusted.Prefixes(); len(got) != 1 || got[0].String() != "172.18.0.0/16" {
		t.Fatalf("prefixes = %v, want masked", got)
	}
}

func TestClientIPMiddlewareAndFallback(t *testing.T) {
	// Without the middleware, ClientIP is the peer: trusting nobody.
	r := requestFrom("[::ffff:198.51.100.7]:4000", "203.0.113.9")
	if got := ClientIP(r); got != "198.51.100.7" {
		t.Fatalf("fallback ClientIP = %q, want the peer", got)
	}
	trusted := mustTrust(t, "172.18.0.0/16")
	var seen string
	handler := trusted.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = ClientIP(r)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), requestFrom("172.18.0.2:5000", "203.0.113.9"))
	if seen != "203.0.113.9" {
		t.Fatalf("ClientIP behind trusted proxy = %q", seen)
	}
	handler.ServeHTTP(httptest.NewRecorder(), requestFrom("198.51.100.7:4000", "203.0.113.9"))
	if seen != "198.51.100.7" {
		t.Fatalf("ClientIP from untrusted peer = %q, want the peer", seen)
	}
}

func FuzzTrustedProxiesResolve(f *testing.F) {
	trusted, err := NewTrustedProxies(netip.MustParsePrefix("172.18.0.0/16"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add("203.0.113.9")
	f.Add("1.2.3.4, 172.18.0.3")
	f.Add(",,,")
	f.Fuzz(func(t *testing.T, xff string) {
		got := trusted.Resolve(requestFrom("172.18.0.2:5000", xff))
		if _, err := netip.ParseAddr(got); err != nil {
			t.Fatalf("Resolve returned non-address %q", got)
		}
		if got == "172.18.0.2" {
			return
		}
		// Anything else is an entry of the header, in canonical form.
		for _, item := range strings.Split(xff, ",") {
			if addr, err := netip.ParseAddr(strings.TrimSpace(item)); err == nil && addr.WithZone("").Unmap().String() == got {
				return
			}
		}
		t.Fatalf("Resolve returned %q, neither the peer nor an entry of %q", got, xff)
	})
}
