// Package observability provides optional instrumentation adapters for Forge.
package observability

import (
	"net/http"

	"github.com/fabriciobonjorno/forge-go/httpserver"
	"github.com/fabriciobonjorno/forge-go/web"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// HTTPConfig selects OpenTelemetry providers for HTTP instrumentation. Nil
// providers use the OpenTelemetry global providers. A nil Propagator defaults
// to W3C Trace Context; baggage is deliberately not accepted by default.
type HTTPConfig struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	Propagator     propagation.TextMapPropagator
}

// HTTPMiddleware instruments Forge HTTP requests with server spans and the
// standard HTTP server duration metric. Route templates, never raw URL paths,
// are used in span names when available. No exporter or backend is included;
// applications configure providers and export pipelines themselves.
func HTTPMiddleware(cfg HTTPConfig) httpserver.Middleware {
	// otelhttp's default formatter follows HTTP semantic conventions and
	// derives low-cardinality names from Request.Pattern.
	var options []otelhttp.Option
	if cfg.TracerProvider != nil {
		options = append(options, otelhttp.WithTracerProvider(cfg.TracerProvider))
	}
	if cfg.MeterProvider != nil {
		options = append(options, otelhttp.WithMeterProvider(cfg.MeterProvider))
	}
	if cfg.Propagator != nil {
		options = append(options, otelhttp.WithPropagators(cfg.Propagator))
	} else {
		options = append(options, otelhttp.WithPropagators(propagation.TraceContext{}))
	}
	instrument := otelhttp.NewMiddleware("forge.http", options...)
	return func(next http.Handler) http.Handler {
		// Add trace identifiers to the request logger only after otelhttp has
		// created/extracted the active server span.
		instrumented := instrument(withTraceLogger(next))
		if routes, ok := next.(interface {
			RoutePattern(*http.Request) string
		}); ok {
			return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if pattern := routes.RoutePattern(request); pattern != "" {
					request = request.Clone(request.Context())
					request.Pattern = pattern
				}
				instrumented.ServeHTTP(w, request)
			})
		}
		return instrumented
	}
}

func withTraceLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		spanContext := trace.SpanFromContext(request.Context()).SpanContext()
		if spanContext.IsValid() {
			logger := web.Logger(request.Context()).With(
				"trace_id", spanContext.TraceID().String(),
				"span_id", spanContext.SpanID().String(),
			)
			ctx := web.WithLogger(request.Context(), logger)
			request = request.WithContext(ctx)
		}
		next.ServeHTTP(w, request)
	})
}
