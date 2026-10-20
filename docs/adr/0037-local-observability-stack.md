# ADR 0037: Preconfigured local observability stack (Grafana, Prometheus, Tempo, Loki)

- **Status:** Accepted
- **Date:** 2026-10-20

This decision is language-neutral: Forge-Go and Forge-Rust generate the same
files, with the same names and commands, and differ only in what the
application binary exports (see [Forge-Rust port](#forge-rust-port)).

## Context

Rails gets dashboards by assembling gems and services; Forge applications
already export traces, metrics and trace-correlated logs
([ADR 0034](0034-dependency-free-telemetry.md)) but leave the receiving side to
each team. The owner asked for Prometheus and Grafana preconfigured, with Tempo
(traces) and Loki (logs) as well, so a generated application can be observed in
Grafana, with logs and traces linked, after one command.

Facts that shaped the design, each checked against the real binaries
(OpenTelemetry Collector, Prometheus, Tempo, Loki and Grafana were run
natively with the shipped configuration and the generated application):

- The application exports traces and metrics over OTLP/HTTP+JSON (any collector
  accepts it) and writes logs to standard output. It has no OTLP log exporter,
  and the framework logs no per-request line: logs are startup lines, errors
  and whatever the application writes, `trace_id` and `span_id` included when
  the logging context is traced.
- Docker Compose interpolates the whole file, so a `${VAR:?}` (a required
  variable) in a service behind an inactive profile still fails
  `docker compose up`, and a profile cannot change another service's
  environment or log driver. An opt-in stack that must set variables on `app`
  and refuse a missing Grafana password therefore cannot be a profile inside
  `compose.yaml` without changing the default stack.
- Tempo 3 requires a Kafka-compatible queue even in one process; Tempo 2.x
  still runs as a single binary on local disk.
- Prometheus 3 has an OTLP receiver, so metrics need no scrape target and no
  open metrics port on the application.

## Decision

`forge new` generates, for every application and every database:

```
compose.observability.yaml                      overlay for compose.yaml
observability/otel-collector.yaml
observability/prometheus.yml
observability/tempo.yaml
observability/loki.yaml
observability/grafana/provisioning/datasources/datasources.yaml
observability/grafana/provisioning/dashboards/dashboards.yaml
observability/grafana/dashboards/forge-application.json
```

### Start and stop

```sh
export GRAFANA_ADMIN_PASSWORD="$(openssl rand -base64 18)"   # or in .env
docker compose -f compose.yaml -f compose.observability.yaml up --build
# Grafana http://127.0.0.1:3000 (user admin), Prometheus http://127.0.0.1:9090
docker compose -f compose.yaml -f compose.observability.yaml down       # -v deletes the data
```

`docker compose up` is unchanged: the default file mentions nothing of the
stack (a test enforces this). The overlay is used instead of a profile for the
reason in the Context; it is an overlay file named `compose.observability.yaml`,
and the stack is called "observability" everywhere.

### Data flow

```
traces, metrics   app --OTLP/HTTP--> otel-collector --> Tempo (traces), Prometheus (OTLP receiver)
logs              app stdout --> Docker fluentd log driver --> otel-collector --> Loki (OTLP, /otlp)
Grafana           Prometheus, Tempo and Loki data sources, provisioned
```

- The overlay sets, on `app`: `FORGE_TELEMETRY_EXPORTER=otlp`,
  `FORGE_TELEMETRY_OTLP_ENDPOINT=http://otel-collector:4318`,
  `FORGE_TELEMETRY_SERVICE_NAME=<application name>` and a 15 s metric interval,
  all existing settings (no new configuration mechanism), and tests load them
  through the framework's own configuration validator.
- **Logs** use Docker's built-in `fluentd` logging driver on `app` with
  `fluentd-async` (the application starts even when the collector is down) and
  `tag` set to the application name. The collector's `fluent_forward` receiver
  makes the line the log body and the tag the `service.name` resource
  attribute, so logs share `service.name` with the application's traces and
  metrics. A `transform` processor extracts `level` (severity) and `trace_id`
  and `span_id` (the record's trace context) from the line, in logfmt or JSON,
  and Loki receives the record on its native OTLP endpoint (`service_name`
  label; `trace_id`, `span_id`, `detected_level` as structured metadata). The
  line is kept whole. `docker compose logs` still works (Docker's dual logging).
  Alternatives rejected: a collector reading `/var/lib/docker/containers`
  (root, Linux paths, and it would read every container on the host); a log
  shipper with the Docker socket (root-equivalent); Loki's Docker driver plugin
  (a separate plugin install that cannot be part of a compose file); and an
  OTLP log exporter in the framework (a new public API, queue and privacy
  surface for what a development stack needs; it remains the better long-term
  path and is tracked as a follow-up).
- **Metrics**: OTLP into Prometheus's receiver. Names translate to the
  Prometheus names documented in OBSERVABILITY.md
  (`http_server_request_duration_seconds_*`...), `service.name` becomes the
  `job` label. No port on the application is opened or scraped.
- **Traces**: the collector forwards to Tempo (2.x, single binary, local disk).
- **Correlation**: Tempo's data source has trace-to-logs (Loki filtered by
  `service_name` and the trace ID) and trace-to-metrics (request rate and p95
  of the span's service); Loki's has a derived field that turns a `trace_id` in
  a line (`trace_id=...` or `"trace_id":"..."`) into a link to the trace.
  Exemplars are not available: the framework does not export them.
- **Dashboard** `forge-application` (the Grafana home page): request rate,
  5xx ratio, latency percentiles, status classes, per-route rate and latency,
  active requests, database operations, latency and errors, queue, scheduler
  and outbox activity, telemetry health, log volume by level, logs, and a
  recent-traces table, filtered by a `service` variable. The framework exports
  no Go runtime or connection-pool metrics, so there are no panels for them;
  adding the metrics, and the panels, is a follow-up.

### Security defaults

- Every published port binds `127.0.0.1`: Grafana (`GRAFANA_PORT`, default
  3000), Prometheus (`PROMETHEUS_PORT`, 9090), the collector's OTLP (4318, so an
  application started with `forge dev` can export) and fluent (24224, where the
  Docker daemon delivers logs). Tempo and Loki publish nothing.
- Grafana's admin password has no default: it comes from
  `GRAFANA_ADMIN_PASSWORD`, required with `${...:?}`, so the stack does not
  start with a guessable credential and no password is generated or stored in
  a file. Anonymous access and sign-up are off. Grafana applies the variable
  when it creates its database; a later change needs the user settings or a
  `down -v`.
- Nothing leaves the machine: Grafana's usage reporting, update checks, news
  feed, plugin preinstall, Gravatar and external snapshots are disabled; Tempo
  and Loki's usage reporting is off; the collector's own metrics are off.
- Images are pinned by exact version **and** digest, and Dependabot's
  `docker-compose` ecosystem is enabled in the generated repository to move
  them. Versions are the ones run during development of this decision:
  `otel/opentelemetry-collector-contrib:0.163.1`, `prom/prometheus:v3.15.0`,
  `grafana/tempo:2.10.8`, `grafana/loki:3.6.18`, `grafana/grafana:13.2.4`.
  The digests are the multi-architecture index digests published for those
  tags.
- Configuration is mounted read-only and holds no secrets. The generated files
  are world-readable (0644, directories 0755) because the containers run as
  other users. Data lives in named volumes with seven-day retention; the stack
  is for development, not production (no replication, no object storage, no
  authentication between components inside the compose network).
- Observability data is as sensitive as the logs and spans the application
  produces; ADR 0034's rules about what the framework records apply
  unchanged, and a local process can write to the loopback ports (see the
  threat model).

## Consequences

- One documented command gives metrics, traces and logs in Grafana with
  trace-to-log navigation, with no framework dependency added.
- Logs reach Loki only for `app` under this overlay (and only text or JSON
  slog lines are parsed for severity and trace context); the `migrate`
  one-off keeps the default driver, and an application run with `forge dev`
  exports traces and metrics but keeps its logs in the terminal.
- The fluentd driver is built into Docker Engine; Podman and some Docker
  setups may lack it. The overlay then still starts but logs do not arrive.
- Pinned versions age: a stack left untouched drifts, which Dependabot makes
  visible. Tempo stays on 2.x until its single-binary story no longer needs a
  Kafka-compatible queue.
- Maintaining the dashboard and the collector's log parsing is part of the
  framework now, and they depend on the metric names in OBSERVABILITY.md; a
  test checks that every metric the dashboard queries is documented there.

## Forge-Rust port

Forge-Rust follows this ADR without change: the same file names under
`observability/` and `compose.observability.yaml`, the same service names
(`otel-collector`, `prometheus`, `tempo`, `loki`, `grafana`), ports, image
pins, variables (`GRAFANA_ADMIN_PASSWORD`, `GRAFANA_PORT`, `PROMETHEUS_PORT`),
commands and README section ("Observability stack"). The static files can be
copied verbatim. What differs is the application side: the overlay's `app`
environment must use Forge-Rust's own telemetry variables, the service name
must match the Docker tag, and the dashboard queries must use the metric names
and labels Forge-Rust exports, which should be the same OpenTelemetry names
and units so Prometheus translates them identically. If Forge-Rust has an OTLP
log exporter, prefer it over the fluentd driver and drop the log-driver block
and the `fluent_forward` receiver; the Loki side is unchanged.
