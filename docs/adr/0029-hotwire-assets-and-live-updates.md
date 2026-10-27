# ADR 0029: Hotwire, digested assets and live updates

- **Status:** Accepted
- **Date:** 2026-10-19
- **Amended by:** [ADR 0049](0049-typescript-by-default.md): the generated
  `app/javascript` is TypeScript, compiled by `forge` into `app/javascript/dist`
  (`forge new --js` keeps the JavaScript described below)

## Context

Server-rendered HTML ([ADR 0025](0025-server-rendered-html.md)) left the
front end where Rails 8 starts: pages and forms, but no JavaScript story. The
stylesheet was served at a fixed URL with a one-day cache, there was no way to
load a script under the strict Content-Security-Policy (no inline scripts), and
nothing pushed changes to open pages. Rails answers with an import map (no
Node), digested assets (Propshaft), Turbo and Stimulus, Turbo Streams in form
responses and `turbo_stream_from` with Action Cable. The Rust sibling
framework already vendors Turbo 8.0.23 and Stimulus 3.2.2 with integrity
pinning (its ADR 0022); the two frameworks should behave alike. Core packages
stay standard-library only ([ADR 0006](0006-zero-dependency-core.md)).

## Decision

### Packages

| Package | Adds | Imports |
| ------- | ---- | ------- |
| `assets` (core) | `New`, `Set` (`Path`, `Handler`), `NewImportMap`, `PinAllFrom` | stdlib |
| `hotwire` (core) | vendored `turbo.js` and `stimulus.js`, `Files`, `Pins`, `StyleHashes` | `assets` |
| `turbo` (core) | `Stream` and its seven actions, `DomID`, `Requested`, `Render` | `web`, `uuid` |
| `broadcast` (core) | `Channel`, `Signer`, `Broker`, `Memory`, `NewHandler`, `Publish` | `turbo`, `tenancy`, `web`, `fault` |
| `broadcast/postgres` | `Broker` over LISTEN/NOTIFY | `broadcast`, `postgres` (pgx) |
| `views` | `Options.Assets/ImportMap/StyleHashes/Streams`, helpers, `Renderer.Fragment` | `assets`, `broadcast`, `turbo` |
| `web` | `AcceptsExplicitly` | unchanged |

### Digested assets

`assets.New` digests every embedded file once at startup (SHA-256) and serves
`stem-<16 hex>.ext` with `Cache-Control: public, max-age=31536000, immutable`,
the full digest as `ETag`, `nosniff` and a closed list of content types. The
handler serves exactly the table's served names, through `http.ServeContent`
(conditional requests, `HEAD`, ranges); everything else is `404`, including
undigested names, directories, `..` and encoded separators (a request whose
`RawPath` differs from its path never matches). The 16-hex digest and naming
match the Rust framework, so identical files get identical URLs. A logical
name must be letters, digits, `.`, `_` and `-` without a leading dot; an
unserved extension or a duplicate name across sources is a startup error, so
what is public is decided by what is embedded.

### Import map and CSP

`assets.ImportMap` renders `{"imports": {...}}` with `json.Marshal` (sorted
keys; `<`, `>`, `&`, U+2028/9 escaped so the JSON cannot end the script) and
exposes the base64 SHA-256 of the exact body. `views.Load` inserts
`'sha256-...'` into `script-src` of the default policy; there is no
`'unsafe-inline'`, no nonce and no per-request work. Turbo creates one inline
`<style>` (its progress bar) that `style-src 'self'` would block, so the
policy also carries the hash of that exact text (`hotwire.StyleHashes`); a
test recomputes it from the vendored `turbo.js` and fails if either changes.
A hash was chosen over a nonce because a nonce allows any inline element that
carries it, and over `'unsafe-inline'` because that defeats the policy. A
custom `Options.ContentSecurityPolicy` is used verbatim.

Helpers `assetPath`, `stylesheetTag` and `javascriptImportmapTags` take
logical names; referencing an unknown asset, or using a helper without its
Option, stops `Load`. They return constant markup around digested paths
(escaped again), never application input.

### Vendored Turbo and Stimulus

