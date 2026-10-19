# Observability

Forge applications produce traces, metrics and trace-correlated logs without
any third-party module: the `telemetry` package implements the part of
OpenTelemetry an application needs on the standard library and speaks the
OpenTelemetry wire protocol (OTLP/HTTP with JSON encoding), so any
OpenTelemetry Collector or compatible backend can receive the data
([ADR 0026](adr/0026-dependency-free-telemetry.md)).

Telemetry is **on by default and exports nothing by default**. Out of the box
every request has a trace ID that appears in its logs and is propagated to
downstream services; incoming trace headers are ignored unless you trust
them; metrics are kept in memory. You choose an export path:

| Goal | Setting |
| ---- | ------- |
| Prometheus scrapes the process | `FORGE_TELEMETRY_METRICS_ADDR=127.0.0.1:9464` (a separate listener serving `GET /metrics`) |
| Traces and metrics to an OpenTelemetry Collector | `FORGE_TELEMETRY_EXPORTER=otlp`, `FORGE_TELEMETRY_OTLP_ENDPOINT=http://collector:4318` |
| See spans while developing | `FORGE_TELEMETRY_EXPORTER=log` |
| No telemetry at all (zero cost) | `FORGE_TELEMETRY_ENABLED=false` |

## Quick start

```bash
# 1. Metrics for Prometheus on a private port
FORGE_TELEMETRY_METRICS_ADDR=127.0.0.1:9464 forge dev
curl -s http://127.0.0.1:9464/metrics | grep http_server_request_duration

# 2. Spans in the development log
FORGE_TELEMETRY_EXPORTER=log forge dev

# 3. Traces and metrics to a local OpenTelemetry Collector (config below)
docker run --rm -p 4318:4318 \
  -v "$PWD/otel-collector.yaml:/etc/otelcol/config.yaml:ro" \
  otel/opentelemetry-collector:0.111.0
FORGE_TELEMETRY_EXPORTER=otlp \
FORGE_TELEMETRY_OTLP_ENDPOINT=http://127.0.0.1:4318 \
FORGE_TELEMETRY_SERVICE_NAME=billing \
forge dev
```

Pin the collector image to a current release in real deployments.

## Configuration

All variables are validated at startup; errors name the variable and never
echo its value.

| Variable | Default | Notes |
| -------- | ------- | ----- |
| `FORGE_TELEMETRY_ENABLED` | `true` | `false` installs no middleware and no instruments |
| `FORGE_TELEMETRY_SERVICE_NAME` | `unknown_service:<binary>` | `service.name` resource attribute; set it |
| `FORGE_TELEMETRY_SERVICE_VERSION` | (empty) | `service.version` |
| `FORGE_TELEMETRY_EXPORTER` | `none` | `none`, `otlp`, or `log` (refused in `production`) |
| `FORGE_TELEMETRY_OTLP_ENDPOINT` | (empty) | Collector base URL; Forge posts to `/v1/traces` and `/v1/metrics`. Required for `otlp`. `production` requires `https` unless `FORGE_TELEMETRY_OTLP_ALLOW_PLAINTEXT=true`. No credentials, query or fragment. |
| `FORGE_TELEMETRY_OTLP_HEADERS` | (empty) | `name=value,name2=value2`, values percent-encoded (the `OTEL_EXPORTER_OTLP_HEADERS` format), e.g. `authorization=Bearer%20TOKEN`. Secret: redacted everywhere. |
| `FORGE_TELEMETRY_OTLP_TIMEOUT` | `10s` | Per export, retries included; at most `1m` |
| `FORGE_TELEMETRY_OTLP_COMPRESSION` | `none` | `none` or `gzip` |
| `FORGE_TELEMETRY_OTLP_ALLOW_PLAINTEXT` | `false` | Permit `http://` endpoints in `production` (sidecar or private network) |
| `FORGE_TELEMETRY_SAMPLE_RATIO` | `1` | Fraction of new traces sampled, `0`-`1` |
| `FORGE_TELEMETRY_TRUST_TRACEPARENT` | `false` | Continue traces from incoming `traceparent`/`tracestate` headers. Enable only behind a proxy or gateway that sets or sanitizes them. |
| `FORGE_TELEMETRY_METRICS_ADDR` | (empty: off) | `host:port` of the Prometheus listener; must differ from `FORGE_HTTP_ADDR` |
| `FORGE_TELEMETRY_METRICS_PUBLIC` | `false` | Confirms a non-loopback `FORGE_TELEMETRY_METRICS_ADDR` (such as `0.0.0.0:9464` in a container). Without it `production` refuses to start and other environments log a warning. |
| `FORGE_TELEMETRY_METRICS_INTERVAL` | `1m` | OTLP metric push interval, at least `1s` |

