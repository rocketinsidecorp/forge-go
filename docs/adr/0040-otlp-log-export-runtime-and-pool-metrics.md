# ADR 0040: OTLP log export, Go runtime metrics and connection pool metrics

- **Status:** Accepted
- **Date:** 2026-10-20

## Context

[ADR 0034](0034-dependency-free-telemetry.md) left logs on standard output and
the framework without runtime or connection pool metrics;
[ADR 0038](0038-local-observability-stack.md) worked around the first with
Docker's fluentd log driver and the collector parsing log lines, and shipped a
dashboard with no runtime or pool panels. Both are gaps that production
operators hit first: logs that are not tied to a service and a trace except by
text matching, and no answer to "are we leaking goroutines", "is the heap
growing", "are requests waiting for a database connection".

The constraints of ADR 0034 hold: standard library only, no new module, no
global provider, bounded queues, privacy by construction.

## Decision

### OTLP log export (opt-in)

`FORGE_TELEMETRY_LOGS_EXPORTER=none|otlp` (default `none`) and
`FORGE_TELEMETRY_LOGS_LEVEL=debug|info|warn|error` (default `info`). It is
independent of `FORGE_TELEMETRY_EXPORTER`; the endpoint, headers, timeout,
compression and the production https rule are the existing
`FORGE_TELEMETRY_OTLP_*` settings, so one collector address serves all three
signals. Records are posted to `/v1/logs` with the same OTLP/HTTP JSON encoding
and the same retrying, no-redirect exporter as spans and metrics.

- **Handler.** `Telemetry.LogHandler()` is an `slog.Handler` that converts a
  record into an OpenTelemetry log record and queues it. `forge.New` tees the
  application logger (`telemetry.NewTeeHandler`) into it when the exporter comes
  from configuration, so standard output keeps every line unchanged and
  `docker compose logs`, `forge dev` and log shippers still work. With
  `forge.WithTelemetry` wiring is the caller's.
- **Record.** Body is the message; attributes are the slog attributes with
  groups flattened to dotted names; severity maps slog's levels onto the
  OpenTelemetry ranges; the trace and span ID come from the context given to
  `InfoContext` or from the `trace_id`/`span_id` attributes of the request
  logger and become the record's own fields, not attributes; the resource is the
  one of traces and metrics, so `service.name` joins the three signals. Values are
  resolved through `slog.LogValuer` first, so `config.Secret` exports its
  redaction; other values are encoded as the standard-output handler encodes
  them (`slog.JSONHandler` semantics in production, `slog.TextHandler` in
  `development`/`test`, chosen by `FromConfig` from the logger it is given), so
  the exporter never carries a field standard output hides. Body (4096 bytes),
  string values (1024) and attribute count (64) are bounded.
- **Backpressure.** The span batcher became generic and serves log records
  too: a bounded queue (2048), one goroutine owned by `Telemetry`, batches of up
  to 512 every second, a full queue drops and counts
  (`forge_telemetry_logs_dropped_total`), logging never blocks a request. A
  failed export drops its batch after the OTLP retries and counts
  (`forge_telemetry_export_errors_total{signal="logs"}`).
- **No feedback.** Export failures are logged with the logger the telemetry was
  built with, which `forge.New` keeps as the plain stdout logger; they never
  enter the log queue, so a down collector cannot generate the records it
  fails to send. A test asserts it.
- **Shutdown.** `Telemetry.Shutdown` now stops spans, log records and metrics
  concurrently within the application's shutdown budget; what is queued at the
  deadline is dropped and counted. Lines logged after shutdown (the final
  "application stopped") reach standard output only.
- **Privacy.** The exporter sends what the logger writes to standard output,
  nothing more. ADR 0034's rule that error messages are never recorded concerns
  the instrumentation the framework adds (spans, metrics); application log calls
  stay the application's responsibility, as on standard output. This is stated in
  OBSERVABILITY.md and the threat model.

Encoding stays JSON, like traces and metrics; protobuf and gRPC remain out of
scope.

### Runtime and pool metrics

A new `Registry.Observe(key, fn)` runs callbacks before each collection (a
Prometheus scrape or an OTLP push), serialized, so state that is cheaper to read
than to push is read on demand and no goroutine is added. `Counter.Observe` and
`FloatCounter.Observe` record running totals read from elsewhere and never
decrease.

