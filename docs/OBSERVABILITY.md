# Observability

Forge applications produce traces, metrics, runtime and connection pool metrics
and trace-correlated logs without
any third-party module: the `telemetry` package implements the part of
OpenTelemetry an application needs on the standard library and speaks the
OpenTelemetry wire protocol (OTLP/HTTP with JSON encoding), so any
OpenTelemetry Collector or compatible backend can receive the data
([ADR 0034](adr/0034-dependency-free-telemetry.md)).

Telemetry is **on by default and exports nothing by default**. Out of the box
every request has a trace ID that appears in its logs and is propagated to
downstream services; incoming trace headers are ignored unless you trust
them; metrics are kept in memory. You choose an export path:

| Goal | Setting |
| ---- | ------- |
| Prometheus scrapes the process | `FORGE_TELEMETRY_METRICS_ADDR=127.0.0.1:9464` (a separate listener serving `GET /metrics`) |
| Traces and metrics to an OpenTelemetry Collector | `FORGE_TELEMETRY_EXPORTER=otlp`, `FORGE_TELEMETRY_OTLP_ENDPOINT=http://collector:4318` |
| Logs to an OpenTelemetry Collector, with trace IDs | `FORGE_TELEMETRY_LOGS_EXPORTER=otlp` (and `FORGE_TELEMETRY_OTLP_ENDPOINT`) |
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

# 4. Logs too (OTLP/HTTP /v1/logs); stdout keeps the same lines
FORGE_TELEMETRY_LOGS_EXPORTER=otlp \
FORGE_TELEMETRY_OTLP_ENDPOINT=http://127.0.0.1:4318 \
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
| `FORGE_TELEMETRY_LOGS_EXPORTER` | `none` | `none` or `otlp`. Independent of `FORGE_TELEMETRY_EXPORTER`; `otlp` sends every log record to the collector as well as to standard output and uses the `FORGE_TELEMETRY_OTLP_*` settings below. |
| `FORGE_TELEMETRY_LOGS_LEVEL` | `info` | Lowest level sent to the log exporter: `debug`, `info`, `warn` or `error`. Standard output keeps the logger's own level. |
| `FORGE_TELEMETRY_OTLP_ENDPOINT` | (empty) | Collector base URL; Forge posts to `/v1/traces`, `/v1/metrics` and `/v1/logs`. Required for either `otlp` exporter. `production` requires `https` unless `FORGE_TELEMETRY_OTLP_ALLOW_PLAINTEXT=true`. No credentials, query or fragment. |
| `FORGE_TELEMETRY_OTLP_HEADERS` | (empty) | `name=value,name2=value2`, values percent-encoded (the `OTEL_EXPORTER_OTLP_HEADERS` format), e.g. `authorization=Bearer%20TOKEN`. Secret: redacted everywhere. |
| `FORGE_TELEMETRY_OTLP_TIMEOUT` | `10s` | Per export, retries included; at most `1m` |
| `FORGE_TELEMETRY_OTLP_COMPRESSION` | `none` | `none` or `gzip` |
| `FORGE_TELEMETRY_OTLP_ALLOW_PLAINTEXT` | `false` | Permit `http://` endpoints in `production` (sidecar or private network) |
| `FORGE_TELEMETRY_SAMPLE_RATIO` | `1` | Fraction of new traces sampled, `0`-`1` |
| `FORGE_TELEMETRY_TRUST_TRACEPARENT` | `false` | Continue traces from incoming `traceparent`/`tracestate` headers. Enable only behind a proxy or gateway that sets or sanitizes them. |
| `FORGE_TELEMETRY_METRICS_ADDR` | (empty: off) | `host:port` of the Prometheus listener; must differ from `FORGE_HTTP_ADDR`. Port `0` lets the kernel choose and never counts as the same address (both may be `127.0.0.1:0`, as in Forge-Rust); the `serving metrics` record carries the address actually bound. |
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
| `db.client.connection.count` (`db_client_connection_count`) | gauge | `db_system_name`, `db_client_connection_pool_name`, `db_client_connection_state` (`idle`, `used`) |
| `db.client.connection.max` (`db_client_connection_max`) | gauge | `db_system_name`, `db_client_connection_pool_name` |
| `forge.db.pool.waits` (`forge_db_pool_waits_total`) | counter | `db_system_name`, `db_client_connection_pool_name` |
| `forge.db.pool.wait.duration` (`forge_db_pool_wait_duration_seconds_total`) | counter (seconds) | `db_system_name`, `db_client_connection_pool_name` |
| `go.goroutine.count` (`go_goroutine_count`) | gauge | |
| `go.memory.heap.alloc` (`go_memory_heap_alloc_bytes`) | gauge | |
| `go.memory.heap.inuse` (`go_memory_heap_inuse_bytes`) | gauge | |
| `go.memory.total` (`go_memory_total_bytes`) | gauge | |
| `go.memory.gc.goal` (`go_memory_gc_goal_bytes`) | gauge | |
| `go.memory.allocated` (`go_memory_allocated_bytes_total`) | counter | |
| `go.gc.cycles` (`go_gc_cycles_total`) | counter | |
| `go.gc.pause.duration` (`go_gc_pause_duration_seconds`) | histogram | |
| `go.processor.limit` (`go_processor_limit`) | gauge | |
| `forge.telemetry.spans.dropped` (`forge_telemetry_spans_dropped_total`) | counter | |
| `forge.telemetry.logs.dropped` (`forge_telemetry_logs_dropped_total`) | counter | |
| `forge.telemetry.export.errors` (`forge_telemetry_export_errors_total`) | counter | `signal` |
| `forge.telemetry.metric.overflows` (`forge_telemetry_metric_overflows_total`) | counter | |

