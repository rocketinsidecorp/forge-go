# Benchmarks

Forge-Go has two kinds of benchmarks, and they answer different questions:

- **Micro-benchmarks** (`go test -bench`) time one hot path in isolation:
  nanoseconds and allocations per operation.
- **The end-to-end benchmark** (`scripts/bench.sh`) builds a generated
  application, runs it on PostgreSQL and measures requests per second and
  latency percentiles of five requests through the whole stack.

Forge-Rust will get a suite with the **same scenario names, variants and output
format** ([Rails parity](RAILS_PARITY.md)), so the two can be read side by side.
The catalog below is language-neutral; only the "Go benchmark" column is Go.

Contents: [Run them](#run-them) · [Scenarios](#scenarios) ·
[Methodology](#methodology) · [Caveats](#caveats) · [Results](#results) ·
[Output format](#output-format-shared-with-forge-rust)

## Run them

```sh
# every micro-benchmark, three times, one package at a time (-p 1: test
# binaries of different packages would otherwise compete for the CPU)
go test -p 1 -run '^$' -bench . -benchmem -count 3 ./...

# one scenario, one package
go test -run '^$' -bench RouterMatch -benchmem -count 5 ./router
go test -run '^$' -bench 'MiddlewareChain|HTTPTelemetry' -benchmem .

# end to end: needs Go, psql, openssl and a PostgreSQL server whose user may
# create roles and databases. The URL is psql's argument, which ps shows: on a
# shared machine leave the password out of it and use PGPASSWORD or ~/.pgpass.
export FORGE_TEST_POSTGRES_URL='postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable'
scripts/bench.sh                                  # 16 workers, 10 s per scenario, 3 runs
BENCH_DURATION=3s BENCH_RUNS=1 scripts/bench.sh   # a quick check
BENCH_CONCURRENCY=64 scripts/bench.sh             # another concurrency
scripts/bench.sh -format json                     # arguments after the script go to the load generator

# the load generator alone, against an application you already run
go run ./internal/loadgen -h
```

`scripts/bench.sh` builds the `forge` CLI and the load generator, runs
`forge new --forge-path --api-auth` against your checkout (loadgen signs in
through the bearer `POST /auth/login`) and
`forge generate scaffold post title:string body:text --tenant-scoped`, pastes
the wiring the generator prints into `bootstrap.go`, creates a non-superuser
role and a database of its own on the server (row-level security applies, as in
production), runs `migrate` and `setup`, starts the binary, measures it, and
then stops it, drops the role and the database and deletes everything it built.
It needs about 100 MB of temporary files (in `$TMPDIR`) while it runs. The settings are environment
variables: `BENCH_CONCURRENCY` (16), `BENCH_DURATION` (10s), `BENCH_WARMUP` (2s),
`BENCH_RUNS` (3), `BENCH_ROWS` (50) and `BENCH_PORT` (18080).

CI runs `go test -run '^$' -bench . -benchtime=1x ./...` in a non-gating job
(`benchmarks`), so every benchmark keeps compiling and running. It does not
compare or store numbers: shared runners are too noisy for that.

## Scenarios

### Micro-benchmarks

| Scenario | Variants | What is timed | Go benchmark |
| --- | --- | --- | --- |
| `router_match` | `static`, `param`, `wildcard` x 1, 10, 100, 1000 routes | `Router.ServeHTTP` for a request matching the middle route of the table, to a handler that does nothing; `static` is `/static/rN/item`, `param` `/param/rN/{id}`, `wildcard` `/files/rN/{path...}` | `BenchmarkRouterMatch/<kind>/<routes>` (`router`) |
| `middleware_chain` | `passthrough`, `context` x 0, 1, 5, 10 middleware | the whole Forge handler chain (security headers, request ID, recovery, body limit, router) for one GET, with N middleware added by `App.Use`; `context` ones also store a value in the request context | `BenchmarkMiddlewareChain/<kind>/<n>` (root) |
| `json_render` | `object`, `list_50` | `web.JSON` of one record (a scaffold's API show) and of a page of 50 (its index) | `BenchmarkJSONRender/<variant>` (`web`) |
| `html_render` | `layout_partial`, `layout_partial_csrf` | a page of 50 rows in a layout with partials, auto-escaping text that contains markup; the second variant is the page a generated app serves, with the CSRF meta tags, a form token and flash messages, through the csrf and flash middleware | `BenchmarkHTMLRender/<variant>` (`views`) |
| `csrf_verify` | `match`, `header_post`, `form_post` | constant-time comparison of two tokens; a POST through the CSRF middleware with the token in a header; the same with the token in a parsed form body | `BenchmarkCSRFVerify/<variant>` (`csrf`) |
| `session_cookie_verify` | none | `AuthenticateCookie` for a valid session cookie: cookie lookup, token decode, SHA-256 digest, session check, principal and tenant context; the session store is an in-memory function | `BenchmarkSessionCookieVerify` (`auth`) |
| `password_verify` | none | Argon2id, m=19456 KiB, t=2, p=1 (Forge-Rust's parameters), one correct password | `BenchmarkPasswordVerify` (`auth`) |
| `uuid_v7` | `new`, `string` | generating an identifier; formatting it | `BenchmarkUUIDv7/<variant>` (`uuid`) |
| `i18n_lookup` | `simple`, `interpolation`, `plural_one`, `plural_other`, `fallback` | `Translator.Translate`: a plain key; one `%{name}` argument; the `one` and `other` plural forms; a key answered by the parent locale (`pt-BR` to `pt`) | `BenchmarkI18nLookup/<variant>` (`i18n`) |
| `attribute_encryption` | `seal`, `open`, `value_seal`, `value_open` | AES-GCM of one 64-byte attribute bound to table, column and record, with `Cipher` and with the `Value` column type models use | `BenchmarkAttributeEncryption/<variant>` (`encryption`) |
| `rate_limit_decision` | `allowed`, `denied`, `distinct_keys` | one `Memory.Allow`; `distinct_keys` inserts a key and evicts another on every call (the worst case) | `BenchmarkRateLimitDecision/<variant>` (`ratelimit`) |

Notes on the choices:

- `password_verify` is **deliberately slow**: the work factor is the defense. It
  is reported so that nobody mistakes its cost for a regression and nobody
  "optimizes" it. Every other figure is smaller by four orders of magnitude.
- Sessions are opaque server-side tokens, not signed or encrypted cookies, so
  there is no cookie signature to verify; `session_cookie_verify` times what the
  middleware does in process. The database lookup of a real session store is in
  the end-to-end scenarios (`index_html`, `show_json`, `create_form`).
- `rate_limit_decision` times the in-process limiter. The PostgreSQL, MySQL and
  SQLite limiters cost one database round trip, which the micro-benchmark would
  only measure as the driver's.
- Two benchmarks that existed before this suite are part of it:
  `BenchmarkHTTPTelemetry` (root; the cost of telemetry, see
  [OBSERVABILITY.md](OBSERVABILITY.md#cost)), and the single static route of the
  former `BenchmarkRouterStatic`, now `router_match/static/1`.
- `middleware_chain` uses `forge.New()` with its defaults, so the chain includes
  telemetry in its default mode (enabled, nothing exported); the 0-middleware
  row is therefore the framework's base cost per request.
- Request and response objects are built once, outside the timed loop, and
  responses go to a writer that discards the body, so the figures are the
  framework's work and not `httptest`'s.

### End-to-end scenarios

Each request is sent over a keep-alive connection by a closed loop of a fixed
number of workers. The sign-in happens once, before measuring.

| Scenario | Request | Success |
| --- | --- | --- |
| `health` | `GET /health` (readiness: includes a database ping) | `200` |
| `welcome` | `GET /` (the generated welcome page, no database) | `200` |
| `index_html` | `GET /posts?limit=50` with the session cookie: a scaffold index of 50 rows from PostgreSQL, rendered with layout, partials, i18n and CSRF tags | `200` |
| `show_json` | `GET /v1/posts/{id}` with a bearer token: one row as JSON | `200` |
| `create_form` | `POST /posts` with the session cookie and the CSRF token in the form: validates, inserts one row, redirects | `303` |

The application is a tenant-scoped scaffold, so every request that touches the
table runs the permission check, the tenant context and PostgreSQL row-level
security. A response with another status counts as an error and its latency is
not recorded.

## Methodology

**What is captured.** `scripts/bench.sh` prints, before the table: the Forge-Go
commit (`+dirty` if the tree has changes), the Go version, the kernel, the
number of CPUs and the CPU model, the PostgreSQL version and the UTC date. Add
the host description by hand (container, virtual machine, laptop). Micro-benchmark
output carries `goos`, `goarch` and `cpu` lines.

**Warm-up.** Micro-benchmarks: `go test` grows the iteration count until a run
takes about a second, which is its warm-up. End to end: every scenario is first
driven for `BENCH_WARMUP` (2 s), unmeasured, so connections, pools, caches and
the template pool are warm.

**Runs.** Micro-benchmarks run `-count 3` and the tables report the **median**
of the three. End to end, each scenario is measured for `BENCH_DURATION`
(10 s) in each of `BENCH_RUNS` (3) rounds; the rounds interleave the
scenarios (health, welcome, index, show, create, health, ...) so slow drift of
the host affects all of them alike. The table reports the median of each figure
over the runs, and the minimum and maximum requests per second so that the
spread is visible.

**Concurrency.** Fixed: 16 workers, one connection each, a closed loop (a worker
sends its next request when the previous one is answered). Latency is measured
by the client from sending the request to reading the whole response.
Percentiles are nearest-rank over every successful request of the run.

**What is measured.** Micro-benchmarks: the Go function and its allocations.
End to end: the compiled application binary (`FORGE_ENV=development`, plain
HTTP, JSON request logger as generated), the HTTP server, the whole handler
chain, the generated controllers, views, repositories, the `pgx` pool and
PostgreSQL itself.

**What is not measured.** TLS and anything a reverse proxy does; a network
(loopback only); sign-in and password hashing (done once, before the clock
starts; see `password_verify`); the observability stack (telemetry is on in its
default mode, nothing is exported); other databases (PostgreSQL only); mail,
jobs, storage and live streams; start-up and memory use; cold caches.

## Caveats

- **One machine.** The load generator, the application and PostgreSQL share the
  same CPUs and the same kernel, so each takes CPU from the others. The
  figures are **throughput of the whole rig**, not of the application alone,
  and the latency includes queueing for CPU.
- **Closed loop.** With a fixed number of workers, throughput and latency are
  tied (requests per second is about 16 divided by the mean latency), and a
  stall delays the requests that would have been sent during it instead of
  being counted against them (coordinated omission). Read the percentiles as
  "latency at this concurrency", not as an open-loop service-level figure.
- **Shared and noisy hosts.** Other work on the machine changes the numbers from
  run to run; see the min-max column and repeat before drawing a conclusion
  from a difference of less than about 10 percent. Compare runs from the same
  host only, and never numbers from different hardware.
- **Loopback and no TLS** make every request cheaper than it is in production.
- **Development settings.** The binary runs with `FORGE_ENV=development`
  because production requires TLS or a trusted proxy.
- **`create_form` grows the table** by one row per request (tens of thousands
  per run); `index_html` always lists the newest 50, so its work does not grow.
  PostgreSQL runs with its default settings.
- **`health` is not "no work"**: it pings the database.
- Router times grow only slightly with the number of routes because
  `net/http`'s `ServeMux` matches through a tree; the wildcard variants also
  pay for path values.
- Micro-benchmark figures are for a discarding writer and a prebuilt request,
  so they are lower bounds of what a server spends on the same work.

## Results

Indicative only: see the [caveats](#caveats). Do not compare them with figures from
another machine or another framework.

Measured on 2026-10-20, in the container this was developed in: **a container
on a shared host; indicative only** (other work ran on the host during both
measurements; its load average was between 1.2 and 2.4 before they started).

| | |
| --- | --- |
| Date | 2026-10-20 |
| CPU | 4 x Intel Xeon Processor @ 2.10 GHz (virtual CPUs; `model name` as the guest reports it) |
| Kernel | Linux 6.18.44 (container) |
| Go | go1.27.1 linux/amd64 |
| PostgreSQL | 18.6 (Ubuntu 18.6-1.pgdg24.04+2), default settings, on the same host at 127.0.0.1 |
| Forge-Go | 52898cb with the benchmark suite of this change in the working tree (`scripts/bench.sh` printed `+dirty`) |

### Micro-benchmarks

`go test -p 1 -run '^$' -bench . -benchmem -count 3 ./...`; the median of three
runs. `password_verify` is deliberately slow (about 113 ms, PBKDF2 with 600000
iterations; this run predates Argon2id, [ADR 0048](adr/0048-argon2id-password-hashing.md),
whose verification measured about 35 ms and 19 MiB on a 4-CPU development
container).

| Scenario | Variant | ns/op | B/op | allocs/op |
| --- | --- | ---: | ---: | ---: |
| `router_match` | `static/1` | 455 | 64 | 1 |
| `router_match` | `static/10` | 484 | 64 | 1 |
| `router_match` | `static/100` | 492 | 64 | 1 |
| `router_match` | `static/1000` | 504 | 64 | 1 |
| `router_match` | `param/1` | 509 | 96 | 3 |
| `router_match` | `param/10` | 535 | 96 | 3 |
| `router_match` | `param/100` | 539 | 96 | 3 |
| `router_match` | `param/1000` | 583 | 96 | 3 |
| `router_match` | `wildcard/1` | 1,328 | 288 | 11 |
| `router_match` | `wildcard/10` | 1,293 | 288 | 11 |
| `router_match` | `wildcard/100` | 1,323 | 288 | 11 |
| `router_match` | `wildcard/1000` | 1,372 | 320 | 11 |
| `middleware_chain` | `passthrough/0` | 5,156 | 3289 | 47 |
| `middleware_chain` | `passthrough/1` | 5,363 | 3289 | 47 |
| `middleware_chain` | `passthrough/5` | 5,232 | 3289 | 47 |
| `middleware_chain` | `passthrough/10` | 5,949 | 3289 | 47 |
| `middleware_chain` | `context/0` | 5,182 | 3289 | 47 |
| `middleware_chain` | `context/1` | 4,965 | 3657 | 49 |
| `middleware_chain` | `context/5` | 6,374 | 5130 | 57 |
| `middleware_chain` | `context/10` | 7,703 | 6971 | 67 |
| `json_render` | `object` | 1,297 | 224 | 4 |
| `json_render` | `list_50` | 39,289 | 4870 | 102 |
| `html_render` | `layout_partial` | 394,347 | 102503 | 2320 |
| `html_render` | `layout_partial_csrf` | 465,586 | 96398 | 2355 |
| `csrf_verify` | `match` | 298 | 256 | 6 |
| `csrf_verify` | `header_post` | 1,263 | 1000 | 15 |
| `csrf_verify` | `form_post` | 3,383 | 2336 | 30 |
| `session_cookie_verify` | `-` | 888 | 824 | 10 |
| `password_verify` | `-` | 113.1 ms | 1080 | 18 |
| `uuid_v7` | `new` | 90 | 0 | 0 |
| `uuid_v7` | `string` | 48 | 48 | 1 |
| `i18n_lookup` | `simple` | 534 | 16 | 1 |
| `i18n_lookup` | `interpolation` | 748 | 371 | 5 |
| `i18n_lookup` | `plural_one` | 916 | 384 | 5 |
| `i18n_lookup` | `plural_other` | 986 | 394 | 6 |
| `i18n_lookup` | `fallback` | 660 | 136 | 5 |
| `attribute_encryption` | `seal` | 648 | 608 | 6 |
| `attribute_encryption` | `open` | 542 | 256 | 3 |
| `attribute_encryption` | `value_seal` | 760 | 688 | 8 |
| `attribute_encryption` | `value_open` | 719 | 352 | 6 |
| `rate_limit_decision` | `allowed` | 115 | 0 | 0 |
| `rate_limit_decision` | `denied` | 108 | 0 | 0 |
| `rate_limit_decision` | `distinct_keys` | 494 | 64 | 1 |
| `http_telemetry` | `baseline` | 2,813 | 1840 | 29 |
| `http_telemetry` | `disabled` | 2,872 | 1840 | 29 |
| `http_telemetry` | `enabled_no_export` | 6,072 | 3289 | 47 |
| `http_telemetry` | `enabled_exporting` | 7,614 | 3949 | 49 |

The `router_match` and `middleware_chain` rows above were measured before the
router matched each request once. Since then `Router.ServeHTTP` allocates
nothing for a static route, once for a route with path values (16 B), and 5
times instead of 11 for `wildcard`; on a loaded 4-CPU machine the medians of
six runs fell from about 450-500 ns to 310-350 ns (`static`, `param`) and from
about 1,280 ns to 910-1,080 ns (`wildcard`). Re-measure on a quiet machine
before quoting the times; `TestMatchedRequestMatchesOnce` pins the allocations.

How to read them: `middleware_chain/*/0` is the whole chain without any
`App.Use` middleware, and it already includes telemetry in its default mode
(enabled, nothing exported: 47 allocations, the same as
`http_telemetry/enabled_no_export`; `http_telemetry/disabled` is the chain
without it, 29). Ten middleware that only call the next handler add about
0.8 us in total, which is within the noise of this host (the three runs of
`passthrough/10` span 5.9 to 7.1 us); ten that add a context value add about
2.5 us, 3.7 KB and 20 allocations (about 250 ns and 2 allocations each). The
size of the route table changes `router_match` by only 10 to 15 percent from 1
to 1000 routes (`ServeMux` matches through a tree); a wildcard route costs
about 2.7 times a static one and 11 allocations instead of 1. Rendering a page of
50 rows through `html/template` is the most expensive request-path scenario,
about 0.4 ms and 2300 allocations.

### End to end

`scripts/bench.sh` with its defaults (a generated application on PostgreSQL, the
load generator and the database on the same four CPUs).

```text
concurrency 16, 10s per run, 2s warm-up, 3 runs, index of 50 rows; figures are medians over the runs
```

| Scenario | Requests/s | Requests/s (min-max) | p50 ms | p95 ms | p99 ms | Errors |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `health` | 17869 | 17663-22101 | 0.67 | 2.36 | 4.55 | 0 |
| `welcome` | 16232 | 15201-17365 | 0.69 | 2.88 | 5.10 | 0 |
| `index_html` | 1346 | 1245-1412 | 11.0 | 22.2 | 29.9 | 0 |
| `show_json` | 5520 | 4640-5998 | 2.52 | 5.91 | 8.69 | 0 |
| `create_form` | 3786 | 2804-4036 | 3.88 | 7.36 | 10.0 | 0 |

`index_html` renders 50 rows per request and is about four times below
`show_json`. In the micro-benchmark the template alone takes about 0.4 ms of
CPU; the 11 ms median is the whole request with 16 workers sharing four CPUs
with the application and PostgreSQL (16 workers at 1346 requests/s is 11.9 ms
per request, Little's law). Where the rest goes was not profiled; profiling it
is the next item of the [roadmap](ROADMAP.md). `create_form` includes the
insert and the redirect. `health` (a database ping) and `welcome` (no database)
show what the rig does for requests without a table.

## Output format (shared with Forge-Rust)

Forge-Rust reports the same scenarios with the same columns.

**Micro-benchmarks** are listed one row per scenario and variant, with the
median of three runs:

```text
| Scenario | Variant | ns/op | B/op | allocs/op |
```

`ns/op` is wall-clock time per operation; `B/op` and `allocs/op` are heap
bytes and allocations per operation, or `n/a` where the language's tooling
does not measure them. Variants are named exactly as above (`static/100`,
`context/5`, `list_50`).

**End to end** is a markdown table with these columns, preceded by one line of
settings (concurrency, time per run, warm-up, runs, index rows):

```text
| Scenario | Requests/s | Requests/s (min-max) | p50 ms | p95 ms | p99 ms | Errors |
```

The scenario names are `health`, `welcome`, `index_html`, `show_json` and
`create_form`. Figures are medians over the runs; `Errors` is the total of
requests with an unexpected status or a transport failure. `-format json`
prints the same figures as a JSON document (`concurrency`, `duration_seconds`,
`warmup_seconds`, `runs`, `index_rows` and, per scenario, `scenario`, `runs[]`,
`median` and `errors`, each with `requests_per_second`, `p50_ms`, `p95_ms` and
`p99_ms`). The scenarios send the same requests to a generated application of
the same shape (tenant-scoped `post` scaffold with `title` and `body`, PostgreSQL,
non-superuser role).
