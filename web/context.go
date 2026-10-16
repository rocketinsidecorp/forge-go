// Package web provides the HTTP adapter toolkit for Forge handlers: JSON
// responses, a single error response format, strict request decoding and the
// request-scoped values (request ID, logger) set by the HTTP server.
package web

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
)

type contextKey int

const (
	requestIDKey contextKey = iota
	loggerKey
	proxyInfoKey
)

// ProxyInfo contains request metadata accepted only from an explicitly
// configured trusted proxy. Zero fields mean the proxy did not send a valid
// value for that field.
type ProxyInfo struct {
	ClientIP netip.Addr
	Scheme   string
}

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the ID the server assigned to the current request.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, logger)
}

// Logger returns the request-scoped logger, which carries the request ID. It
// never returns nil.
func Logger(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return discardLogger
}

// WithProxyInfo adds metadata validated by Forge's trusted-proxy middleware.
// Applications should not populate this from request headers directly.
func WithProxyInfo(ctx context.Context, info ProxyInfo) context.Context {
	if info.ClientIP.Zone() != "" {
		info.ClientIP = netip.Addr{}
	}
	if info.Scheme != "http" && info.Scheme != "https" {
		info.Scheme = ""
	}
	if !info.ClientIP.IsValid() && info.Scheme == "" {
		return ctx
	}
	return context.WithValue(ctx, proxyInfoKey, info)
}

// ProxyInfoFromContext returns metadata supplied by trusted-proxy middleware.
func ProxyInfoFromContext(ctx context.Context) (ProxyInfo, bool) {
	info, ok := ctx.Value(proxyInfoKey).(ProxyInfo)
	return info, ok
}

// ClientIP returns the validated forwarded client address, or the socket peer
// address when no trusted proxy supplied a valid address.
func ClientIP(r *http.Request) (netip.Addr, bool) {
	if info, ok := ProxyInfoFromContext(r.Context()); ok && info.ClientIP.IsValid() {
		return info.ClientIP, true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	address, err := netip.ParseAddr(host)
	if err != nil || address.Zone() != "" {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}

// RequestScheme returns a trusted forwarded scheme, or derives it from the
// direct TLS connection. It never trusts Request.URL or raw forwarded headers.
func RequestScheme(r *http.Request) string {
	if info, ok := ProxyInfoFromContext(r.Context()); ok && info.Scheme != "" {
		return info.Scheme
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}