## What is recorded

### Traces

| Source | Span | Attributes |
| ------ | ---- | ---------- |
| HTTP server (every request) | `GET /users/{id}` (server; `GET` when no route matched) | `http.request.method`, `http.route`, `http.response.status_code`, `url.scheme`, `network.protocol.version`, `error.type` for 5xx |
| `telemetry.Transport` (outgoing HTTP) | `GET` (client) | `http.request.method`, `server.address`, `server.port`, `http.response.status_code`, `error.type` |
| `postgres`, `mysql`, `sqlite` with `WithTelemetry` | `SELECT`, `INSERT`... (client) | `db.system.name`, `db.operation.name`, `error.type` |
| `queue.Enqueue` inside a trace | `publish <queue>` (producer) | `messaging.*`, `messaging.message.id` (job ID), `forge.job.kind` |
| `queue.Worker` | `process <queue>` (consumer, new trace) | `messaging.*`, job ID, kind, attempt, `forge.job.outcome` |
| `outbox/postgres.Dispatcher` | `process outbox` (consumer, new trace) | event ID, type, version, attempt, `forge.outbox.outcome` |
| `jobs.Scheduler` | `job <name>` (new trace) | `forge.scheduler.job`, `forge.scheduler.outcome` |
| `mail.Instrument` inside a trace | `mail send` (client) | `error.type` |

Database, outgoing HTTP and mail spans are created only inside a traced
operation (a request, a job, a delivery), so pollers and startup queries
never create root spans. Their metrics are always recorded.

Span limits: 64 attributes, 32 events, string values truncated to 1024 bytes.
Errors are recorded as `error.type` only (a `fault` code such as
`not_found`, `timeout`, `canceled`, or the Go type of the innermost error).

### Metrics

| Metric (Prometheus name) | Type | Labels |
| ------------------------ | ---- | ------ |
| `http.server.request.duration` (`http_server_request_duration_seconds`) | histogram | `http_request_method`, `http_route`, `http_response_status_class` |
| `http.server.active_requests` (`http_server_active_requests`) | gauge | `http_request_method` |
| `http.client.request.duration` (`http_client_request_duration_seconds`) | histogram | `http_request_method`, `http_response_status_class` |
| `db.client.operation.duration` (`db_client_operation_duration_seconds`) | histogram | `db_system_name`, `db_operation_name`, `error_type` |
| `forge.queue.job.duration` (`forge_queue_job_duration_seconds`) | histogram | `messaging_destination_name`, `forge_job_kind`, `forge_job_outcome` |
| `forge.outbox.dispatch.duration` (`forge_outbox_dispatch_duration_seconds`) | histogram | `forge_event_type`, `forge_outbox_outcome` |
| `forge.scheduler.job.duration` (`forge_scheduler_job_duration_seconds`) | histogram | `forge_scheduler_job`, `forge_scheduler_outcome` |
| `forge.mail.send.duration` (`forge_mail_send_duration_seconds`) | histogram | `forge_mail_outcome` |
| `forge.telemetry.spans.dropped` (`forge_telemetry_spans_dropped_total`) | counter | |
| `forge.telemetry.export.errors` (`forge_telemetry_export_errors_total`) | counter | `signal` |
| `forge.telemetry.metric.overflows` (`forge_telemetry_metric_overflows_total`) | counter | |

Duration buckets (seconds) follow the OpenTelemetry advice: 0.005, 0.01,
0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10. Counters and
histograms are cumulative. The application's own metrics use the same
registry:

```go
orders, err := app.Telemetry().Metrics().Counter("shop.orders.created", "Orders created.", "{order}", "channel")
// handle err
orders.Add(1, "web")
```

### Logs

Request logs (`web.Logger(ctx)`) carry `trace_id` and `span_id`. Loggers built
by `forge.Main` wrap their handler with `telemetry.NewLogHandler`, so any
`logger.InfoContext(ctx, ...)` with a traced context is correlated too. Search
your log store for a `trace_id` from a trace, or the reverse.

## Cardinality rules

Every label value comes from a bounded set chosen by code, never from
request input:

- HTTP routes are the matched `ServeMux` pattern (`/users/{id}`), never the
  raw path. Requests no route matched (404, 405, 400, 413) have an empty
  route label.
