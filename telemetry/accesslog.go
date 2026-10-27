package telemetry

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/rocketinsidecorp/forge-go/web"
)

// AccessLogOptions configure AccessLog.
type AccessLogOptions struct {
	// Level is the level of the request line: Info in development, Debug
	// elsewhere (config.Config.EventLevel).
	Level slog.Level
	// CountQueries adds the number of statements the request ran
	// (queries=N), counted by the database adapters (QueryCounter). Set it
	// only when they log, that is when telemetry is enabled: without them
	// the count would always read 0.
	CountQueries bool
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// AccessLog returns middleware that writes one line after each request, with
// the request logger (web.Logger, so the request ID and trace IDs ride
// along): LogEventHTTP with method, route, status, duration and, with
// CountQueries, queries. It logs the route pattern ("/posts/{id}"), never the
// path or the query string, which hold identifiers and personal data and
// would make every request a new value. A request no route matched has an
// empty route. When the level is not enabled the middleware does nothing.
//
// Install it inside the telemetry middleware, so the request logger carries
// the trace; it reuses the web.RouteInfo that middleware installed.
func AccessLog(opts AccessLogOptions) func(http.Handler) http.Handler {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			logger := web.Logger(ctx)
			if !logger.Enabled(ctx, opts.Level) {
				next.ServeHTTP(w, r)
				return
			}
			route := web.RouteInfoFromContext(ctx)
			if route == nil {
				route = &web.RouteInfo{}
				ctx = web.WithRouteInfo(ctx, route)
			}
			var counter *QueryCounter
			if opts.CountQueries {
				ctx, counter = WithQueryCounter(ctx)
			}
			method := NormalizeMethod(r.Method)
			recorder := &statusRecorder{ResponseWriter: w}
			start := now()
			defer func() {
				status := recorder.statusCode()
				recovered := recover()
				if recovered != nil {
					status = http.StatusInternalServerError
				}
				attrs := make([]slog.Attr, 0, 5)
				attrs = append(attrs,
					slog.String("method", method),
					slog.String("route", RoutePath(route.Pattern())),
					slog.Int("status", status),
					slog.Duration("duration", now().Sub(start)),
				)
				if counter != nil {
					attrs = append(attrs, slog.Int("queries", counter.Count()))
				}
				logger.LogAttrs(ctx, opts.Level, LogEventHTTP, attrs...)
				if recovered != nil {
					panic(recovered)
				}
			}()
			next.ServeHTTP(recorder, r.WithContext(ctx))
		})
	}
}
