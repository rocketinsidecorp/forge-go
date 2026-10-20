# ADR 0043: OpenAPI from typed routes

- **Status:** Accepted
- **Date:** 2026-10-20
- **Supersedes:** [ADR 0022](0022-openapi-from-typed-routes.md) (proposed)

This decision is language-neutral: Forge-Go and Forge-Rust produce the same
document from the same declarations, behind the same variable, route and
commands (see [Forge-Rust port](#forge-rust-port)). The guide is
[docs/OPENAPI.md](../OPENAPI.md).

## Context

Rails has no built-in API description; gems such as rswag write one by hand
next to request specs, and it drifts. Forge JSON handlers are plain
`http.Handler`s that decode with `web.DecodeJSON`, encode with `web.JSON`
and answer errors with `web.Error` (`{"error":{"code","message","request_id"}}`,
the only error shape, not RFC 9457 problem+json). Nothing described them.

ADR 0022 proposed a new `api.Handle[Req, Res]` registration that also owns
binding and encoding. Generated resources, however, already have handlers
with behavior a generic binder does not express (PATCH members that
distinguish absent from `null`, a `Location` header, `?version=` on DELETE,
per-field error codes for times and UUIDs), and rewriting them would be a
parallel routing abstraction. Constraints: the core is standard-library only
(ADR 0006); reflection must not run per request; a wrong description is
worse than none; the document must be stable enough to commit and diff.

## Decision

1. **Descriptions travel with the handler, opt-in.** Package `openapi`
   (core) adds `Describe(handler, Operation) http.Handler`: the handler
   behaves exactly as before and carries an `Operation` (ID, summary,
   description, tags, deprecated, `Permission`, `Authenticated`, `Params`,
   `Request`, `Responses`, `Errors`). Types are named with generics:
   `openapi.TypeOf[T]()`, `openapi.JSON[T](status)`, `openapi.Empty(status)`.
   Registration is unchanged (`app.Handle(pattern, openapi.Describe(h, op))`,
   so `forge routes` still sees the literal pattern); `router.Lookup` returns
   the registered handler and the document is built from the router's own
   route table. Undescribed routes keep working and are listed as
   undocumented, never guessed at. Wrap after any per-route guard, so the
   registered handler is the described one.
2. **Accurate by construction where possible.** `openapi.Typed[Req, Res]
   (status, op, func(*http.Request, Req) (Res, error))` decodes `Req`
   strictly with `web.DecodeJSON` (unless `Req` is `openapi.NoBody`), calls
   `Req.Validate()` when present (value or pointer receiver; it should
   return a `*fault.Error`, and the operation documents 422), writes `Res` with `web.JSON` and errors
   with `web.Error`, and derives `op.Request` and the success response from
   its type parameters, so the description cannot drift. For handlers with
   richer behavior, `Describe` plus contract tests (6) close the gap.
3. **Schemas follow `encoding/json`, reflected once.** Struct fields use
   encoding/json's rules: `json` names, `-`, embedded structs with Go's
   promotion and conflict rules, `,string`; a member is `required` unless
   `omitempty`/`omitzero` or promoted through an embedded pointer;
   pointers, slices, maps and `[]byte` are nullable (`["T","null"]` or
   `anyOf [..., null]`) because nil encodes as `null`; objects are
   `additionalProperties: false`, which is also what `web.DecodeJSON`
   accepts. `time.Time` is `date-time`; `uuid.UUID` is `format: uuid` with
   the canonical UUIDv7 pattern; integers carry `int32`/`int64` and ranges;
   `[]byte` is base64; `json.RawMessage` and interfaces are any value; maps
   need string, integer or `TextMarshaler` keys. Types with their own
   encoding are any value (`json.Marshaler`) or a string
   (`TextMarshaler`) unless they implement `openapi.Typer`
   (`OpenAPIType() Type`, "encodes like this type"). Tags refine a field:
   `doc` (description), `example` (JSON or text), `format`, and
   `openapi:"required"`/`openapi:"nonnull"`. Channels, functions and
   complex numbers, unknown `openapi` options, a `Params` path field the
   pattern lacks, duplicate operation IDs and statuses, a body on 204 or
   304, and methods OpenAPI 3.1 lacks are build errors.
4. **Components and determinism.** Every named struct type is a component
   (`$ref: #/components/schemas/Name`), which also terminates recursion.
   Names are the Go name in upper camel case (snake_case parts joined) and type arguments
   reduced to their names (`pagination.Page[postResponse]` is
   `Page_PostResponse`); a type may choose its name with `openapi.Namer`
   (`OpenAPIName() string`; `health.Result` is `HealthReport`). Names that
   collide are qualified with the package name of the type and of its type
   arguments (`pagination.Page_v1.Item`, `pagination.Page_v2.Item`), then
   with package paths; names that still collide (function-local types of
   one package, an `OpenAPIName` equal to another name) are a build error
   naming both types, never one schema standing for two. Paths are
   sorted, object keys are sorted, the JSON is indented with two spaces and
   ends in a newline, and HTML characters are not escaped: the same routes
   and types always produce the same bytes.
5. **Errors and security.** Every error response, listed or implied, and
   the `default` response use `web.ErrorBody` (`ErrorBody`/`ErrorDetail`
   components). A request body implies 400, 413 and 415 (what decoding
   answers); `Authenticated` implies 401 and the `bearerAuth` scheme;
   `Permission` adds 403 and `x-forge-permission`. A response with its own
   body type and an error status is declared in `Responses` (readiness
   answers 503 with its report). Path parameters come from the pattern
   (`{id}`; `{rest...}` becomes `{rest}`; `{$}` is dropped) and default to
   strings; `Params` is a struct with `path`, `query` and `header` tags.
   `pagination.Query` documents `cursor` and `limit`.
6. **Contract tests.** `Document.ValidateResponse(method, path, status,
   header, body)` validates a response against the published document (the
   JSON Schema subset Build emits: `$ref`, `anyOf`, `type`, `properties`,
   `required`, `additionalProperties`, `items`, lengths, ranges, `pattern`,
   `date-time`, `uuid`, base64), matching the request with `ServeMux`'s own
   rules. `Document.CheckResponses(handler, report)` wraps a handler in tests
   so every response of a documented route is checked; generated resource
   and `--api` controller tests use it, so each of their tests is also a
   contract test.
7. **Served only where safe.** `FORGE_OPENAPI_ENABLED` (true/false): unset
   serves `GET /openapi.json` in development and test only; `true` serves it
   in staging and production too (with a startup warning); `false` never.
   When served, the document is checked as `Run` starts, so an inaccurate
   description stops startup rather than failing the first request; the
   served copy is built on its first request, after the router has frozen,
   so routes added between `Run` and the first request are included. The
   route is mounted when the first request arrives; an application that
   registered its own `/openapi.json` (a hand-written document) keeps it
   and Forge's is not mounted, so upgrading breaks no existing route. The
   document lists every documented route, so production does not publish it
   by default ([threat model](../THREAT_MODEL.md)).
8. **The binary writes it; the CLI mirrors `forge migrate`.** The
   application binary answers the reserved `openapi [--output FILE | --check
   FILE]` command: it runs `Configure` without listening, writes the
   document to stdout (logs go to stderr) or FILE, lists undescribed routes
   on stderr, and `--check` exits 1 when FILE differs (for CI). `forge
   openapi` builds `cmd/NAME` and runs that command with the development
   environment, like `forge migrate`. `forge routes` stays static (it never
   runs application code): types can only be known by running the
   application, so the document cannot come from source analysis.
9. **Generated applications describe their JSON endpoints.** `forge new`
   calls `app.SetAPIInfo` with the application's name, serves `GET
   /v1/status` through `openapi.Typed`, and describes `GET /v1/auth/session`;
   its bootstrap test fetches `/openapi.json` and validates the status
   response. `forge generate resource|scaffold` describe their five JSON
   routes with operation IDs, permissions, parameters and types (create and
   PATCH requests mark required, optional and non-null members; the PATCH
   wrapper implements `Typer`), and `--api` controllers describe their
   actions. The framework's health routes are described. The `auth`
   package's JSON handlers describe themselves (`openapi.Describe` inside
   their constructors, which the application registers as returned):
   password and cookie sign-in, sign-out, MFA completion and password
   recovery, with the statuses each answers, `Retry-After` and
   `Set-Cookie`, the `Origin` and `X-CSRF-Token` headers cookie routes need,
   and no examples on secret-bearing members. The generated bootstrap test
   checks that the identity paths are in the document and that an identity
   response matches it.

## Consequences

- A fresh application has a meaningful, valid OpenAPI 3.1 document with no
  YAML to maintain; client generation is left to standard OpenAPI tooling.
- Descriptions declared with `Describe` can still be wrong about what a
  handler does; only `Typed` handlers are correct by construction. The
  contract tests catch drift for whatever the tests exercise.
- `forge openapi` runs `Configure`, so it needs what startup needs (the
  development database for PostgreSQL and MySQL applications), like
  `forge migrate`.
- Nil slices encode as `null`, so array members are nullable even when a
  handler always allocates them; this is accurate, if less convenient for
  generated clients.
- Adding `openapi` to the reserved binary commands makes an application
  command of that name a startup error.

## Follow-ups

- Describe the TOTP enrollment, confirmation, rotation and disable
  handlers of the `auth` package (their sign-in, sign-out, MFA completion,
  recovery and cookie siblings are described). An application mounts them
  behind its own guard, so the registered handler is the guard and a
  description inside the constructor would be hidden; their step-up
  authorizers choose their error statuses. `Middleware.Authenticate` would
  have to pass a handler's description through (adding `Authenticated`),
  and the authorizers' statuses would need declaring.
- A security scheme for the session cookie and `in: cookie` parameters:
  cookie routes document their `Origin` and `X-CSRF-Token` headers and
  `Set-Cookie`, and say in prose that the session and CSRF cookies are sent.
- Request validation from the document (rejecting what the schema
  forbids before the handler) and request contract checks in tests.
- `enum`, `minLength`/`maxLength` and other constraints from explicit tags,
  shared with domain validation, instead of free-text `doc`.
- A `--check` step in the generated CI workflow once applications commit
  their `openapi.json`.
- A `describe-only` mode for `Configure` so `forge openapi` does not need a
  database.
- Form (`application/x-www-form-urlencoded`) and HTML responses, and
  Server-Sent Events, are not described.

## Forge-Rust port

Planned ([forge-rust#25](https://github.com/rocketinsidecorp/forge-rust/issues/25)). Same contract, Rust idioms:

- **Declaration:** an opt-in description attached to a route next to its
  handler, with the same fields (`id`, `summary`, `description`, `tags`,
  `deprecated`, `permission`, `authenticated`, `params`, `request`,
  `responses`, `errors`), and a typed handler equivalent to `Typed`
  (`Json<Req>` in, `Json<Res>` out) whose description comes from its types.
  Undescribed routes are listed, not guessed. Types describe themselves
  through a derive or trait (the counterpart of reflection, `Typer` and
  `Namer`) that follows serde's attributes the way Go follows
  encoding/json: `rename`, `skip`, `flatten`, `skip_serializing_if` as
  optional, `Option<T>` as nullable, `deny_unknown_fields` as
  `additionalProperties: false`; `uuid::Uuid` (v7) and `time`/`chrono`
  timestamps map to the same `uuid` pattern and `date-time` format.
- **Document:** OpenAPI 3.1.0 with the same `jsonSchemaDialect`, the same
  error components (`ErrorBody`, `ErrorDetail`) for every error and
  `default` response, the same implied statuses (400/413/415 with a body,
  401 with authentication, 403 and `x-forge-permission` with a
  permission), the `bearerAuth` scheme, the same operation ID derivation
  (`getV1PostsById`), the same component naming and collision rule, the same
  path conversion, and byte-for-byte the same formatting (sorted keys,
  two-space indent, trailing newline, no HTML escaping), so both frameworks
  produce identical files for equivalent applications. The same build
  errors.
- **Serving:** `GET /openapi.json`, `FORGE_OPENAPI_ENABLED` with the same
  default (development and test only), the same startup check and warning.
- **Commands:** the application binary's `openapi [--output FILE | --check
  FILE]` and `forge openapi [--output FILE | --check FILE]` with the same
  output, stderr notes and exit statuses.
- **Generated code and tests:** the same described endpoints (`/v1/status`,
  `/v1/auth/session`, the auth package's sign-in, sign-out, MFA completion,
  recovery and cookie routes with the same statuses and headers, health routes,
  resources, `--api` controllers), the
  same operation IDs (`createPost`, `listPosts`, `getPost`, `updatePost`,
  `deletePost`), and contract tests that validate every handler response
  (the counterparts of `ValidateResponse` and `CheckResponses`).