- Methods outside the standard set are recorded as `_OTHER`; statuses as
  classes (`2xx`...`5xx`).
- Database operations are an allowlisted first keyword (`SELECT`...) or
  `OTHER`; errors are the database-independent kind (`unique_violation`...).
- Queue kinds, event types and scheduler job names are the names the
  application registered.

As a backstop each instrument holds at most 1000 series (label
combinations). New combinations beyond that are folded into one series whose
labels are all `_overflow`, and `forge_telemetry_metric_overflows_total`
counts them: an alert on that counter means a label is fed by unbounded data.
Label values are truncated to 128 bytes. Labels must be declared when an
instrument is created; there is no way to add one per measurement.

## What is never recorded

Request and response bodies, query strings, raw URL paths, headers, cookies,
client IP addresses, user agents, SQL text, SQL arguments, job payloads,
event payloads, mail recipients, subjects and bodies, and error messages.
Error messages are excluded because they routinely embed identifiers, SQL
values or addresses; the error type is recorded instead. Span names are
route patterns, statement verbs, queue names or registered job names.

Application code that adds attributes (`span.SetAttributes`) is responsible
for keeping personal data out of them.

## Export paths

### OpenTelemetry Collector

`otel-collector.yaml`, printing what it receives and forwarding nowhere:

```yaml
receivers:
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318
processors:
  batch: {}
exporters:
  debug:
    verbosity: basic
service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [debug]
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [debug]
```

Replace `debug` with your backend's exporter (`otlphttp` to Grafana Tempo,
Jaeger, Honeycomb, Grafana Cloud, and so on). Keeping a collector between the
application and a vendor confines vendor credentials and tail sampling to the
collector; the application only needs the collector's address. When the
application talks to a vendor directly, put the API key in
`FORGE_TELEMETRY_OTLP_HEADERS`.

The exporter sends batches of up to 512 spans at least every 5 seconds and
metrics every `FORGE_TELEMETRY_METRICS_INTERVAL`. It retries network errors
and HTTP 429/502/503/504 up to 5 times with exponential backoff and jitter,
honors `Retry-After`, never follows redirects, and gives up within
`FORGE_TELEMETRY_OTLP_TIMEOUT`.

### Prometheus and Grafana

Set `FORGE_TELEMETRY_METRICS_ADDR` and scrape it:

```yaml
# prometheus.yml
scrape_configs:
  - job_name: forge
    scrape_interval: 15s
    static_configs:
      - targets: ["127.0.0.1:9464"]
```

```bash
docker run --rm --network host \
  -v "$PWD/prometheus.yml:/etc/prometheus/prometheus.yml:ro" \
  prom/prometheus:v2.55.0
```

Useful queries for a Grafana dashboard:

```promql
# p95 latency per route
histogram_quantile(0.95, sum by (le, http_route) (rate(http_server_request_duration_seconds_bucket[5m])))
# error ratio
sum(rate(http_server_request_duration_seconds_count{http_response_status_class="5xx"}[5m]))
  / sum(rate(http_server_request_duration_seconds_count[5m]))
# slow queries by operation
histogram_quantile(0.99, sum by (le, db_operation_name) (rate(db_client_operation_duration_seconds_bucket[5m])))
# dead jobs
sum by (forge_job_kind) (rate(forge_queue_job_duration_seconds_count{forge_job_outcome="dead"}[15m]))
```

The metrics listener is separate from the application listener by design:
the public router never serves `/metrics`. It is plain HTTP without
authentication and answers only `GET`/`HEAD /metrics`. Outside containers
bind it to `127.0.0.1`. In a container bind it to `0.0.0.0:9464` so
Prometheus can reach it **inside the cluster network**, confirm that with
`FORGE_TELEMETRY_METRICS_PUBLIC=true`, and do not publish the port to the
internet (no `ports:` mapping in Compose, no public Service or load-balancer
rule in Kubernetes). A non-loopback address without that confirmation stops
`production` at startup and logs a warning elsewhere.

## Propagation and sampling

By default incoming `traceparent` and `tracestate` headers are ignored and
every request starts a new trace: a client could otherwise pick the trace IDs
that appear in your logs, force recording of its requests past
`FORGE_TELEMETRY_SAMPLE_RATIO` (a sampled parent is always followed), and
push `tracestate` to your downstream services. Set
`FORGE_TELEMETRY_TRUST_TRACEPARENT=true` when the service sits behind a proxy,
gateway or mesh that sets or sanitizes these headers, or only receives
internal traffic. Trusted requests with a valid W3C `traceparent` continue
that trace and forward the validated `tracestate` unchanged. Parsing is
strict: lowercase hex, no all-zero identifiers, exact lengths, repeated
headers rejected (fuzz-tested).