**Connection pools.** `postgres.Open`, `mysql.Open` and `sqlite.Open` with
`WithTelemetry(app.Telemetry())` export their pool: connections in use and idle
against the limit, how many acquisitions had to wait and for how long (a
`rate()` of the wait duration is the average number of callers blocked). The
pool name label is `default`; name each pool with `WithPoolName("replica")`
when an application opens more than one database of the same kind (a name used
twice replaces the earlier pool, and the wait counters continue from its
totals). Closing the database (`Close` or `Shutdown`) stops reading the pool
and sets its connection counts and its `db.client.connection.max` to zero; a
collection that was reading the pool when it closed does not write them back.
`sqldb.Observer.ObservePool` called again for the same database moves it to the
new name (the earlier series drop to zero) and refuses a closed database. With `database/sql` (MySQL, SQLite) a
wait means every connection up to the limit was in use; with pgx (PostgreSQL) it
also counts an acquisition that waited for a new connection to be established.
The statistics are read when the metrics are collected (a scrape or an OTLP
push), never per query.

**Go runtime.** Always recorded, read with `runtime/metrics` when the metrics
are collected (no stop-the-world). `go.memory.heap.alloc` is the bytes of
allocated heap objects (`MemStats.HeapAlloc`), `go.memory.heap.inuse` the bytes
in in-use heap spans (`HeapInuse`), `go.memory.total` everything the runtime has
mapped, `go.memory.gc.goal` the heap size that triggers the next collection.
`go.gc.pause.duration` histograms the stop-the-world pauses in buckets from
10 microseconds to 1 second, re-bucketed from the runtime's finer histogram: a
pause counts in the bucket that holds the upper edge of its runtime bucket, so a
pause just below a bound can be counted one bucket high, and the sum is
estimated from bucket midpoints. A `go.goroutine.count` that only grows
is a goroutine leak.

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

With `FORGE_TELEMETRY_LOGS_EXPORTER=otlp` the application logger writes every
record to standard output **and** queues it for export to the collector's
`/v1/logs`, over the same OTLP/HTTP JSON path as traces and metrics (endpoint,
headers, timeout, gzip and the production https rule are the
`FORGE_TELEMETRY_OTLP_*` settings). Each OTLP log record has:

| Field | Source |
| ----- | ------ |
| body | the message, cut at 4096 bytes |
| severity number and text | the slog level: Debug 5 `DEBUG`, Info 9 `INFO`, Warn 13 `WARN`, Error 17 `ERROR`; levels in between keep their order inside the range, below Debug is `TRACE` (1-4), above Error+3 is `FATAL` (21-24) |
| attributes | the record's attributes and those added with `With`, groups as dotted prefixes (`http.status`), at most 64 (the rest counted in `droppedAttributesCount`), string values cut at 1024 bytes |
| `traceId`, `spanId`, `flags` | the span of the context passed to `InfoContext`, or the `trace_id`/`span_id` attributes the request logger carries; they are fields of the record, not repeated as attributes |
| resource | `service.name`, `service.version`, `deployment.environment.name`, as for traces and metrics |
| time, observed time | the record's time; when Forge queued it |

