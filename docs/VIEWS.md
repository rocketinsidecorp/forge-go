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
app/assets/assets.go                  embeds the stylesheet and images; builds the digested asset table and import map
app/assets/application.css            the stylesheet, served as /assets/application-<digest>.css
app/javascript/application.ts         front-end entry point (Turbo and the Stimulus controllers), TypeScript; application.js with --js
app/javascript/controllers/           Stimulus controllers: index.ts registers them, hello_controller.ts is an example
app/javascript/dist/                  the compiled JavaScript the binary embeds (generated, committed)
config/locales/locales.go             embeds the translations; locales.New(cfg, logger) builds the translator
config/locales/en.json                the text of the layout and the welcome page (forge new --locale adds es.json or pt-BR.json)
```

The pages' text is translated: see [Internationalization](I18N.md).

Static files and JavaScript are covered in
[Assets, JavaScript and Hotwire](#assets-javascript-and-hotwire).

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
config/locales/authentication.en.json  their text (and authentication.LOCALE.json for --locale)
```

`bootstrap.Configure` installs four middleware before routing, for every
route:

```go
translations, err := translator.Middleware(i18n.NegotiationOptions{})
...
protection, err := csrf.New(csrf.Options{})
...
for _, middleware := range []func(http.Handler) http.Handler{translations, web.MethodOverride, protection.Handler, web.Flash} {
	if err := app.Use(middleware); err != nil {
		return err
	}
}
```