New traces are sampled with probability `FORGE_TELEMETRY_SAMPLE_RATIO`
(OpenTelemetry's `TraceIdRatioBased` algorithm, so services sharing a ratio
agree); child spans follow their parent (`ParentBased`). Unsampled spans
still have IDs for log correlation and propagation but record nothing.

Outgoing requests propagate the trace when sent through the instrumented
transport:

```go
transport, err := app.Telemetry().Transport(nil) // nil: http.DefaultTransport
// handle err
client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
```

Use it only for services that may see your trace IDs.

## Instrumenting components

`forge.New` builds the telemetry from configuration, instruments the HTTP
server and shuts telemetry down last, after the other shutdown hooks, so
their spans are exported. Pass `app.Telemetry()` (nil when disabled; every
component accepts nil) to the rest:

```go
database, err := postgres.Open(ctx, app.Config().Database, postgres.WithTelemetry(app.Telemetry()))
// mysql.WithTelemetry and sqlite.WithTelemetry work the same way.
worker, err := queue.NewWorker(store, registry, queue.WorkerOptions{Telemetry: app.Telemetry()})
dispatcher, err := outboxpostgres.NewDispatcher(database.Pool(), handler, outboxpostgres.DispatcherOptions{Telemetry: app.Telemetry()})
scheduler := jobs.NewScheduler(jobs.SchedulerOptions{Telemetry: app.Telemetry()})
sender, err = mail.Instrument(sender, app.Telemetry())
```

Generated applications pass it to the database adapter and the scheduler.

Custom spans:

```go
ctx, span := app.Telemetry().Tracer("example.com/billing").Start(ctx, "charge card", telemetry.SpanKindInternal)
defer span.End()
if err := charge(ctx); err != nil {
	span.RecordError(err)
	return err
}
```

### Database

The `mysql` and `sqlite` adapters wrap their `database/sql` connector
(`sqldb.InstrumentConnector`), so statements in transactions and prepared
statements are covered; the wrapper forwards every optional driver interface
and keeps `database/sql`'s fallbacks. PostgreSQL uses pgx's query tracer;
batches (`SendBatch`) and `COPY` are not traced. Query spans end when the
driver returns (MySQL, SQLite) or when the rows are closed (PostgreSQL).
Migrations are not instrumented.

### Jobs: enqueue-to-run linkage

A job's processing span starts a new trace: job rows do not store the
enqueuing trace context, and adding it would change the queue schema of every
adapter. Both the `publish` span (inside the enqueuing request) and the
`process` span carry the job ID as `messaging.message.id`; search for it to
go from a request to the job attempts it caused. Handler work (database
queries, outgoing HTTP) nests under the processing span.

## Lifecycle and backpressure

Ended spans wait in a bounded in-memory queue (2048 spans). One goroutine,
owned by the `Telemetry` and stopped by its `Shutdown`, exports them; when
the queue is full new spans are dropped and counted
(`forge_telemetry_spans_dropped_total`) instead of slowing requests down. A
failed export drops its batch after the retries. On shutdown Forge flushes
spans and a final metric snapshot within the remaining
`FORGE_HTTP_SHUTDOWN_TIMEOUT`; whatever is still queued at the deadline is
dropped and counted.

## Cost

Measured with `go test -run='^$' -bench=BenchmarkHTTPTelemetry -benchmem .`
on the full Forge handler chain:

| Mode | Allocations per request (extra) |
| ---- | ------------------------------- |
| `FORGE_TELEMETRY_ENABLED=false` | 0: identical to a server built without telemetry code (enforced by `TestDisabledTelemetryAddsNoAllocations`) |
| Enabled, nothing exported (default) | about 18 (trace IDs, span, request logger with `trace_id`, route record) |
| Enabled, every request sampled and exported | about 20 plus the batched export |

Recording into an existing metric series does not allocate.

## Limitations

- Traces and metrics only; logs stay on standard output (collect them with
  your log shipper and join on `trace_id`).
- OTLP/HTTP with JSON encoding only; no gRPC and no protobuf encoding.
- Metric temporality is cumulative; exemplars and exponential histograms are
  not implemented.
- No baggage propagation.
