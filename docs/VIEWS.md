# Server-rendered HTML

Forge applications can serve HTML pages and forms as well as JSON: layouts
and partials, form decoding with field errors, redirects, flash messages,
CSRF-protected forms and `PATCH`/`DELETE` from HTML forms. Everything is in
the standard-library core. The decisions and threat model are in
[ADR 0025](adr/0025-server-rendered-html.md).

## What `forge new` generates

```text
app/views/views.go                    embeds the templates; views.New() builds the renderer
app/views/views_test.go               renders the layout through the middleware
app/views/layouts/application.gohtml  the layout every page with a "content" block uses
app/views/shared/_flash.gohtml        flash messages, included by the layout
app/bootstrap/assets/application.css  the stylesheet, served at /assets/application.css
```

A PostgreSQL application also gets the sign-in pages (see
[Authentication pages](#authentication-pages)):

```text
app/adapters/htmlauth/htmlauth.go      sign-in, sign-out, two-factor and password-reset handlers
app/adapters/htmlauth/htmlauth_test.go drives them through the middleware
app/views/sessions/new.gohtml          sign-in form
app/views/sessions/mfa.gohtml          authenticator or backup code
app/views/sessions/_sign_out.gohtml    a sign-out button to include where wanted
app/views/passwords/new.gohtml         forgot-password form
app/views/passwords/edit.gohtml        choose a new password
```

`bootstrap.Configure` installs three middleware before routing, for every
route:

```go
protection, err := csrf.New(csrf.Options{})
...
for _, middleware := range []func(http.Handler) http.Handler{web.MethodOverride, protection.Handler, web.Flash} {
	if err := app.Use(middleware); err != nil {
		return err
	}
}
```

- `web.MethodOverride` turns a `POST` form with `_method=PATCH` (or `PUT`,
  `DELETE`) into that method before routing.
- `csrf.Protection.Handler` rejects unsafe requests without the CSRF token or
  from another origin (see [CSRF](#csrf)).
- `web.Flash` carries messages such as "Post was created." to the next
  request.

## Templates

Templates are Go [`html/template`](https://pkg.go.dev/html/template) files
with the `.gohtml` extension. A template's name is its path below
`app/views` without the extension:

| File | Kind | Name |
| ---- | ---- | ---- |
| `layouts/application.gohtml` | layout | `layouts/application` |
| `shared/_flash.gohtml` | partial (leading `_`) | `shared/_flash` |
| `posts/_form.gohtml` | partial | `posts/_form` |
| `posts/index.gohtml` | page | `posts/index` |

A page that defines a `content` block is rendered inside the layout; it can
also define `title`. A page without `content` is a complete document.
Partials are included with `{{template "posts/_form" .}}`; to pass several
values, build a map with `dict`.

Every page is parsed with its own copy of the layouts and partials, so each
page's `content` and `title` stay its own. `views.New()` parses and escapes
every page at startup and fails on a template html/template cannot escape
safely. The generated `views.go` embeds templates one directory deep
(`//go:embed */*.gohtml`); add `*/*/*.gohtml` before nesting deeper.

### Helpers

| Helper | Output |
| ------ | ------ |
| `{{csrfField}}` | `<input type="hidden" name="authenticity_token" value="...">`; put it in every form that does not use `GET` |
| `{{csrfMeta}}` | `csrf-param` and `csrf-token` meta tags for scripts (the layout has them) |
| `{{csrfToken}}` | the token, for a `data-` attribute |
| `{{methodField "PATCH"}}` | `<input type="hidden" name="_method" value="PATCH">`; `PUT`, `PATCH` or `DELETE` only |
| `{{range flashes}}` | the incoming `web.FlashMessage` values (`.Kind`, `.Text`) |
| `{{flash "notice"}}` | the text of one incoming message, or empty |
| `{{domID "post" .ID}}` | `post_0190...`, or `new_post` for a zero UUID |
| `{{dict "Form" .Form "Action" "/posts"}}` | a map, for partials |

Values are escaped for their context (HTML text, attribute, URL, CSS, JS) by
html/template. Do not convert user input to `template.HTML`, and do not add
functions that do.

### Rendering

Build the renderer once and share it; it is safe for concurrent use:

```go
renderer, err := views.New() // the application's app/views package
```

Render with the request so the CSRF and flash helpers work:

```go
err := renderer.RenderRequest(w, r, "posts/index", http.StatusOK, data)
```

The page is executed into a buffer first, so a template error never sends a
partial response. Pages are sent with
`views.DefaultContentSecurityPolicy`, which allows same-origin scripts,
styles, images and fonts, forms posting to the same origin, and nothing
inline: put styles in `app/bootstrap/assets/` files, not in `style=`
attributes or `<style>` elements.

## Scaffold a CRUD

`forge generate scaffold` writes the HTML CRUD of the example below, backed by
a real database table, in one command (the equivalent of `rails g scaffold`).
It takes the arguments of `forge generate resource`, writes the same files for
the resource (migration, domain, application, persistence, JSON API and their
tests) and adds the pages:

```sh
forge generate scaffold post title:string body:text published:bool --global --dry-run
forge generate scaffold post title:string body:text published:bool --global
```

```text
  create  db/migrations/<timestamp>_create_posts.{up,down}.sql
  create  app/domain/post.go (+ _test)
  create  app/application/post.go (+ _test)
  create  app/adapters/persistence/post.go (+ _test)
  create  app/adapters/httpapi/post.go (+ _test)        JSON API at /v1/posts
  create  app/adapters/htmlui/post.go                   the HTML handlers
  create  app/adapters/htmlui/post_test.go
  create  app/views/posts/index.gohtml
  create  app/views/posts/show.gohtml
  create  app/views/posts/new.gohtml
  create  app/views/posts/edit.gohtml
  create  app/views/posts/_form.gohtml
  create  app/views/posts/not_found.gohtml
```

An application without `app/views/views.go` also gets the base `forge new`
generates (the views package, `layouts/application.gohtml`,
`shared/_flash.gohtml`). `forge generate view welcome` adds a single page,
`app/views/pages/welcome.gohtml` (or `DIR/NAME`), to the same tree.

Like every generator it refuses to overwrite a file and does not edit
`app/bootstrap/bootstrap.go`; it prints the wiring. For a PostgreSQL
application (Forge identity) the HTML part is:

```go
protectCookie := func(permission string, next http.Handler) (http.Handler, error) {
	required, err := auth.NewPermission(permission)
	// ...
	guarded, err := auth.RequireAudited(required, next, repository)
	// ...
	return middleware.AuthenticateBrowser("/session/new", guarded)
}

// forge new already builds the renderer for the sign-in pages; reuse it.
if err := htmlui.RegisterPosts(app, postService, renderer, protectCookie); err != nil {
	return err
}
```

For other databases the same deny-all `protect` guard as for the JSON routes
serves the pages until you write your own authentication and authorization.
The pages are never public: `RegisterPosts` refuses a missing guard, index and
show need `posts:read`, and new, edit, create, update and destroy need
`posts:write`. A `--global` scaffold keeps all of those writes (the forms
included) behind `protectPlatformWrite`, as the JSON routes do. The pages
authenticate the session cookie the sign-in page sets; a signed-out browser
is redirected to `/session/new` and back after signing in, while JSON clients
get the `401` error. The forms are
protected by the CSRF middleware from `bootstrap.go`; the command warns if
that file does not install `csrf.Protection`, `web.MethodOverride` and
`web.Flash`.

What the pages do:

| Request | Result |
| ------- | ------ |
| `GET /posts` | the newest posts, a "Next page" link (keyset cursor) |
| `GET /posts/new`, `GET /posts/{id}/edit` | the form (`_form.gohtml`): CSRF field, `_method=PATCH` and the stored `version` when editing |
| `POST /posts` | `303` to the new post with "Post was created."; invalid input re-renders the form with `422`, the submitted text and a message beside each field |
| `PATCH /posts/{id}` | `303` to the post with "Post was updated."; `422` as above; `409` when someone else changed the post since the form was loaded: the form is shown again with a link to the latest version and nothing is written |
| `DELETE /posts/{id}` | `303` to the index with "Post was deleted."; when the post changed since the page was shown it is kept and the browser returns to it with an alert |
| a missing or malformed id | the `404` page (`posts/not_found`); also for another tenant's row |

Fields become inputs by type: `string` a text input, `text` a textarea, `int`
a number input, `time` a `datetime-local` input, `uuid` a text input with a
UUIDv7 pattern, `bool` a checkbox. An optional field (`'body:text?'`) is not
`required` and may be left empty, which clears the stored value; an optional
`bool` is a not-set/yes/no select, because an unchecked box cannot say "not
set". Browser attributes are conveniences: the domain validates every value,
the form reports problems with the domain's own messages (the domain reports
the first invalid field per submission; conversion errors are all reported
together), and a request with a field the form does not have (`id`,
`tenant_id`, `version` on create) is a `400`, so nothing can be mass-assigned.

The generated `post_test.go` drives the pages through the same middleware the
application installs and the application's own templates, with an in-memory
repository: index, new, create (success and `422`), show, edit, update
(success, `422`, stale version), destroy (including a stale delete), `404`,
CSRF rejection for create, update and delete, unknown form fields, the
permission split and the refusal to register without a guard.

Edit the templates freely: they are production code in `app/views/posts/`.
Styles belong in `app/bootstrap/assets/application.css` (the page
Content-Security-Policy forbids inline styles); the rules the scaffold uses
(`table`, `.form-errors`, `.muted`, checkbox width) are part of the stylesheet
`forge new` generates today, and are plain CSS to copy into an older one.

## Example: posts

Scaffolded handlers follow this shape. The handler below implements every
action of a resource by hand. Register it with
`app.Resources`, which maps methods to routes:

| Method | Route | Handler |
| ------ | ----- | ------- |
| `GET` | `/posts` | `Index` |
| `GET` | `/posts/new` | `New` |
| `POST` | `/posts` | `Create` |
| `GET` | `/posts/{id}` | `Show` |
| `GET` | `/posts/{id}/edit` | `Edit` |
| `PATCH`, `PUT` | `/posts/{id}` | `Update` |
| `DELETE` | `/posts/{id}` | `Destroy` |

A handler may implement any subset; only those routes are registered.
`forge routes` lists them with the rest of the application's routes.

`app/adapters/htmlui/posts.go` (an in-memory store keeps the example short;
use an application service in real code):

```go
package htmlui

import (
	"errors"
	"net/http"
	"strings"
	"sync"

	forgeviews "github.com/rocketinsidecorp/forge-go/views"
	"github.com/rocketinsidecorp/forge-go/web"
)

type PostForm struct {
	Title     string `form:"title"`
	Published bool   `form:"published"`
}

type Posts struct {
	Views *forgeviews.Renderer

	mu    sync.Mutex
	posts []PostForm
}

type postsPage struct {
	Form   PostForm
	Errors web.FormErrors
	Posts  []PostForm
}

func (h *Posts) Index(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	page := postsPage{Posts: append([]PostForm(nil), h.posts...)}
	h.mu.Unlock()
	h.render(w, r, "posts/index", http.StatusOK, page)
}

func (h *Posts) New(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "posts/new", http.StatusOK, postsPage{})
}

func (h *Posts) Create(w http.ResponseWriter, r *http.Request) {
	var form PostForm
	err := web.DecodeForm(r, &form)
	var problems web.FormErrors
	switch {
	case errors.As(err, &problems):
	case err != nil:
		web.Error(w, r, err) // 400, 413 or 415
		return
	}
	if strings.TrimSpace(form.Title) == "" {
		problems.Add("title", "blank", "can't be blank")
	}
	if len(problems) > 0 {
		h.render(w, r, "posts/new", http.StatusUnprocessableEntity, postsPage{Form: form, Errors: problems})
		return
	}
	h.mu.Lock()
	h.posts = append(h.posts, form)
	h.mu.Unlock()
	if err := web.SetFlash(w, r, web.FlashNotice, "Post was created."); err != nil {
		web.Error(w, r, err)
		return
	}
	if err := web.Redirect(w, "/posts"); err != nil {
		web.Error(w, r, err)
	}
}

func (h *Posts) render(w http.ResponseWriter, r *http.Request, name string, status int, page postsPage) {
	if err := h.Views.RenderRequest(w, r, name, status, page); err != nil {
		web.Error(w, r, err)
	}
}
```

Wire it in `routes` in `app/bootstrap/bootstrap.go`, importing
`<module>/app/adapters/htmlui` and `<module>/app/views`:

```go
renderer, err := views.New()
if err != nil {
	return err
}
if err := app.Resources("/posts", &htmlui.Posts{Views: renderer}); err != nil {
	return err
}
```

`app/views/posts/index.gohtml`:

```html
{{define "title"}}Posts{{end}}
{{define "content"}}
<h1>Posts</h1>
<p><a href="/posts/new">New post</a></p>
<ul>
	{{range .Posts}}<li>{{.Title}}{{if .Published}} (published){{end}}</li>{{else}}<li>No posts yet.</li>{{end}}
</ul>
{{end}}
```

`app/views/posts/_form.gohtml`:

```html
<form method="post" action="{{.Action}}">
	{{csrfField}}
	{{with .Method}}{{methodField .}}{{end}}
	<div class="field">
		<label for="post_title">Title</label>
		<input id="post_title" name="title" value="{{.Page.Form.Title}}"
			{{if .Page.Errors.Has "title"}}aria-invalid="true" aria-describedby="post_title_error"{{end}}>
		{{with .Page.Errors.Message "title"}}<p class="field-error" id="post_title_error">{{.}}</p>{{end}}
	</div>
	<div class="field">
		<label><input type="checkbox" name="published" {{if .Page.Form.Published}}checked{{end}}> Published</label>
	</div>
	<button>Save</button>
</form>
```

`app/views/posts/new.gohtml`:

```html
{{define "title"}}New post{{end}}
{{define "content"}}
<h1>New post</h1>
{{template "posts/_form" (dict "Action" "/posts" "Method" "" "Page" .)}}
{{end}}
```

An edit page passes `"Method" "patch"` and the post's path as the action; a
delete button is a small form:

```html
<form method="post" action="/posts/{{.ID}}">
	{{csrfField}}
	{{methodField "delete"}}
	<button>Delete</button>
</form>
```

## Authentication pages

`forge new -d postgresql` generates the sign-in, sign-out, two-factor and
password-reset pages and wires them in `bootstrap.Configure`; in an older
PostgreSQL application, `forge generate authentication` adds the same files
and prints the wiring (other databases are refused: Forge persists identities
only in PostgreSQL). Create the first account with `forge setup`, then sign
in at `/session/new`:

```sh
forge migrate
forge setup --tenant acme --email you@example.com
forge dev     # http://localhost:8080/session/new
```

The handlers in `app/adapters/htmlauth` decode the forms, choose the
messages and render `sessions/*` and `passwords/*`; edit them like any
other page. `auth.Browser` does the security work (CSRF verification, the
session cookies, uniform failures, throttling with `429` and `Retry-After`,
the second-factor challenge cookie) and `auth.ReturnPath` keeps `return_to`
local. To show a sign-out button, include the partial on pages people see
while signed in:

```gotemplate
{{template "sessions/_sign_out" .}}
```

Protect your own pages with `middleware.AuthenticateBrowser(htmlauth.SignInPath, page)`
so signed-out browsers land on the sign-in page. The flow, its security
properties and `setup` are described in
[AUTHENTICATION.md](AUTHENTICATION.md#server-rendered-sign-in-pages) and
[ADR 0026](adr/0026-authentication-pages-and-setup.md).

## Forms

`web.DecodeForm(r, &form)` reads an `application/x-www-form-urlencoded`
body into a struct:

- Only exported fields tagged `form:"name"` are bound. Untagged fields can
  never be set from a request.
- Supported types: `string`, `bool`, signed and unsigned integers,
  `time.Time`, `uuid.UUID`, types implementing `encoding.TextUnmarshaler`,
  pointers to these, and `[]string` (repeated values, such as a multi-select).
- Every tagged field is assigned. An unchecked checkbox is absent and
  decodes to `false`; an absent or empty pointer field is `nil`; an empty
  non-pointer number, time or UUID is a `blank` field error.
- `time.Time` accepts HTML `date` and `datetime-local` values (read as UTC)
  and RFC 3339.
- Unknown fields and a repeated value for a scalar field are
  `400 invalid_form`. Bodies are limited to 1 MiB and 1000 fields
  (`413 body_too_large`, `400 invalid_form`) and the server's body limit
  still applies. Other content types are `415`; multipart bodies are not
  supported yet.
- Values that do not convert are returned together as `web.FormErrors`.
  Render the form again with `422` and `Errors.Message "field"`;
  `web.Error` answers a `FormErrors` with `422 invalid_fields`.
- The query string is never read, and the `_method` and
  `authenticity_token` fields are skipped.

## Redirects

`web.Redirect(w, "/posts")` answers `303 See Other`, which makes the browser
follow up a `POST`, `PATCH` or `DELETE` with a `GET`. Only local paths are
accepted: an absolute URL, `//host`, a backslash, or a control, space or
non-ASCII character returns `web.ErrUnsafeRedirect` and writes nothing, so a
`return_to` parameter cannot send users to another site. Check with
`web.IsLocalPath` before trusting such a value.

## Flash messages

```go
if err := web.SetFlash(w, r, web.FlashNotice, "Post was created."); err != nil { ... }
```

`SetFlash` must run before the response is written; the message is shown on
the next request and then cleared. Kinds are `web.FlashNotice`,
`web.FlashAlert` or any lowercase name; up to four messages of 512 bytes.
The cookie is `HttpOnly` and unsigned: anyone can read their own flash in the
browser's cookie store, so never put secrets or personal data in it.

## CSRF

Every form that changes state needs `{{csrfField}}`. Scripts send the token
from the `csrf-token` meta tag in the `X-CSRF-Token` header:

```js
fetch("/posts/1", {
	method: "DELETE",
	headers: { "X-CSRF-Token": document.querySelector('meta[name="csrf-token"]').content },
});
```

Unsafe requests are rejected with `403` when:

- the browser marks the request as coming from another origin (including a
  sibling subdomain), unless the origin is in `csrf.Options.TrustedOrigins`;
- the token is missing, repeated, malformed or does not match the
  `__Host-forge_csrf` cookie.

The token check does not apply to requests with an `Authorization: Bearer`
header or a JSON body, which cross-site pages cannot send, so JSON APIs keep
working; cookie-authenticated JSON routes still use `auth.RequireCSRF`.
Endpoints that receive form posts from other systems (webhooks) must
authenticate them by signature and be listed in `csrf.Options.Exempt`:

```go
protection, err := csrf.New(csrf.Options{Exempt: []string{"POST /webhooks/payments"}})
```

`GET`, `HEAD` and `OPTIONS` handlers must never change state: they are not
checked.

The CSRF and flash cookies use the `__Host-` prefix and therefore need
HTTPS. Browsers treat `http://localhost` and `http://127.0.0.1` as secure, so
`forge dev` works locally.

## Content negotiation

`web.PrefersHTML(r)` reports whether the client asked for HTML (browser
navigations and form submissions do; `curl`, `fetch()` defaults and API
clients do not). `web.Negotiate(r, "application/json", "text/html")` returns
the preferred offer. CSRF rejections use it to answer browsers with an HTML
page and API clients with the JSON error format.

## Testing pages

Run handlers through the same middleware the application installs:

```go
protection, _ := csrf.New(csrf.Options{})
handler := web.Flash(protection.Handler(http.HandlerFunc(posts.New)))
```

Read the token from the `__Host-forge_csrf` cookie of a `GET` response and
send it back with the cookie in the `authenticity_token` field. The
generated `app/bootstrap/bootstrap_test.go` shows the full cycle.
