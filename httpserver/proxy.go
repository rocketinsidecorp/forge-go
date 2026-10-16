package httpserver

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/fabriciobonjorno/forge-go/web"
)

const (
	maxForwardedForBytes = 4096
	maxForwardedForHops  = 32
)

func trustedProxy(prefixes []netip.Prefix, next http.Handler) http.Handler {
	if len(prefixes) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, ok := remoteIP(r.RemoteAddr)
		if !ok || !containsIP(prefixes, peer) {
			next.ServeHTTP(w, r)
			return
		}

		info := web.ProxyInfo{}
		if client, ok := forwardedClientIP(r.Header.Values("X-Forwarded-For"), prefixes); ok {
			info.ClientIP = client
		}
		if scheme, ok := forwardedScheme(r.Header.Values("X-Forwarded-Proto")); ok {
			info.Scheme = scheme
		}
		ctx := web.WithProxyInfo(r.Context(), info)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func remoteIP(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	address, err := netip.ParseAddr(host)
	if err != nil || address.Zone() != "" {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}

func containsIP(prefixes []netip.Prefix, address netip.Addr) bool {
	address = address.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func forwardedClientIP(values []string, trusted []netip.Prefix) (netip.Addr, bool) {
	if len(values) == 0 {
		return netip.Addr{}, false
	}
	value := strings.Join(values, ",")
	if len(value) > maxForwardedForBytes {
		return netip.Addr{}, false
	}
	parts := strings.Split(value, ",")
	if len(parts) == 0 || len(parts) > maxForwardedForHops {
		return netip.Addr{}, false
	}
	addresses := make([]netip.Addr, 0, len(parts))
	for _, part := range parts {
		address, err := netip.ParseAddr(strings.TrimSpace(part))
		if err != nil || address.Zone() != "" {
			return netip.Addr{}, false
		}
		addresses = append(addresses, address.Unmap())
	}
	for index := len(addresses) - 1; index >= 0; index-- {
		if !containsIP(trusted, addresses[index]) {
			return addresses[index], true
		}
	}
	return netip.Addr{}, false
}

func forwardedScheme(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(values[0])) {
	case "http":
		return "http", true
	case "https":
		return "https", true
	default:
		return "", false
	}
}
