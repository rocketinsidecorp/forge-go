# ADR 0022: OpenAPI from typed routes

- **Status:** Superseded by [ADR 0043](0043-openapi-from-typed-routes.md)
- **Date:** 2026-10-14

ADR 0043 keeps this decision's goals (Go types as the single source of
truth, OpenAPI 3.1, nothing reflected per request, the binary's `openapi`
command and `forge openapi --check`) but attaches descriptions to ordinary
handlers instead of adding a separate `api.Handle` registration.

## Context

Forge handlers are plain `http.Handler`s: each one decodes, validates,
calls the application and encodes by hand, and nothing describes the API to
clients. Writing an OpenAPI document by hand duplicates every schema and
drifts; generating handlers from a hand-written document inverts the source
of truth into YAML. The goal is one source of truth (Go types), no schema
duplication, and no reflection on the request path.

## Decision

- A standard-library core package `api` registers **typed operations**:
  `api.Handle[Req, Res any](router, api.Operation{...}, func(ctx, Req) (Res, error))`.
  The operation declares method and path, summary, success status, the
  permission it requires, and its error codes. The adapter binds path
  (`path:"id"`), query (`query:"limit"`) and strict JSON body fields into
  `Req`, calls an optional `Validate() error`, encodes `Res` with
  `web.JSON` and errors with `web.Error`.
- **Schemas come from the Go types**, by reflection once at registration and
  cached: structs and `json` tags, `omitempty` and pointers as optional,
  slices, string-keyed maps, `time.Time` (`date-time`), `uuid.UUID`
  (`uuid`, documented as v7), integers with formats. Nothing is reflected
  per request.
- The document is **OpenAPI 3.1** (JSON Schema 2020-12), with the single
  Forge error body, bearer and cookie security schemes, and each operation's
  permission as `x-forge-permission`.
- The spec is produced by the application binary's `openapi` subcommand, so
  it never needs a running server or network exposure. `forge openapi`
  builds the application and prints or writes it; `forge openapi --check`
  fails when a committed `openapi.json` is out of date (for CI). Serving it
  over HTTP is an explicit, opt-in route.
- Resource generators emit typed operations, so generated APIs are
  documented by construction.

## Consequences

- Handlers lose their decode/encode boilerplate and gain a documented
  contract; plain `http.Handler` routes keep working and are listed as
  undocumented.
- Client generation is delegated to standard OpenAPI tooling; Forge does not
  ship generated clients.
- Validation rules beyond types (lengths, ranges) live in `Validate()` and
  are not yet reflected in the schema; adding them needs explicit tags and a
  follow-up decision.