The files are the Rust framework's, byte for byte, with licences retained
(`hotwire/vendor/TURBO-LICENSE`, `STIMULUS-LICENSE`) and provenance in
`hotwire/vendor/README.md`. `hotwire_test.go` pins their SHA-256 values. The
npm tarball integrity (sha512) cannot be checked offline; it is recorded from
the Rust project's verification. Updating is a deliberate three-step change
(replace, update constants, update the CSP hash) that the tests enforce.

Generated applications get `app/javascript/application.js`,
`controllers/{application,index,hello_controller}.js` and `app/assets`
(stylesheet, logo, bundle). `PinAllFrom` pins every controller by bare name,
because relative imports would bypass the digested URLs. The layout includes
the import map tags next to the existing `csrfMeta`, which Turbo reads. The
welcome page, a standalone document with an inline stylesheet, sets
`turbo-visit-control: reload`, because Turbo would otherwise swap its inline
styles into a document whose policy forbids them.

### Turbo Streams

`turbo` builds `<turbo-stream>` elements from validated DOM ids and
`template.HTML` content. `Renderer.Fragment` renders a partial (`_NAME`) so
streams use the same escaping as pages. `turbo.Requested` requires the Accept
header to name `text/vnd.turbo-stream.html` itself (`web.AcceptsExplicitly`),
because `web.Negotiate` alone would hand a stream to `*/*` clients. Fragments
fail on `csrfField`/`csrfMeta` (no response writer), so a request-bound token
cannot enter a broadcast.

### Broadcasts over Server-Sent Events

SSE, not WebSockets: Turbo supports it natively through
`<turbo-stream-source>`, it is plain HTTP through every proxy and middleware,
needs no upgrade handshake or origin protocol of its own, and the Rails
`turbo_stream_from` case is server-to-client only.

- **Channel names are signed capabilities.** `Signer` HMAC-SHA256s
  `channel + expiry` with a domain-separated label (`stdlib crypto/hmac`; no
  custom cryptography), constant-time compared, with key rotation via
  `PreviousKeys`. The key must be at least 32 bytes and is shared by all
  replicas (`FORGE_STREAMS_KEY` in generated applications; staging and
  production refuse to start without it, development uses a random per-process
  key). Tokens expire (24 hours default, 7 days maximum).
- **Tenant channels fail closed.** A `Channel` with a tenant is a different
  channel from the global one of the same name. The handler requires the
  request's tenant (`tenancy.FromContext`, set by authentication) to equal
  the channel's, and `turboStreamFrom` fails the render without a tenant;
  global channels need the explicit `turboStreamFromGlobal`. `Authorize` adds
  application rules (the scaffold requires `NAME:read`).
- **Re-authorization.** Streams end after `MaxConnectionAge` (30 minutes) and
  the browser reconnects, so a revoked session or permission stops receiving.
  A token alone is a bearer capability, not an identity, hence the
  authentication and tenant checks in front of it.
- **Bounded and lifecycle-owned.** `Memory` starts no goroutines (non-blocking
  sends to bounded queues; a full queue drops the subscriber with
  `ErrSlowConsumer`); subscribers are capped per channel and per process;
  messages are capped (1 MiB). The handler is the request goroutine. The
  broker closes when the context given to `NewMemory` ends, which is
  Configure's context, cancelled when shutdown begins, because
  `App.OnShutdown` hooks run only after the HTTP server has drained and open
  streams would otherwise hold the drain until its timeout. This uses
  `context.AfterFunc`, which runs only because the context ended.
- **Timeouts.** `http.Server.ReadTimeout`/`WriteTimeout` would end every
  stream. The handler clears the read deadline and sets a fresh write
  deadline (default 10 seconds) before every write with
  `http.ResponseController`, so a client that stops reading cannot pin a
  connection. The write deadline is cleared again after each flush: HTTP/2
  resets a stream whose write deadline passes even with nothing to write, and
  the heartbeat (20 seconds) is longer than the write timeout (10 seconds), so
  a deadline left set would end every idle stream. Tests run real HTTP/1.1 and
  HTTP/2 (TLS) servers with short timeouts and a longer heartbeat. Clearing
  the read deadline is defensive; no test shows it is required.
