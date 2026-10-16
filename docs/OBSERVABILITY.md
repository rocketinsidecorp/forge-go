# Observability

Forge keeps observability integrations optional. Core runtime packages remain
standard-library-only; applications opt into OpenTelemetry HTTP instrumentation
by adding the middleware to the Forge composition:

```go
app, err := forge.New(
	forge.WithHTTPMiddleware(observability.HTTPMiddleware(observability.HTTPConfig{
		TracerProvider: tracerProvider,
		MeterProvider:  meterProvider,
	})),
)
```

`tracerProvider` and `meterProvider` are configured by the application. Forge
does not install a global provider, exporter, collector, or hosted service.
Applications can use the OpenTelemetry global providers by leaving provider
fields nil, and can choose their own exporters, sampling, resource attributes,
and shutdown lifecycle. Register provider shutdown with `App.OnShutdown`.

The HTTP middleware creates server spans and the standard OpenTelemetry HTTP
server metrics, including request duration, for requests that reach the
configured application middleware. Earlier server-generated responses (such as
request-ID failures or body-limit rejections) occur outside this middleware and
are not currently instrumented. It extracts W3C Trace Context by
default; baggage is not extracted unless the application explicitly supplies a
propagator. When the handler is Forge's router, spans and metric attributes use
the matched route template (for example, `GET /users/{id}` and
`http.route=/users/{id}`), never the concrete URL path. Unmatched requests use a
bounded fallback span name and do not receive a route attribute.

Within an active request span, Forge enriches the request-scoped logger with
`trace_id` and `span_id`. Use `web.Logger(ctx).InfoContext(ctx, ...)` (or the
corresponding level method) for correlated records. This does not replace the
global `slog` logger or automatically correlate direct `slog.InfoContext` calls;
applications that need that behavior can configure an OpenTelemetry `slog`
bridge themselves.

Scheduled jobs can be wrapped with `observability.TraceJob(provider, name,
job)`. It starts one internal span per run, passes the span context to the job,
and records a generic failure event/status while preserving the original error
for the scheduler's error callback. Use the scheduler's stable registration
name, not a per-run value.

```go
err := scheduler.Register(
	"daily-cleanup",
	spec,
	observability.TraceJob(tracerProvider, "daily-cleanup", cleanup),
)
```

The official `otelhttp` instrumentation preserves the normal HTTP response
writer interfaces. Do not record request/response bodies, credentials, session
tokens, tenant IDs, or other sensitive values as span attributes. At production
traffic levels, configure sampling and histogram aggregation in the application
to control telemetry volume and cost.

The OpenTelemetry Go SDK records recovered panic values on spans by default.
If panic values may contain sensitive data, configure the application's SDK
tracer provider with `sdktrace.WithoutPanicRecording()` and rely on the
framework's generic HTTP error response; configure application logging to
redact panic values as needed.

This integration currently covers inbound HTTP and scheduled-job spans.
Database operations, outbound HTTP clients, the PostgreSQL outbox worker,
global `slog` bridging, and a framework-owned exporter pipeline remain outside
the current implementation.
