package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/rocketinsidecorp/forge-go/config"
)

// maxForwardedHops bounds the X-Forwarded-For entries examined per request.
// A chain longer than this made only of trusted proxies is not a real
// deployment; the peer address is used instead.
const maxForwardedHops = 32

type clientIPKey struct{}

// TrustedProxies is the set of networks whose X-Forwarded-For header is
// believed (ADR 0037). The zero value trusts nobody, so the client address
// is always the TCP peer. It is immutable and safe for concurrent use.
type TrustedProxies struct {
	prefixes []netip.Prefix
}

// NewTrustedProxies returns the set of the given prefixes (for a single
// proxy, a /32 or /128); config.HTTP.TrustedProxies holds them, parsed from
// FORGE_HTTP_TRUSTED_PROXIES under the same rules, which are
// config.CheckTrustedProxy: it refuses invalid prefixes, IPv4 prefixes
// shorter than /8 and IPv6 prefixes shorter than /16 (they would let almost
// any client choose its address) and IPv4-mapped IPv6 prefixes (peers are
// compared in their IPv4 form, so such a prefix would silently never match;
// write the IPv4 form).
func NewTrustedProxies(prefixes ...netip.Prefix) (TrustedProxies, error) {
	normalized := make([]netip.Prefix, 0, len(prefixes))
	for i, prefix := range prefixes {
		if err := config.CheckTrustedProxy(prefix); err != nil {
			return TrustedProxies{}, fmt.Errorf("trusted proxy %d %w", i+1, err)
		}
		normalized = append(normalized, prefix.Masked())
	}
	return TrustedProxies{prefixes: normalized}, nil
}

// Prefixes returns a copy of the trusted prefixes.
func (t TrustedProxies) Prefixes() []netip.Prefix { return slices.Clone(t.prefixes) }

// Empty reports whether nobody is trusted.
func (t TrustedProxies) Empty() bool { return len(t.prefixes) == 0 }

func (t TrustedProxies) trusts(addr netip.Addr) bool {
	for _, prefix := range t.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// Resolve returns the address of the client that sent r.
//
// When the TCP peer (r.RemoteAddr) is not a trusted proxy, the result is the
// peer and every forwarding header is ignored. When it is, X-Forwarded-For
// is read from the right (the entry the peer itself appended) towards the
// left, skipping trusted proxies, and the first untrusted address is the
// client: entries to its left were written by the client or by proxies
// nobody vouches for. If an entry on that walk is empty or not a bare IP
// address, the result is the peer: a malformed chain is never resolved to a
// value the client may have chosen. If every entry is trusted, the leftmost
// one is the client. Forwarded and X-Real-IP are never read.
//
// Addresses are returned in canonical form, without zone, with
// IPv4-mapped IPv6 addresses as IPv4. A peer that is not an IP address (a
// Unix socket, a test's "pipe") is returned unchanged without its port.
func (t TrustedProxies) Resolve(r *http.Request) string {
	host := peerHost(r.RemoteAddr)
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.WithZone("").Unmap()
	if !t.trusts(peer) {
		return peer.String()
	}
	client, hops := peer, 0
	values := r.Header.Values("X-Forwarded-For")
	for i := len(values) - 1; i >= 0; i-- {
		rest := values[i]
		for {
			comma := strings.LastIndexByte(rest, ',')
			addr, err := netip.ParseAddr(strings.TrimSpace(rest[comma+1:]))
			if err != nil {
				return peer.String()
			}
			addr = addr.WithZone("").Unmap()
			if !t.trusts(addr) {
				return addr.String()
			}
			if hops++; hops > maxForwardedHops {
				return peer.String()
			}
			client = addr
			if comma < 0 {
				break
			}
			rest = rest[:comma]
		}
	}
	return client.String()
}

// Middleware stores the resolved client address in each request's context,
// where ClientIP reads it. The HTTP server installs it outside all other
// middleware (see httpserver.NewWithOptions).
func (t TrustedProxies) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey{}, t.Resolve(r))))
	})
}

// ClientIP returns the client address of r: the one TrustedProxies.Middleware
// resolved, or, when no resolver ran, the TCP peer address (the same result
// as trusting nobody). Login throttles, rate limits, stream caps and audit
// sources use it. It is a full address; callers that count clients bucket
// IPv6 addresses themselves (by /64).
func ClientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok {
		return ip
	}
	return TrustedProxies{}.Resolve(r)
}

func peerHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