Values are resolved through `slog.LogValuer` before export, so a
`config.Secret` is exported as `[REDACTED]`, exactly as on standard output.
Other values (structs, pointers, maps, errors) are encoded the way the
standard-output handler encodes them: like `slog.JSONHandler` in production
(an error as its message, anything else through `encoding/json`, so
`json:"-"` and unexported fields stay out and `MarshalJSON`/`MarshalText`
redactions apply; a `*url.URL` exports without its password), like
`slog.TextHandler` in `development` and `test`. A value that fails to encode
exports a placeholder naming its type. The exporter therefore never sends more
than standard output shows: keep personal data and secrets out of log calls as
you would for standard output.

**Redaction with `ReplaceAttr`.** slog handlers do not reveal their options, so
a `slog.HandlerOptions.ReplaceAttr` that redacts or removes attributes on
standard output reaches the exporter only when you hand it over; otherwise the
collector receives the values standard output hides. Build the standard-output
handler with `telemetry.NewOutputHandler`, which reports its format and its
`ReplaceAttr` to `forge.New` (through `telemetry.FromConfig`):

```go
redact := func(groups []string, a slog.Attr) slog.Attr {
	if a.Key == "password" {
		return slog.String(a.Key, "[REDACTED]")
	}
	return a
}
logger := slog.New(telemetry.NewOutputHandler(os.Stdout, telemetry.LogFormatJSON,
	&slog.HandlerOptions{ReplaceAttr: redact}))
app, err := forge.New(forge.WithLogger(logger))
```

When you build the telemetry yourself (`telemetry.New`, or
`forge.WithTelemetry`), set `Options.LogReplaceAttr` to the same function and
`Options.LogFormat` to the handler's format. A custom handler that wraps
`slog.TextHandler` or carries a `ReplaceAttr` must implement
`telemetry.LogOutputHandler` (`LogOutput() telemetry.LogOutput`), or the
exporter encodes values as `slog.JSONHandler` does and applies no
`ReplaceAttr`; `telemetry.NewLogHandler` reports what the handler it wraps
reports. The exporter applies `ReplaceAttr` as slog does: to the message (key
`msg`, no groups) and to every attribute with its open groups, after resolving
`slog.LogValuer` and again on the result, never to a group itself; an empty
result removes the attribute, and a group left empty is left out. The record's
time and level are exported as logged. One deliberate difference: Go 1.27's
handlers leave a group that `ReplaceAttr` emptied open, so the attributes after
it are written inside it; the exporter keeps them under their own keys.

