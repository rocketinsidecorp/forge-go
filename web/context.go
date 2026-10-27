// Package web provides the HTTP adapter toolkit for Forge handlers: JSON
// responses, a single error response format, strict request decoding and the
// request-scoped values (request ID, logger) set by the HTTP server.
package web

import (
	"context"
	"io"
	"log/slog"
	"sync"
)

type contextKey int

const (
	requestIDKey contextKey = iota
	loggerKey
	routeKey
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// WithRequestID returns a context that carries the request ID; the server sets it for every
// request.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the ID the server assigned to the current request.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// WithLogger returns a context that carries the request-scoped logger.
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

// RouteInfo receives the ServeMux pattern of the route that serves a
// request, such as "GET /users/{id}". Middleware that runs before routing
// (telemetry, access logs) installs one with WithRouteInfo and reads it after
// the handler returns; the router fills it through RecordRoute. Labeling
// requests by pattern instead of raw path keeps metric cardinality bounded.
//
// It is safe for concurrent use: with http.TimeoutHandler the router may
// record the route on another goroutine after the middleware has read it.
// The zero value is ready to use.
type RouteInfo struct {
	mu      sync.Mutex
	pattern string
}

// Pattern returns the recorded pattern, or "" when no route matched yet.
func (r *RouteInfo) Pattern() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pattern
}

func (r *RouteInfo) set(pattern string) {
	r.mu.Lock()
	r.pattern = pattern
	r.mu.Unlock()
}

// WithRouteInfo returns ctx carrying info for the router to fill.
func WithRouteInfo(ctx context.Context, info *RouteInfo) context.Context {
	return context.WithValue(ctx, routeKey, info)
}

// RouteInfoFromContext returns the RouteInfo ctx carries, or nil. Middleware
// that needs the matched pattern reuses it instead of installing a second one,
// which would hide the route from the middleware outside it.
func RouteInfoFromContext(ctx context.Context) *RouteInfo {
	info, _ := ctx.Value(routeKey).(*RouteInfo)
	return info
}

// RecordRoute stores the matched pattern in the RouteInfo carried by ctx, if
// any. It does not allocate.
func RecordRoute(ctx context.Context, pattern string) {
	if info, ok := ctx.Value(routeKey).(*RouteInfo); ok && info != nil {
		info.set(pattern)
	}
}