- **Go runtime**, always on when telemetry is enabled, read with
  `runtime/metrics` (no stop-the-world): `go.goroutine.count`,
  `go.memory.heap.alloc`, `go.memory.heap.inuse`, `go.memory.total`,
  `go.memory.gc.goal`, `go.memory.allocated`, `go.gc.cycles`,
  `go.gc.pause.duration` (a histogram folded from the runtime's pause histogram;
  the sum is estimated from bucket midpoints) and `go.processor.limit`. Names follow the OpenTelemetry Go
  runtime conventions where one exists.
- **Connection pools** for `postgres` (pgx), `mysql` and `sqlite`
  (`database/sql`) opened `WithTelemetry`: `db.client.connection.count`
  (`idle`/`used`), `db.client.connection.max` (OpenTelemetry database client
  names), `forge.db.pool.waits` and `forge.db.pool.wait.duration`. The pool name
  label is `default` or `WithPoolName(...)`; registering a name again replaces the
  pool. Waits are not exactly comparable: `database/sql` counts waits at the pool
  limit, pgx also waits for a new connection to be established.

### Local stack

The generated overlay sets `FORGE_TELEMETRY_LOGS_EXPORTER=otlp` on `app` and the
collector's logs pipeline is `otlp -> memory_limiter, batch -> Loki`. The Docker
**fluentd log driver is removed**, with the collector's `fluent_forward`
receiver, its `transform/logs` parsing, the published port 24224 and the
rootless-Docker caveat. Reasons: the application's own logs (the only logs the
framework has) now arrive structured, with real severity and trace context, no
regular-expression parsing and no dependency on a Docker feature; the app
starts with or without the collector; one fewer published port. What is lost:
output that bypasses the logger (a crash's stack trace, a library printing to
stderr) is no longer in Loki, but it is still in `docker compose logs`. The
`migrate` one-off never had log shipping. Because `trace_id` is now structured
metadata and no longer part of the line, Grafana's links changed: the Loki data
source's derived field reads the `trace_id` label, and Tempo's "Logs for this
span" uses a custom query (`{service_name="..."} | trace_id="..."`) instead of a
line filter. The dashboard gains connection pool, Go runtime and dropped-log
panels; a test checks every metric it queries is documented.

## Consequences

- Logs, traces and metrics share `service.name` and exporter settings, so one
  collector address and one set of variables cover everything; the log
  exporter is off unless asked for.
- Dashboards and alerts can use runtime and pool metrics from both `/metrics`
  and OTLP, without another library.
- Forge maintains log encoding, severity mapping and a second user of the
  batcher; the OTLP logs JSON mapping is stable in OTLP 1.x.
- An application that sets `FORGE_TELEMETRY_LOGS_EXPORTER=otlp` sends all its
  log lines to the collector: operators must treat the collector like the log
  store. Logging a raw secret leaks it there as it does to standard output.
- Pool statistics need the adapter opened with `WithTelemetry`; a pool created
  elsewhere can be observed with `Telemetry.ObservePool`.
- Not implemented: log sampling, per-record rate limits, log events for
  spans, exemplars, OTLP protobuf.

## Forge-Rust port

Forge-Rust follows the same contract: the variables `FORGE_TELEMETRY_LOGS_EXPORTER`
and `FORGE_TELEMETRY_LOGS_LEVEL` reusing the existing OTLP endpoint settings; a
tracing-subscriber layer (not a second logger) that tees to stdout and a bounded
non-blocking batch exporter with drop-and-count, failure diagnostics kept off the
exported path and a bounded flush at shutdown; the same record mapping (body is
the message, attributes flattened with dots, severity ranges, trace and span ID
as fields, `service.name` shared with traces and metrics, secret types
redacted); and the same metric names and units: `go.*` becomes the process
metrics Rust can read cheaply (resident memory, allocations if a counting
allocator is opted in, task and thread counts; there is no goroutine or GC
equivalent), `db.client.connection.count`/`.max`, `forge.db.pool.waits` and
`forge.db.pool.wait.duration` from the `sqlx` pool (`size`, `num_idle`, plus the
wait totals the Forge pool wrapper counts). The generated stack files are the
same as Forge-Go's: the log-driver block, the `fluent_forward` receiver and
port 24224 are not generated, the collector logs pipeline is `otlp`, and the
Grafana provisioning and dashboard JSON can be copied verbatim as long as the
metric names and labels match. A follow-up in Forge-Rust tracks the port.
