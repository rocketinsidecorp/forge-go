// Package web provides the HTTP adapter toolkit for Forge handlers: JSON
// responses, a single error response format, strict request decoding and the
// request-scoped values (request ID, logger) set by the HTTP server.
package web

import (
	"context"
	"io"
	"log/slog"
)

type contextKey int

const (
	requestIDKey contextKey = iota
	loggerKey
	routeKey
)

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

// RouteInfo receives the ServeMux pattern of the route that serves a
// request, such as "GET /users/{id}". Middleware that runs before routing
// (telemetry, access logs) installs one with WithRouteInfo and reads it after
// the handler returns; the router fills it through RecordRoute. Labeling
// requests by pattern instead of raw path keeps metric cardinality bounded.
type RouteInfo struct {
	Pattern string
}

// WithRouteInfo returns ctx carrying info for the router to fill.
func WithRouteInfo(ctx context.Context, info *RouteInfo) context.Context {
	return context.WithValue(ctx, routeKey, info)
}

// RecordRoute stores the matched pattern in the RouteInfo carried by ctx, if
// any. It does not allocate.
func RecordRoute(ctx context.Context, pattern string) {
	if info, ok := ctx.Value(routeKey).(*RouteInfo); ok && info != nil {
		info.Pattern = pattern
	}
}
