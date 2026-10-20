# OpenAPI from typed routes

Forge describes an application's JSON API as an OpenAPI 3.1 document built
from the Go types its routes declare, so the description cannot be edited
out of step with the code. Nothing is written by hand and nothing is
reflected per request. The decision record is
[ADR 0043](adr/0043-openapi-from-typed-routes.md).

```bash
forge new shop && cd shop && forge dev
curl http://127.0.0.1:8080/openapi.json            # development and test only, by default

forge openapi                                      # print the document
forge openapi --output openapi.json                # write it, to commit and diff
forge openapi --check openapi.json                 # exit 1 when the committed file is stale (CI)

FORGE_OPENAPI_ENABLED=true ./bin/shop              # serve it in staging or production too
```

## Describe a route

A route keeps its ordinary handler. `openapi.Describe` attaches what it
accepts and answers:

```go
import "github.com/rocketinsidecorp/forge-go/openapi"

type createPost struct {
	Title string  `json:"title" doc:"Shown in lists." example:"Hello"`
	Body  *string `json:"body,omitempty"`
}

type post struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

err := app.Handle("POST /v1/posts", openapi.Describe(protected, openapi.Operation{
	Summary:    "Create a post",
	Tags:       []string{"posts"},
	Permission: "posts:write",                   // bearerAuth, 401, 403, x-forge-permission
	Request:    openapi.TypeOf[createPost](),    // 400, 413, 415 are implied
	Responses:  []openapi.Response{{Status: http.StatusCreated, Body: openapi.TypeOf[post](), Headers: []string{"Location"}}},
	Errors:     []int{http.StatusConflict},      // answered with web.Error
}))
```

Wrap the handler that is registered, after any per-route guard: the
document reads the handler the router holds. Undescribed routes keep working;
`forge openapi` lists them on stderr and the document leaves them out.

`openapi.Typed` goes further: it decodes the request, calls the function and
writes the result, and takes the description from the function's types, so
the two cannot disagree.

```go
type status struct {
	Service string `json:"service"`
	Status  string `json:"status"`
}

app.Handle("GET /v1/status", openapi.Typed(http.StatusOK,
	openapi.Operation{Summary: "Service status", Tags: []string{"status"}},
	func(r *http.Request, _ openapi.NoBody) (status, error) {
		return status{Service: "shop", Status: "ok"}, nil
	}))
```

A `Typed` request type is decoded strictly with `web.DecodeJSON` (unless it
is `openapi.NoBody`), and its `Validate() error` method runs when it has one.
Errors go through `web.Error`.

Name the API once, in `Configure`:

```go
app.SetAPIInfo(openapi.Info{Title: "shop", Version: "0.1.0"})
```

## Parameters

Path parameters come from the pattern (`{id}` is a required string). Give
them types and documentation, and declare query and header parameters, with
a `Params` struct:

```go
type deleteParams struct {
	ID      uuid.UUID `path:"id" doc:"The post."`
	Version int       `query:"version" openapi:"required"`
	Trace   string    `header:"X-Trace"`
}

openapi.Operation{Params: openapi.TypeOf[deleteParams](), ...}
```

`pagination.Query` documents the `cursor` and `limit` parameters of
`pagination.FromQuery`. A `path` field the pattern lacks is an error.

## How Go types become schemas

The schema says what `encoding/json` writes and `web.DecodeJSON` accepts.

| Go | JSON Schema |
| --- | --- |
| `string`, `bool` | `string`, `boolean` |
| `int`, `int64`, `uint`... | `integer` with `int32`/`int64` and ranges |
| `float32`, `float64` | `number` (`float`, `double`) |
| `time.Time` | `string`, `date-time` |
| `uuid.UUID` | `string`, `uuid`, the canonical UUIDv7 pattern |
| `[]byte` | `string`, base64 |
| `*T`, `[]T`, `map[string]T` | nullable (`["T","null"]` or `anyOf [..., null]`): nil encodes as `null` |
| `[N]T` | array of exactly N items |
| named struct | `$ref: #/components/schemas/Name` (recursion terminates there) |
| anonymous struct | inline object |
| `any`, `json.RawMessage`, a `json.Marshaler` | any value |
| an `encoding.TextMarshaler` | `string` |

