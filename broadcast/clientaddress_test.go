package broadcast

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/rocketinsidecorp/forge-go/web"
)

// The anonymous per-client stream cap counts the resolved client (ADR
// 0037), IPv6 by /64, and ignores forwarded headers from untrusted peers.
func TestClientAddressUsesResolvedClient(t *testing.T) {
	trusted, err := web.NewTrustedProxies(netip.MustParsePrefix("172.18.0.0/16"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ remote, xff, want string }{
		{"172.18.0.2:1", "203.0.113.9", "203.0.113.9"},
		{"172.18.0.2:1", "2001:db8:1:2::7", "2001:db8:1:2::/64"},
		{"198.51.100.7:1", "203.0.113.9", "198.51.100.7"},
		{"[2001:db8:5:6::1]:1", "203.0.113.9", "2001:db8:5:6::/64"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/streams", nil)
		r.RemoteAddr = tc.remote
		r.Header.Set("X-Forwarded-For", tc.xff)
		var got string
		trusted.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			got = clientAddress(r)
		})).ServeHTTP(httptest.NewRecorder(), r)
		if got != tc.want {
			t.Errorf("clientAddress(%s, %q) = %q, want %q", tc.remote, tc.xff, got, tc.want)
		}
	}
}