- **Per-account limits.** Each stream also takes a slot of its principal (16
  by default; the client address when no principal is authenticated) and of
  its tenant (1000), so one account cannot take a channel's whole capacity;
  over the limit is `429 stream_limit` with `Retry-After`.
- **SSE framing.** Payload lines are split on `\n`, `\r\n` and `\r` and each
  becomes a `data:` field, so a payload cannot end an event, inject `id:` or
  `event:` fields, or smuggle comments.
- **No replay.** Delivery is at most once. Last-Event-ID replay would need a
  retained, tenant-partitioned log with its own retention and authorization;
  Turbo pages are re-rendered on navigation, so a gap heals on the next
  visit. This is documented, not hidden.

### PostgreSQL LISTEN/NOTIFY

`broadcast/postgres` wraps a `Memory` broker. One listener goroutine, owned by
the broker (`Shutdown` stops and waits), holds a dedicated connection that it
hijacks out of the pool on exit (so it never returns to the pool still
listening) and reconnects with 1-30 second backoff. `Publish` delivers locally
at once and sends `NOTIFY`; each broker skips notifications carrying its own
origin id, so a message is heard once. `PublishTx` sends in the caller's
transaction with origin `-`, so it is delivered everywhere (including locally)
only on commit.

NOTIFY payloads must be under 8000 bytes. Messages are chunked (4500 bytes
base64 encoded plus a header) into consecutive notifications of one
transaction, which PostgreSQL delivers contiguously and in order, and
reassembled with bounded buffers (64 pending messages, a byte budget, a 30
second lifetime; chunks out of order or changing channel discard the
message). The alternative, a reference to a row in a table, needs a migration,
retention and a cleanup job for a feature that is otherwise stateless;
chunking was chosen and the limit (256 KiB) is explicit. Notifications that do
not parse are logged and ignored. NOTIFY is database-wide and not
tenant-aware: the channel key carries the tenant, exactly as in memory, and
whoever can NOTIFY on the database can publish.

### Scaffold

`forge generate scaffold ... --broadcast` is opt-in. It opens a long-lived
endpoint and requires a signing key and wiring (an authentication middleware
and a per-channel permission), so on-by-default would either ship an endpoint
nobody reviewed or force configuration on every scaffold. It generates
`app/adapters/htmlstreams` once per application, the `_item` row partial, and
publishing in the htmlui adapter (create `prepend`, update `replace`, delete
`remove`) to the tenant's or the global `<plural>` channel after the change
is stored; a publish failure is logged and never fails the request. The
printed wiring never opens the stream to the public: without Forge identity
both guards are deny-all, and with it the stream needs `AuthenticateCookie`
and `<plural>:read`. The generated tests run a real event stream end to end on
PostgreSQL. Applications whose views package predates this ADR are refused
with instructions rather than patched.

`forge new` moves the stylesheet and logo from `app/bootstrap/assets` to
`app/assets` (the views package needs the bundle, and bootstrap imports views,
so it cannot live in bootstrap), and `identityRoutes` takes the context so
the broker can follow it.

## Consequences

- Pages load JavaScript under a strict policy with no inline script except one
  hashed import map; assets are cacheable forever.
- A page can show changes made by other people without polling, across
  replicas on PostgreSQL, with tenant isolation enforced by the channel key,
  the signed name and the request's tenant.
- Applications upgrading from earlier generated code keep working (their
  layout still links `/assets/application.css` and their own handler) but must
  adopt the new views package to use helpers or `--broadcast`.
- Each open stream holds one connection and one request goroutine; capacity
  is bounded by the subscriber limits and the server's connection limits.
  Behind a proxy, streams need buffering disabled and an idle timeout above
  the heartbeat.
- Stream names appear in URLs and therefore in access logs for their
  lifetime. They carry no secrets beyond the capability to read one channel,
  the same as Rails' signed stream names; short TTLs and re-authorization
  limit exposure.
- Not provided: WebSocket transport, replay of missed messages, presence,
  per-user streams, Turbo Frames helpers, morphing and `turbo_refreshes_with`.