Objects list their members with `additionalProperties: false`. A member is
`required` unless it has `omitempty` or `omitzero` (or comes from an
embedded pointer, whose fields may be absent). Embedded structs are
flattened with Go's rules; `json:"-"` is skipped; `json:",string"` makes
the value a string.

Field tags:

| Tag | Effect |
| --- | --- |
| `doc:"..."` | `description` |
| `example:"..."` | `examples` (parsed as JSON when it is JSON) |
| `format:"email"` | `format` |
| `openapi:"required"` | required even with `omitempty` |
| `openapi:"nonnull"` | not nullable even if a pointer |

A type whose encoding differs from its structure implements `openapi.Typer`
(`OpenAPIType() openapi.Type`, "encodes like this type"); generated PATCH
fields use it to read as `T` or `null`. A type chooses its component name
with `OpenAPIName() string` (`openapi.Namer`; no import needed). Component
names are the Go names with a capital first letter (`Page_PostResponse` for
`pagination.Page[postResponse]`); names that collide are qualified with the
package (`httpapi.Item`).

Build errors, never guesses: channels, functions and complex numbers;
maps with other key types; unknown `openapi` options; a body on 204 or 304;
the same status as a response and an error; duplicate operation IDs;
methods OpenAPI 3.1 has no field for.

## Errors and security

Every error response, and the `default` response, is `web.ErrorBody`
(`{"error":{"code","message","request_id"}}`), Forge's only error format.
Statuses are added for what the framework answers itself:

| Declared | Adds |
| --- | --- |
| `Request` | 400, 413, 415 |
| `Authenticated: true` | 401, `security: bearerAuth` |
| `Permission: "posts:write"` | 401, 403, `security: bearerAuth`, `x-forge-permission` |

A response with its own body and an error status (readiness answers 503
with its report) goes in `Responses`.

## Serving and the CLI

`GET /openapi.json` is served when `FORGE_OPENAPI_ENABLED` allows it:

| `FORGE_OPENAPI_ENABLED` | development, test | staging, production |
| --- | --- | --- |
| unset | served | not served |
| `true` | served | served, with a warning at startup |
| `false` | not served | not served |

The document lists every described route, admin and internal ones included,
with the permission each requires: publishing it in production is a
decision ([threat model](THREAT_MODEL.md)). When it is served, it is built as
the server starts, so an inaccurate description stops startup.

`forge openapi` builds `cmd/NAME` and runs the binary's `openapi` command
with `.env.development` loaded, like `forge migrate`. It runs `Configure`
without listening, so PostgreSQL and MySQL applications need their
development database running. The binary itself accepts the same command,
with the same environment `serve` needs:

```bash
forge build && ./bin/shop openapi --output openapi.json
```

In code, `app.OpenAPI()` returns the `*openapi.Document`
(`JSON()`, `Paths()`, `Undocumented()`).

## Contract tests

`Document.ValidateResponse(method, path, status, header, body)` checks a
response against the document. `Document.CheckResponses` wraps a handler in
tests and reports every response of a documented route that does not match:

```go
spec, err := app.OpenAPI()
if err != nil {
	t.Fatal(err)
}
handler := spec.CheckResponses(app.Handler(), func(r *http.Request, err error) {
	t.Errorf("%s %s: %v", r.Method, r.URL, err)
})
// every request below is now also a contract test
```

Generated resources and `--api` controllers build their test handlers this
way, and the generated bootstrap test fetches `/openapi.json` and validates
`GET /v1/status`.

## What generated applications describe

- `forge new`: `GET /v1/status` (`openapi.Typed`), the health routes, and
  with PostgreSQL `GET /v1/auth/session`.
- `forge generate resource|scaffold`: `createPost`, `listPosts`, `getPost`,
  `updatePost` and `deletePost`, with their permissions, `pagination.Query`,
  the `{id}` and `?version=` parameters, the `Location` header and the
  error statuses. Create requests mark required members as present and
  non-null; PATCH members are optional, and `null` only where it clears an
  optional field.
- `forge generate controller ... --api`: each action.

Not described yet: the `auth` package's JSON routes (`/auth/login`,
logout, MFA, recovery and cookie routes), HTML pages, forms and event
streams. See the follow-ups in ADR 0043.