- the translator's middleware picks the request's locale (cookie,
  `Accept-Language`, default) for `t`, `l`, `number` and `i18n.T` (see
  [Internationalization](I18N.md)).
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
| `{{csrfToken}}` | the token, masked afresh on every call (see [CSRF](#csrf)), for a `data-` attribute |
| `{{methodField "PATCH"}}` | `<input type="hidden" name="_method" value="PATCH">`; `PUT`, `PATCH` or `DELETE` only |
| `{{range flashes}}` | the incoming `web.FlashMessage` values (`.Kind`, `.Text`) |
| `{{flash "notice"}}` | the text of one incoming message, or empty |
| `{{domID "post" .ID}}` | `post_0190...`, or `new_post` for a zero UUID |
| `{{dict "Form" .Form "Action" "/posts"}}` | a map, for partials |
| `{{t "posts.index.title"}}` | the translation for the request's locale; `{{t "posts.count" "count" .Total}}` picks the plural form, `_html` keys are trusted markup with escaped arguments ([I18N.md](I18N.md)) |
| `{{l .CreatedAt "short"}}` | a time in the locale's `time.formats.short` (`"date.formats.long"` names any key) |
| `{{number .Total 2}}` | a number with the locale's separators; integers get no decimals unless a precision is given |
| `{{locale}}` | the request's locale, for `<html lang="{{locale}}">` |

Values are escaped for their context (HTML text, attribute, URL, CSS, JS) by
html/template. Do not convert user input to `template.HTML`, and do not add
functions that do.

### Rendering

Build the renderer once and share it; it is safe for concurrent use:

```go
renderer, err := views.New(views.WithI18n(translator)) // the application's app/views package
```

`translator` is the one `bootstrap.Configure` builds with `locales.New`;
pages translate for the request's locale, and the translator serves
fragments rendered without a request (broadcasts) in the default locale.

Render with the request so the CSRF and flash helpers work:

```go
err := renderer.RenderRequest(w, r, "posts/index", http.StatusOK, data)
```

The page is executed into a buffer first, so a template error never sends a
partial response. Pages are sent with
`views.DefaultContentSecurityPolicy`, which allows same-origin scripts,
styles, images and fonts, forms posting to the same origin, and nothing
inline: put styles in `app/assets/` files, not in `style=` attributes or
`<style>` elements. The one inline script a page needs, the import map, is
allowed by its SHA-256 hash (never `'unsafe-inline'`): `Load` adds the hash to
the policy.

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
`app/bootstrap/bootstrap.go`; it mounts the routes in
`app/bootstrap/routes_gen.go` (docs/GENERATORS.md, "The routes seam"), or
prints the wiring in an application generated before that file existed. For
a PostgreSQL application (Forge identity) the HTML part is:

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
Styles belong in `app/assets/application.css` (the page
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
renderer, err := views.New(views.WithI18n(translator))
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

Pages never embed the token itself. `{{csrfField}}`, `{{csrfMeta}}` and
`{{csrfToken}}` (all through `csrf.Token`) write it masked, as Rails does:
a fresh 32-byte one-time pad followed by the token XOR the pad, 86
characters of base64url. Every render embeds different bytes, so a page
compressed together with reflected input cannot leak the token through its
size (BREACH). The middleware and `auth.RequireCSRF` unmask what they receive
and compare it with the cookie in constant time. They also accept the token
unmasked: scripts that read the cookie send it so (cookies are not part of a
compressed body), and forms rendered before the upgrade to masked tokens
carry it, so pages open during a deploy keep working. Accepting the unmasked
form in `authenticity_token` is a transition for one release;
`csrf.MaskToken` and `csrf.UnmaskToken` are there for custom renderers.

TODO(v0.2.0): stop accepting an unmasked token in the `authenticity_token`
form field. Masked tokens first ship in v0.1.0 (no release has been tagged
yet), so v0.2.0 is the release after the one-release transition. The
`X-CSRF-Token` header keeps accepting the unmasked cookie value, which
scripts read from the cookie.

### Upgrading: tests that compare the embedded token with the cookie

Applications generated before masked tokens have two tests that compare the
token a page embeds with the `__Host-forge_csrf` cookie byte for byte, and
fail after the upgrade although nothing is broken:

- `app/views/views_test.go`, `TestLayoutRendersFlashAndCSRF`: it expects
  `<meta name="csrf-token" content="` + cookie + `">` and
  `<input type="hidden" name="authenticity_token" value="` + cookie + `">`;
- `app/bootstrap/bootstrap_test.go`, `TestBrowserPipelineRequiresCSRFToken`:
  it fails when `cookies[0].Value != form.Body.String()`.

Compare with `csrf.Match(cookieToken, embedded)` instead. It reports whether
`embedded`, masked or not, carries the cookie's token (constant time):

```go
// bootstrap_test.go, TestBrowserPipelineRequiresCSRFToken
if form.Code != http.StatusOK || len(cookies) != 1 || cookies[0].Name != csrf.CookieName ||
	cookies[0].Value == form.Body.String() || !csrf.Match(cookies[0].Value, form.Body.String()) {
	t.Fatalf("form status=%d cookies=%v body=%q", form.Code, cookies, form.Body.String())
}
```

```go
// views_test.go, TestLayoutRendersFlashAndCSRF: drop the two token lines
// from the list of wanted fragments, then check what the page embeds.
meta := embeddedValue(body, `<meta name="csrf-token" content="`)
field := embeddedValue(body, `<input type="hidden" name="authenticity_token" value="`)
if !csrf.Match(token, meta) || !csrf.Match(token, field) || meta == token || field == token {
	t.Fatalf("the embedded CSRF tokens do not carry the cookie token masked: meta=%q field=%q", meta, field)
}

// embeddedValue is the attribute value that follows prefix in body.
func embeddedValue(body, prefix string) string {
	_, rest, found := strings.Cut(body, prefix)
	if !found {
		return ""
	}
	value, _, _ := strings.Cut(rest, `"`)
	return value
}
```

A new application from `forge new` already has these assertions.

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

## Assets, JavaScript and Hotwire

Forge is full-stack like Rails 8 without Node. Static files are embedded in
the binary, JavaScript is loaded through an import map, and Turbo and Stimulus
are vendored by Forge ([ADR 0029](adr/0029-hotwire-assets-and-live-updates.md)).

### Digested assets

`assets.New(assets.Options{}, sources...)` reads `fs.FS` values (embedded
files) once at startup into a table of logical names to digested paths:
`application.css` is served as `/assets/application-<16 hex of SHA-256>.css`
with `Cache-Control: public, max-age=31536000, immutable`, an `ETag` (the full
digest), `X-Content-Type-Options: nosniff` and the right `Content-Type`. Only
table entries are served: undigested names, directory listings (`/assets/`),
`..` and encoded separators are `404`. A name the table does not contain
cannot be linked: `assetPath` and the tag helpers fail `views.Load`.

```go
bundle, _ := appassets.Load()                      // app/assets: table + import map
app.Handle("GET /assets/", bundle.Set.Handler()) // bootstrap.go does this
```

In templates:

| Helper | Output |
| ------ | ------ |
| `{{assetPath "forge-go-logo.png"}}` | `/assets/forge-go-logo-<digest>.png` |
| `{{stylesheetTag "application"}}` | `<link rel="stylesheet" href="...">` |
| `{{javascriptImportmapTags "application"}}` | the import map, `<link rel="modulepreload">` for each pinned module, and `<script type="module" src="...application-<digest>.js">` |

To publish another file, add its name to the `//go:embed` line in
`app/assets/assets.go` (only extensions Forge serves are accepted: css, js,
mjs, map, svg, png, jpg, jpeg, gif, webp, ico, woff2, txt).

### JavaScript: import map, Turbo and Stimulus

The front end is TypeScript by default ([ADR 0049](adr/0049-typescript-by-default.md);
`forge new --js` keeps plain JavaScript, with `.js` where this page says `.ts`
and no `dist`). `app/javascript/application.ts` is the entry point:

```ts
import "@hotwired/turbo"
import "controllers"
```

There is no bundler and no Node toolchain. `forge dev`, `forge build`,
`forge test` and `forge check` compile each `.ts` file alone with esbuild's Go
API, which only strips the types, into `app/javascript/dist` (generated and
committed: the application's `javascript` package embeds it, so a plain
`go build` as in the generated Dockerfile needs nothing else, and a generated
test fails when a source changed and `dist` did not, or when a compiled file
is not the one `forge` wrote: its header records the SHA-256 of the source and
of its own body; review `dist/` diffs like code). Everything under `dist/` is
embedded; only `application.js` and `controllers/*` are pinned in the import
map, so a file such as `lib/format.ts` needs `forgeassets.PinAllFrom(set, "lib")`
in `app/assets/assets.go` to be imported by name. The browser resolves
bare specifiers through the import
map the layout renders, which `app/assets/assets.go` builds: `@hotwired/turbo`
and `@hotwired/stimulus` (the vendored files in package `hotwire`, pinned by
SHA-256 and verified by its test, with their MIT licences retained) and every
file of `dist/controllers/` by its name without `.js`
(`controllers/hello_controller`; `controllers/index.js` is also `controllers`).
Digested URLs mean a deploy invalidates exactly the files that changed.

Types are checked by `tsc --noEmit` (`forge check` runs it when `tsc` is on
`PATH`, and prints one note saying how to install it when not:
`npm install --global --ignore-scripts typescript@6.0.3`). `tsconfig.json` is strict, resolves
the import map's bare names with `paths`, and finds the declarations of Turbo
and Stimulus in `app/javascript/types/` (copied unchanged from the npm
packages; `types/README.md` has the provenance).

Add a Stimulus controller:

```sh
cp app/javascript/controllers/hello_controller.ts app/javascript/controllers/clipboard_controller.ts
```

then in `app/javascript/controllers/index.ts`:

```ts
import ClipboardController from "controllers/clipboard_controller"
application.register("clipboard", ClipboardController)
```

and run `forge build` (`forge dev` compiles on save, with inline source maps,
without rewriting `dist` in your working tree), then use
`data-controller="clipboard"` in a template. Write controllers with
`textContent`, not `innerHTML`, for user input. Inline `<script>` blocks and
`onclick=` attributes are blocked by the policy by design.

The import map is the only inline script. Its SHA-256 goes into
`script-src` automatically (`views.Options.ImportMap`), and Turbo's progress
bar `<style>` is allowed by the hash of its exact text
(`views.Options.StyleHashes`, `hotwire.StyleHashes()`; a test recomputes it
from `turbo.js`). A custom `Options.ContentSecurityPolicy` replaces the default
entirely: include `ImportMap.CSPSource()` yourself.

Turbo Drive works with the pages unchanged: forms answer `303` after success
and `422` (or `409`) with the form re-rendered on failure, which is what Turbo
requires; the CSRF token reaches Turbo through the `csrf-token` meta tag it
sends as `X-CSRF-Token`. Pages that are complete documents with their own
inline styles (the welcome page) declare
`<meta name="turbo-visit-control" content="reload">` so Turbo loads them with
a normal page visit.

### Turbo Streams

Package `turbo` builds `<turbo-stream>` elements: `Append`, `Prepend`,
`Replace`, `Update`, `Remove`, `Before`, `After`. The target is a validated
DOM id (`[A-Za-z][A-Za-z0-9_:-]*`, at most 128 bytes: it cannot inject
attributes) and the content is `template.HTML` from `html/template`, so user
data stays escaped. `turbo.DomID("post", id)` is the same id the `domID`
helper prints. `Renderer.Fragment` renders a partial (a file named `_NAME.gohtml`)
as stream content:

```go
row, err := renderer.Fragment(r, "posts/_post", item)
stream, err := turbo.Prepend("posts", row)
if turbo.Requested(r) { // Accept names text/vnd.turbo-stream.html
	return turbo.Render(w, http.StatusOK, stream)
}
return web.Redirect(w, "/posts") // browsers without Turbo, and API clients
```

`turbo.Requested` ignores wildcard `Accept` values: only a client that asks for
the stream type gets one. Respond to a failed form with `422` and the form
HTML (as the scaffold does) or a stream with `turbo.Render(w, 422, ...)`.

### Real-time broadcasts (Server-Sent Events)

Package `broadcast` pushes streams to every open page that subscribed, like
Rails' `Turbo::StreamsChannel` and `turbo_stream_from`:

```go
signer, _ := broadcast.NewSigner(broadcast.SignerOptions{Key: key}) // 32+ random bytes, same on every replica
broker, _ := broadcast.NewMemory(ctx, broadcast.MemoryOptions{})    // ctx: Configure's, cancelled when shutdown begins
events, _ := broadcast.NewHandler(broadcast.HandlerOptions{
	Broker: broker, Signer: signer,
	Authorize: func(r *http.Request, ch broadcast.Channel) error { /* a permission check */ return nil },
})
app.Handle("GET /streams", authenticate(events))
app.OnShutdown(broker.Shutdown)

renderer, _ := views.Load(files, views.Options{Streams: signer /* ... */})
```

A page subscribes with `{{turboStreamFrom "posts"}}`, which renders
`<turbo-stream-source src="/streams?stream=TOKEN">` for the request tenant's
`posts` channel (it fails the render when there is no tenant), or
`{{turboStreamFromGlobal "banner"}}` for a channel shared by every tenant.
Server code publishes after the change is stored:

```go
channel, _ := broadcast.ForTenant(tenant.ID, "posts") // or broadcast.Global
err := broadcast.Publish(ctx, broker, channel, stream)
```

- **Authorization.** The token is an HMAC-SHA256 signature over the channel and
  an expiry (24 hours by default, 7 days at most; keys rotate with
  `PreviousKeys`). A client cannot subscribe to a channel the server did not
  render for it. A tenant channel also requires the caller's tenant
  (`tenancy.FromContext`, set by the authentication middleware you mount the
  handler behind) to equal the channel's: it fails closed with `403`.
  `Authorize` adds your rule. Connections are re-authorized every
  `MaxConnectionAge` (30 minutes): the stream ends and the browser reconnects.
- **What is broadcast is the same HTML for every subscriber.** Render it with
  `Fragment`: it cannot render `csrfField`, so a token never leaks into a
  broadcast. Put nothing per-user in a broadcast partial.
- **Delivery** is at most once, with no replay: a subscriber that falls behind
  its bounded queue is dropped (the browser reconnects), and a message
  published while a client is disconnected is not delivered.
- **Resources are bounded:** subscribers per process and per channel,
  per-subscriber queue, message size (1 MiB); the in-memory broker starts no
  goroutines. Excess connections get `503` with `Retry-After`.
- **Timeouts.** The handler clears the server's read and write deadlines per
  connection (`http.ResponseController`) and applies a 10 second deadline to
  every write, so streams outlive `FORGE_HTTP_WRITE_TIMEOUT` while a stalled
  client cannot hold a connection (the deadline is cleared after each flush, so
  idle HTTP/2 streams are not reset). Open streams are capped per principal (16)
  and per tenant (1000) with `429`; proxy limits must count concurrent
  requests, not connections, because HTTP/2 multiplexes streams over one
  connection. Heartbeat comments (20 seconds) keep
  proxies from closing idle streams; disable proxy buffering for the route
  (`X-Accel-Buffering: no` is sent).
- **Shutdown.** Closing the broker ends open streams, which lets the HTTP
  server drain. `NewMemory(ctx, ...)` closes itself when `ctx` is cancelled.

#### Several replicas: PostgreSQL LISTEN/NOTIFY

The in-memory broker reaches only its own process. `broadcast/postgres` fans
messages out through the database:

```go
local, _ := broadcast.NewMemory(ctx, broadcast.MemoryOptions{})
broker, _ := bcpostgres.New(ctx, database, local, bcpostgres.Options{})
app.OnShutdown(broker.Shutdown)
// events handler: broadcast.NewHandler(broadcast.HandlerOptions{Broker: broker, ...})
```

`Publish` delivers to local subscribers immediately and sends a `NOTIFY`
for the others; `PublishTx(ctx, tx, ...)` queues the notification in your
transaction, so it is delivered only if the transaction commits. `NOTIFY`
payloads are limited to 8000 bytes, so a message is split into chunks of 4500
bytes sent as consecutive notifications of one transaction and reassembled by
the listener (messages are limited to 256 KiB by default; send something
smaller, or have the client reload, for anything bigger). The listener holds
one pool connection, reconnects with backoff (messages sent meanwhile are
lost), does not work through PgBouncer in transaction pooling mode, and
ignores malformed notifications. The channel is database-wide: anyone who can
`NOTIFY` on the database can publish, so the database role is the trust
boundary.

### Live updates in the scaffold

```sh
forge generate scaffold post title:string body:text --tenant-scoped --broadcast
```

`--broadcast` is opt-in: it opens a long-lived endpoint and needs a signing
key, so it is not implied by `scaffold`. It adds:

```text
app/adapters/htmlstreams/streams.go       signer from FORGE_STREAMS_KEY, in-memory broker, GET /streams (once per application)
app/adapters/htmlstreams/streams_test.go  tenant, forged name, permission and delivery tests
app/views/posts/_item.gohtml              the table row, rendered by the index and by broadcasts
```

and changes the index (a `turboStreamFrom` subscription, `<tbody id="posts">`)
and the adapter: after a successful create, update or delete it publishes a
`prepend`, `replace` or `remove` of the row to the `posts` channel of the
caller's tenant (the global channel for `--global`). Publishing never fails the
request. The printed wiring defines the signer and mounts `/streams` behind
`AuthenticateCookie` with a per-channel permission check (`posts:read` for the
`posts` channel); without Forge identity both are deny-all until you write
them. Set `FORGE_STREAMS_KEY` (`openssl rand -base64 32`) in staging and
production; development falls back to a random per-process key.

### Rotating the stream signing key

Changing `FORGE_STREAMS_KEY` alone invalidates the stream name of every page
that is already open: its next reconnect gets `403 stream_invalid` and stops
updating until the page is reloaded. To rotate without that, move the old key
to `FORGE_STREAMS_PREVIOUS_KEYS` (comma-separated, at most 3, same format).
Previous keys only verify, never sign; the current key signs. Do it in two
steps:

```sh
OLD_STREAMS_KEY="$FORGE_STREAMS_KEY"              # the key every replica uses now
NEW_STREAMS_KEY="$(openssl rand -base64 32)"      # keep both somewhere safe
# step 1: every replica learns the new key, but still signs with the old one
export FORGE_STREAMS_KEY=$OLD_STREAMS_KEY
export FORGE_STREAMS_PREVIOUS_KEYS=$NEW_STREAMS_KEY
# redeploy every replica, then
# step 2: sign with the new key; the old one keeps verifying pages already open
export FORGE_STREAMS_KEY=$NEW_STREAMS_KEY
export FORGE_STREAMS_PREVIOUS_KEYS=$OLD_STREAMS_KEY
# redeploy every replica; after the token lifetime (24 hours by default, up to
# 7 days with broadcast.SignerOptions.TTL) unset FORGE_STREAMS_PREVIOUS_KEYS and redeploy
```

Two steps keep a rolling redeploy safe: after step 1 every replica already
accepts the new key, so a page whose stream name was signed by a replica that
finished step 2 keeps updating when it reconnects to one that has not. Going
straight to step 2 would make the replicas still on the old configuration
answer `403 stream_invalid` to those pages.

The generated `htmlstreams.NewSigner` reads both variables
(`broadcast.DecodePreviousKeys` parses the list); neither value is ever echoed in an error.

**Upgrading an application generated before `FORGE_STREAMS_PREVIOUS_KEYS`.**
The generator writes `app/adapters/htmlstreams/streams.go` once, so an older
application keeps a three-argument `NewSigner(environment, key, logger)`, and
the wiring printed by a later `forge generate scaffold --broadcast` (four
arguments) does not compile against it. Either keep calling the old signature
(rotation then needs the old key added by hand), or update `streams.go`:

```go
// PreviousKeysEnvironment names the variable bootstrap reads the previous
// signing keys from while FORGE_STREAMS_KEY is rotated.
const PreviousKeysEnvironment = "FORGE_STREAMS_PREVIOUS_KEYS"

func NewSigner(environment config.Environment, key, previousKeys string, logger interface{ Warn(string, ...any) }) (*broadcast.Signer, error) {
	previous, err := broadcast.DecodePreviousKeys(previousKeys)
	if err != nil {
		return nil, errors.New(PreviousKeysEnvironment + " is not valid: " + strings.TrimPrefix(err.Error(), "broadcast: "))
	}
	if key != "" {
		return broadcast.NewSignerFromBase64(key, broadcast.SignerOptions{PreviousKeys: previous})
	}
	if previous != nil {
		return nil, errors.New(PreviousKeysEnvironment + " needs " + KeyEnvironment + ": the previous keys only verify, the current key signs")
	}
	// ... the rest of the function is unchanged (production and staging
	// refuse an empty key; development uses a random one).
}
```

then pass `os.Getenv(htmlstreams.PreviousKeysEnvironment)` as the new third
argument in `bootstrap.go` and add `""` to the calls in `streams_test.go`
(`import "strings"` as well).

Rows are prepended, so a person on page two of the list sees new rows at the
top of that page; reload to page.

## Testing pages

Run handlers through the same middleware the application installs:

```go
translator, _ := locales.New(config.Default(), nil)
translations, _ := translator.Middleware(i18n.NegotiationOptions{})
protection, _ := csrf.New(csrf.Options{})
handler := translations(web.Flash(protection.Handler(http.HandlerFunc(posts.New))))
```

Fail the test when a page shows `translation missing`, as the generated
tests do.

Read the token from the `__Host-forge_csrf` cookie of a `GET` response and
send it back with the cookie in the `authenticity_token` field. The
generated `app/bootstrap/bootstrap_test.go` shows the full cycle.
