# ADR 0034: Dependency-free telemetry speaking OpenTelemetry protocols

- **Status:** Accepted
- **Date:** 2026-10-19

## Context

Phase 5 asks for server spans with W3C trace-context propagation,
bounded-cardinality HTTP metrics with an export path, logs correlated with
traces, and database and background-job instrumentation, without coupling
the standard-library core to an exporter ([ADR 0006](0006-zero-dependency-core.md)).

The OpenTelemetry Go SDK would bring roughly twenty modules (otel, sdk,
metric, otlp exporters, gRPC, protobuf) into every application, and its
exporters into the core. A separate module was considered; it would still
put those modules into every instrumented application and split versioning.
What applications need is narrow: start spans, propagate context, record a
few histograms, and send them somewhere standard.

## Decision

Forge implements the needed subset of OpenTelemetry in the core package
`telemetry`, on the standard library only, and interoperates through the
standard protocols instead of the SDK:

- **Propagation:** W3C `traceparent`/`tracestate`, strictly parsed and
  fuzz-tested. Incoming context is untrusted by default: each request starts
  a new root trace unless `FORGE_TELEMETRY_TRUST_TRACEPARENT=true`, because a
  trusted sampled parent lets any client force recording past the sample
  ratio, choose trace IDs and forward `tracestate`.
- **Tracing:** a minimal tracer (kind, attributes, events, status), IDs from
  `crypto/rand`, parent-based `TraceIdRatioBased` sampling compatible with
  other OpenTelemetry services. Spans are recorded only when sampled and an
  exporter is configured; otherwise they still carry IDs for log correlation.
- **Export:** OTLP/HTTP with the JSON encoding (protobuf JSON mapping),
  implemented with `net/http`: bounded retries with backoff and jitter on
  429/502/503/504 and network errors, `Retry-After`, timeouts, optional gzip,
  secret headers, no redirects. A log exporter serves development.
- **Batching:** a bounded queue (2048) drained by one goroutine owned by the
  `Telemetry` value; full queue drops and counts; `Shutdown` flushes within
  the application's shutdown budget and is registered as the first
  `OnShutdown` hook, so it runs last.
- **Metrics:** counters, gauges and explicit-bucket histograms with label
  names fixed at registration and a per-instrument series cap (1000, then an
  `_overflow` series and a counter). Exposed in the Prometheus text format on
  a **separate listener** (`FORGE_TELEMETRY_METRICS_ADDR`, off by default;
  a non-loopback address needs `FORGE_TELEMETRY_METRICS_PUBLIC=true` in
  production)
  and pushed with OTLP when that exporter is selected.
- **Configuration:** `FORGE_TELEMETRY_*`, validated at startup; telemetry is
  enabled with exporter `none` by default. `FORGE_TELEMETRY_ENABLED=false`
  installs nothing, and the disabled path is proven allocation-free relative
  to a server without telemetry code.
- **Instrumentation:** the HTTP server middleware sits in a dedicated
  `httpserver.Options.Instrument` slot inside the request-ID layer and outside
  panic recovery; the router reports the matched pattern through
  `web.RouteInfo`. Adapters take an explicit `WithTelemetry` option or
  `Telemetry` field (nil-safe); there is no global provider.
- **Privacy by construction:** no raw paths, query strings, headers, bodies,
  SQL text or arguments, payloads, recipients or error messages are recorded;
  errors become `error.type`.

## Consequences

- Applications gain traces, metrics and correlated logs with no new module,
  and any OpenTelemetry Collector, Prometheus, Grafana stack or OTLP-capable
  vendor can consume them.
- Forge maintains the encoding and propagation code and must follow OTLP and
  W3C changes itself; the JSON encoding is stable in OTLP 1.x.
- Not implemented: OTLP gRPC/protobuf, logs export, baggage, exemplars,
  exponential histograms, delta temporality, span links. Jobs start new
  traces and are linked to their enqueuing request by job ID rather than by
  stored context, to avoid a queue schema change.
- Applications that need the full SDK can still use it in their own code;
  traces cross between services instrumented by Forge and by the SDK through
  `traceparent`, but within one process the two keep separate span contexts.