Backpressure and lifecycle: records wait in a bounded queue (2048 records);
one goroutine owned by the `Telemetry` exports batches of up to 512 at least
every second. Logging never blocks: when the queue is full the record is
dropped and counted in `forge_telemetry_logs_dropped_total`, and standard
output still has it. A failed export (after the OTLP retries, within
`FORGE_TELEMETRY_OTLP_TIMEOUT`) drops its batch, counts it in the same metric
and in `forge_telemetry_export_errors_total{signal="logs"}`, and is reported
**on standard output only**: export failures never enter the log queue, so a
broken collector cannot feed itself. On shutdown the queue is flushed within the
remaining `FORGE_HTTP_SHUTDOWN_TIMEOUT`; lines logged after telemetry shut down
(the final "application stopped") reach standard output only. Output that does
not pass through the logger (a crash's stack trace, `fmt.Println`) is not
exported.

When you build the application yourself instead of using `forge.Main`, give the
telemetry the plain logger and tee your own:

```go
tel, err := telemetry.FromConfig(cfg, plainLogger) // plainLogger writes to stdout only
logger := slog.New(telemetry.NewTeeHandler(plainLogger.Handler(), tel.LogHandler()))
```

`forge.New` does this for you when the log exporter comes from the
configuration (`app.Logger()` is then the tee). With `forge.WithTelemetry(t)` the
wiring is yours: `t.LogHandler()` is nil unless `t` was built with a log
exporter.

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
    logs:
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

The exporter sends batches of up to 512 spans at least every 5 seconds, log
records in batches of up to 512 at least every second, and metrics every `FORGE_TELEMETRY_METRICS_INTERVAL`. It retries network errors
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

## Local stack: Grafana, Prometheus, Tempo and Loki

`forge new` generates `compose.observability.yaml` and an `observability/`
directory that run an OpenTelemetry Collector, Prometheus, Tempo, Loki and
Grafana next to the application, already wired
([ADR 0038](adr/0038-local-observability-stack.md)). It is opt-in: the default
`docker compose up` does not start it.

```bash
export GRAFANA_ADMIN_PASSWORD="$(openssl rand -base64 18)"   # required; or put it in .env
docker compose -f compose.yaml -f compose.observability.yaml up --build
curl http://127.0.0.1:8080/v1/status
# Grafana: http://127.0.0.1:3000 (user admin) opens the "Forge application" dashboard
# Prometheus: http://127.0.0.1:9090
# compose interpolates the whole file, so GRAFANA_ADMIN_PASSWORD must be set for down too
docker compose -f compose.yaml -f compose.observability.yaml down      # add -v to delete the data
```

| Signal | Path | Where to look in Grafana |
| ------ | ---- | ------------------------ |
| Traces | app, OTLP to the collector, then Tempo | Explore, Tempo; the dashboard's "Recent traces" |
| Metrics | app, OTLP to the collector, then Prometheus's OTLP receiver | The dashboard; Explore, Prometheus |
| Logs | app, OTLP to the collector (`FORGE_TELEMETRY_LOGS_EXPORTER=otlp`), then Loki's OTLP endpoint | The dashboard's "Application logs"; Explore, Loki |

Traces and logs are linked both ways: a span's "Logs for this span" opens Loki
filtered by its trace ID, and the `trace_id` of a log record is a link to the
trace. Log records carry `trace_id` when they are written with a traced context
(`logger.InfoContext(ctx, ...)`, `web.Logger(ctx)`); the framework itself logs
startup and errors, not every request. Loki receives the records on its OTLP
endpoint: the message is the line, `service.name` is the `service_name` label,
and `trace_id`, `span_id`, the severity (`detected_level`) and the attributes of
the log call are structured metadata, so filter with
`{service_name="shop"} | trace_id="..."` or `| status >= 500`. The collector
does no parsing. Output that bypasses the logger is only in `docker compose
logs`.

Every published port binds `127.0.0.1` (Grafana `GRAFANA_PORT`, Prometheus
`PROMETHEUS_PORT`, the collector's OTLP 4318), Grafana has
no default password and no anonymous access, nothing is sent off the machine,
and images are pinned by version and digest. The application's own variables
stay the ones above; the overlay only sets `FORGE_TELEMETRY_EXPORTER=otlp`,
`FORGE_TELEMETRY_OTLP_ENDPOINT`, `FORGE_TELEMETRY_SERVICE_NAME`,
`FORGE_TELEMETRY_LOGS_EXPORTER=otlp` and a 15 s `FORGE_TELEMETRY_METRICS_INTERVAL`
on the `app` service. An application started
with `forge dev` can export to the running collector
(`FORGE_TELEMETRY_EXPORTER=otlp FORGE_TELEMETRY_LOGS_EXPORTER=otlp FORGE_TELEMETRY_OTLP_ENDPOINT=http://127.0.0.1:4318`),
logs included. The dashboard also has panels for the database connection pool
(connections by state, utilization, waits, time waiting) and the Go runtime
(goroutines, heap, allocation rate, GC cycles and pause) and for dropped log
records.

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
spans, log records and a final metric snapshot, concurrently, within the remaining
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

- Logs are exported only from the application logger; other standard output
  (crash traces, `fmt.Println`) needs your log shipper, which can join on
  `trace_id` only for lines that carry it.
- OTLP/HTTP with JSON encoding only; no gRPC and no protobuf encoding.
- Metric temporality is cumulative; exemplars and exponential histograms are
  not implemented.
- No baggage propagation.
