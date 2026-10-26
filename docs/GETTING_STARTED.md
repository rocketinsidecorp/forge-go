# Getting started with Forge

This guide builds a small blog from `forge new` to a deployable application:
server-rendered pages, sign-in, live updates, translations, email, a
background job, an OpenAPI document and tests. It is the Forge version of the
Rails "Getting Started" guide, and every command in it was run against this
repository.

Forge is a Go framework with Rails conventions, so the names carry over:

| Rails | Forge |
| ----- | ----- |
| `rails new blog` | `forge new blog` |
| `bin/rails server` | `forge dev` (rebuilds and reloads the browser on save) |
| `bin/rails db:prepare`, `db:migrate` | `forge db prepare`, `forge migrate` |
| `rails g scaffold`, `g model`, `g controller`, `g mailer`, `g job` | `forge generate scaffold`, `model`, `controller`, `mailer`, `job` |
| `rails d ...` | `forge destroy ...` |
| `rails routes` | `forge routes` |
| `rails test` | `forge test` |
| `rails credentials:edit` | `forge credentials edit` |
| Kamal | `forge deploy` |

One difference to know up front: **generators never edit
`app/bootstrap/bootstrap.go`**, your composition root. They print the code to
add and where, and you paste it. That is deliberate (the file is yours), and
this guide shows each paste in full.

Contents: [1 Prerequisites](#1-prerequisites) ·
[2 Install the CLI](#2-install-the-cli) ·
[3 Create the application](#3-create-the-application) ·
[4 Set up and run it](#4-set-up-and-run-it) ·
[5 Scaffold posts](#5-scaffold-posts) ·
[6 Comments: a model and a controller](#6-comments-a-model-and-a-controller) ·
[7 Authentication](#7-authentication) ·
[8 Translations](#8-translations) ·
[9 Email](#9-email) ·
[10 Background jobs](#10-background-jobs) ·
[11 OpenAPI, tests and diagnostics](#11-openapi-tests-and-diagnostics) ·
[12 Credentials](#12-credentials) ·
[13 Observability](#13-observability) ·
[14 Deploy](#14-deploy) ·
[15 Undo with `forge destroy`](#15-undo-with-forge-destroy) ·
[16 Where to go next](#16-where-to-go-next)

## 1. Prerequisites

- **Go 1.27.3 or newer.** `go version` must say so.
- **A database.** PostgreSQL 18 is the default and the only database with
  sign-in and tenants, so this guide uses it. The easiest way to get one is
  Docker (with Compose): the generated `compose.yaml` starts it. Without
  Docker, any PostgreSQL 18 server whose user may create databases works.
  SQLite needs nothing installed ([see below](#sqlite-instead-of-postgresql)).
- `curl` and `openssl` for a few commands, and the `psql` client for `forge db
  console` (section 11).

Docker is also what builds the production image (`docker build`, `forge
deploy`) and runs the observability stack; none of the rest needs it.

## 2. Install the CLI

```sh
go install github.com/rocketinsidecorp/forge-go/cmd/forge@latest
forge help            # the command list
```

`go install` puts `forge` in `$(go env GOPATH)/bin` (usually `~/go/bin`); make
sure that directory is on your `PATH`.

## 3. Create the application

```sh
forge new blog
cd blog
```

`forge new` writes the application, runs `go mod tidy` and prints the next
steps. Choose another database with `-d mysql`, `-d mariadb` or `-d sqlite`,
and the default language of the pages with `--locale en|es|pt-BR`.

```text
blog/
  cmd/blog/main.go            the binary: forge.Main(...) plus migrate, seed and setup commands
  app/bootstrap/              the composition root: opens the database, wires routes (yours to edit)
  app/adapters/htmlauth/      sign-in, sign-out, two-factor and password-reset pages
  app/views/                  layout, flash partial and the sign-in pages (Go html/template)
  app/assets/, app/javascript/ digested static files; Turbo and Stimulus, no Node toolchain
  config/locales/             translations, one JSON file per locale
  config/credentials/         encrypted credentials (the .key file is git-ignored)
  db/migrations/              SQL migrations, embedded in the binary; db/seeds.go
  .env.development            committed development settings (no secrets)
  .env.local                  your private overrides (git-ignored)
  Dockerfile, compose.yaml    distroless production image; PostgreSQL + migrate + app
  compose.observability.yaml, observability/   opt-in Grafana, Prometheus, Tempo and Loki
  .github/workflows/ci.yml    gofmt, vet, test -race, govulncheck, docker build
```

The code you will add later follows the hexagonal layout: `app/domain` (rules),
`app/application` (use cases and ports), `app/adapters` (HTTP and persistence),
and `app/bootstrap` (the only place that picks implementations).
[ARCHITECTURE.md](ARCHITECTURE.md) explains why.

## 4. Set up and run it

Start PostgreSQL, then create the database, apply the migrations and load the
seed data in one step:

```sh
docker compose up -d postgres
forge db prepare
```

```text
created postgres database "blog_development" on 127.0.0.1:27521
...
applied  20260927090000_identity
...
seed data loaded
```

The port (`27521`) is derived from the application name so it does not clash
with a PostgreSQL on your machine; `.env.development` has the URL. To use a
server you already run instead of the Compose one, put its URL in `.env.local`
and run `forge db prepare` against it (it creates the database if the user may,
so `docker compose up` is not needed):

```sh
echo 'FORGE_DATABASE_URL=postgres://you:secret@127.0.0.1:5432/blog_development?sslmode=disable' >> .env.local
echo 'FORGE_MIGRATION_DATABASE_URL=postgres://you:secret@127.0.0.1:5432/blog_development?sslmode=disable' >> .env.local
echo 'FORGE_TEST_DATABASE_URL=postgres://you:secret@127.0.0.1:5432/postgres?sslmode=disable' >> .env.local
forge db prepare
```

The Compose database serves as `app_runtime` and migrates as `app_migrator`
(`.env.development`), two roles its `docker/postgres/init.sql` creates; with
your own server one user can do both during development. In production keep
them apart ([Database roles](DATABASE.md#database-roles-postgresql)): in
staging and production the application refuses to start as a superuser.

Then create the first organization, tenant and owner account. The command
prompts twice for the password (12 or more characters) and never takes it as an
argument:

```sh
forge setup --tenant acme --email you@example.com --organization "Acme Inc"
```

```text
Created organization "Acme Inc" with tenant "acme".
Created owner you@example.com with the "owner" role (0 permissions).
Sign in at /session/new with tenant "acme".
```

Run the application:

```sh
forge dev
```

```text
forge dev 15:18:38: watching .; open http://127.0.0.1:8080 (browser reload on)
forge dev 15:18:38: building
time=... level=INFO msg="starting HTTP server" address=127.0.0.1:41685 environment=development transport=plain
forge dev 15:18:39: running
```

Open <http://127.0.0.1:8080/> for the welcome page. In another terminal:

```sh
curl http://127.0.0.1:8080/v1/status        # {"service":"blog","status":"ok"}
curl http://127.0.0.1:8080/health/ready     # {"healthy":true,"checks":[{"name":"database","healthy":true}]}
```

Leave `forge dev` running for the rest of the guide. It watches Go files,
templates, assets, migrations and `.env.*`, rebuilds, swaps the process once
the new build works, and reloads the page. A build error is shown over the
page and the old build keeps serving. (`forge dev --no-reload` builds and runs
once. `docker compose up --build` runs the production image instead.)

Run the generated tests once to see a green baseline. They create and drop
their own databases on the server named by `FORGE_TEST_DATABASE_URL`:

```sh
forge test
```

### SQLite instead of PostgreSQL

```sh
forge new blog -d sqlite
cd blog
forge db prepare        # creates storage/development.db
forge dev
```

Generators, tests, views, translations, OpenAPI and `forge destroy` work the
same. What SQLite does not have is Forge identity: no `forge setup`, no sign-in
pages (`forge generate authentication` is refused) and nothing that supplies a
tenant, so scaffold with `--global` instead of `--tenant-scoped`. A scaffold's
pages and its `--broadcast` stream refuse everyone until you write the guard
the printed wiring asks for; its placeholder is deny-all. For local
experiments only, replace the body of that `protect` function with
`return next, nil` (every request passes) and pages such as `/posts` work;
never deploy that.
Sections 5 (without sign-in), 8, 11, 12 and 15 apply; the rest of this guide was run
on PostgreSQL.

## 5. Scaffold posts

```sh
forge generate scaffold post title:string body:text --tenant-scoped --broadcast
```

```text
  create  db/migrations/20261020151843_create_posts.up.sql
  create  db/migrations/20261020151843_create_posts.down.sql
  create  app/domain/post.go
  ...
  create  app/adapters/htmlui/post.go
  create  app/adapters/htmlstreams/streams.go
  create  app/adapters/htmlstreams/streams_test.go
  create  config/locales/posts.en.json
  create  app/views/posts/index.gohtml
  ...
```

This is `rails g scaffold`: a migration, the domain entity, use cases, a
PostgreSQL repository, a JSON API (`/v1/posts`), HTML pages (`/posts`), forms
with CSRF tokens, a translation file and tests for every layer.

- `post` is singular `snake_case`; fields are `name:type` with `type` one of
  `string`, `text`, `int`, `bool`, `time`, `uuid`; a trailing `?` (quoted:
  `'subtitle:string?'`) makes a field optional.
- `--tenant-scoped` binds every query to the signed-in organization and, on
  PostgreSQL, enforces it again with row-level security. `--global` is for
  data every tenant shares.
- `--broadcast` adds live updates over Server-Sent Events (section 5.2).
- Add `--dry-run` to any generator to list its files without writing them.

### 5.1 Wire it into the application

The generator prints exactly what to paste into `app/bootstrap/bootstrap.go`.
All edits are inside `identityRoutes` unless noted.

**a. Imports.** Add each one to the group it belongs to (standard library,
`github.com/rocketinsidecorp/forge-go/...`, then `blog/...`):

```go
"os"
"time"
"github.com/rocketinsidecorp/forge-go/broadcast"
"github.com/rocketinsidecorp/forge-go/uuid"
"blog/app/adapters/htmlstreams"
"blog/app/adapters/htmlui"
"blog/app/adapters/httpapi"
"blog/app/adapters/persistence"
"blog/app/application"
```

**b. The stream signer**, just before the line `renderer, err := views.New(...)`,
and give the renderer the signer so pages can sign their stream names:

```go
	streamSigner, err := htmlstreams.NewSigner(app.Config().Environment, os.Getenv(htmlstreams.KeyEnvironment), app.Logger())
	if err != nil {
		return err
	}
	renderer, err := views.New(views.WithI18n(translator), views.WithStreams(streamSigner))
```

**c. The guards and the event stream**, right after the existing
`middleware, err := auth.NewMiddleware(repository)` statement and its `if err
!= nil` block. `protect` guards the JSON API (bearer token), `protectCookie`
guards the pages (session cookie; signed-out browsers are sent to the sign-in
page and back), and the stream is readable only with the permission of the
resource it carries:

```go
	protect := func(permission string, next http.Handler) (http.Handler, error) {
		required, err := auth.NewPermission(permission)
		if err != nil {
			return nil, err
		}
		guarded, err := auth.RequireAudited(required, next, repository)
		if err != nil {
			return nil, err
		}
		return middleware.Authenticate(guarded), nil
	}
	protectCookie := func(permission string, next http.Handler) (http.Handler, error) {
		required, err := auth.NewPermission(permission)
		if err != nil {
			return nil, err
		}
		guarded, err := auth.RequireAudited(required, next, repository)
		if err != nil {
			return nil, err
		}
		return middleware.AuthenticateBrowser("/session/new", guarded)
	}
	streams, err := htmlstreams.Register(ctx, app, streamSigner, htmlstreams.Options{
		Authenticate: middleware.AuthenticateCookie,
		Authorize: func(r *http.Request, channel broadcast.Channel) error {
			principal, ok := auth.FromContext(r.Context())
			if !ok {
				return auth.ErrCredentialsRequired
			}
			required, err := auth.NewPermission(channel.Name + ":read")
			if err != nil || !principal.Can(required) {
				return auth.ErrPermissionDenied
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
```

**d. The resource**, right after that:

```go
	postService, err := application.NewPostService(persistence.NewPostRepository(database), time.Now, uuid.New)
	if err != nil {
		return err
	}
	if err := httpapi.RegisterPosts(app, postService, protect); err != nil {
		return err
	}
	if err := htmlui.RegisterPosts(app, postService, renderer, protectCookie, htmlui.WithPostBroadcast(streams.Broker)); err != nil {
		return err
	}
```

Format the file, give live updates a signing key, apply the migration and
look at the routes:

```sh
gofmt -w app/bootstrap/bootstrap.go
echo "FORGE_STREAMS_KEY=$(openssl rand -base64 32)" >> .env.local
forge migrate
forge routes | grep posts
```

`forge dev` rebuilds by itself when `bootstrap.go` and `.env.local` change.
`forge migrate` applies `create_posts`, which also registers the permissions
`posts:read` and `posts:write` and grants them to the `owner` role that
`forge setup` created.

Open <http://localhost:8080/posts>. Use `localhost`, not `127.0.0.1`: the
session cookies are `__Host-` cookies, which browsers only accept from a secure
context, and `http://localhost` is one. You are sent to the sign-in page; sign
in with organization `acme`, your email and password, click **New post**, fill
in a title and a body, and save. A blank title shows the form again with `422`
and the message beside the field, and a stale edit shows `409` instead of
overwriting someone else's change.

Prefer the terminal? The same flow with `curl` (the password is read without
echo; every form needs the CSRF token from the page):

```sh
B=http://localhost:8080 JAR=$(mktemp)
token() { sed -n 's/.*name="authenticity_token" value="\([^"]*\)".*/\1/p' | head -1; }
printf 'Owner password: '; read -rs OWNER_PASSWORD; echo   # bash or zsh
T=$(curl -s -c $JAR -b $JAR $B/session/new | token)
curl -s -o /dev/null -c $JAR -b $JAR -d "authenticity_token=$T" -d tenant=acme -d email=you@example.com --data-urlencode "password=$OWNER_PASSWORD" $B/session
T=$(curl -s -c $JAR -b $JAR $B/posts/new | token)
curl -si -c $JAR -b $JAR -d "authenticity_token=$T" --data-urlencode "title=Hello Forge" --data-urlencode "body=My first post" $B/posts | grep -E '^(HTTP|Location)'
curl -s -b $JAR $B/posts | grep '<td>Hello'
unset OWNER_PASSWORD
```

```text
HTTP/1.1 303 See Other
Location: /posts/01a159e5-6973-736a-bb43-a3931d325457
	<td>Hello Forge</td>
```

### 5.2 Live updates

Open `/posts` in two browser windows, signed in to the same organization.
Create, edit or delete a post in one and the other changes without a reload:
the server pushes Turbo Streams over Server-Sent Events, on channels whose
names are signed and tenant-scoped. Delivery is best effort and at most once,
so the next page load is always authoritative. A single process uses the
in-memory broker the wiring builds; with several replicas use
`broadcast/postgres` ([VIEWS.md](VIEWS.md#real-time-broadcasts-server-sent-events)).
Set `FORGE_STREAMS_KEY` to the same value on every replica in staging and
production (`openssl rand -base64 32`); development falls back to a random key
per process and logs a warning without one.

### 5.3 Test

```sh
forge test
```

The scaffold's tests cover validation, optimistic locking, the permission
checks, CSRF, tenant isolation (including with row-level security forced on
the table's owner) and an end-to-end event stream.

## 6. Comments: a model and a controller

A **model** is the data and its rules without any HTTP; a **controller** is
read-only pages without any data. Generate the model first:

```sh
forge generate model comment post_id:uuid body:text --tenant-scoped
forge migrate
forge test
```

```text
  create  db/migrations/20261020151947_create_comments.up.sql
  create  db/migrations/20261020151947_create_comments.down.sql
  create  app/domain/comment.go
  ...
  create  app/adapters/persistence/comment_test.go
```

There is no `references` field type: `post_id:uuid` is a plain `uuid` column
(UUIDv7 everywhere), so the migration adds no foreign key. The model's use
cases are `application.NewCommentService(persistence.NewCommentRepository(database), time.Now, uuid.New)`;
Go refuses an unused variable, so wire the service where the code that uses
it is (a handler or a job), not before. For full CRUD pages for comments, use
`forge generate scaffold comment ...` instead of the model.

Now a guarded page:

```sh
forge generate controller comments index
```

```text
  create  app/adapters/htmlui/comments_controller.go
  create  app/adapters/htmlui/comments_controller_test.go
  create  app/views/comments/index.gohtml
```

The controller asks for the `comments:read` permission, which the model's
migration already registers (so no second migration is generated), and the
owner holds it. Mount it with the `protectCookie` guard from section 5, right
after the posts registration in `identityRoutes`:

```go
	if err := htmlui.RegisterCommentsController(app, renderer, protectCookie); err != nil {
		return err
	}
```

Edit the page. The layout and the heading stay; replace the body of
`app/views/comments/index.gohtml`:

```sh
cat > app/views/comments/index.gohtml <<'EOF'
{{define "title"}}{{.Title}}{{end}}
{{define "content"}}
<h1>{{.Title}}</h1>
<p>Moderation tools will live here.</p>
{{end}}
EOF
forge routes | grep comments
forge test
```

```text
GET     /comments                  -        app/adapters/htmlui/comments_controller.go:47
```

Reload <http://localhost:8080/comments>. Other actions are other arguments:
`forge generate controller reports monthly` is `GET /reports/monthly` guarded
by `reports:read`, `--api` makes JSON handlers under `/v1`, and `--public`
mounts pages anyone may read (with a warning). Controllers answer `GET` only;
anything that changes data is a scaffold.
See [GENERATORS.md](GENERATORS.md).

## 7. Authentication

You already have it: `forge new` on PostgreSQL generates the sign-in, sign-out,
two-factor and password-reset pages, and `forge setup` created the owner.

| URL | Page |
| --- | ---- |
| `/session/new` | sign in (organization, email, password) |
| `/session/mfa` | authenticator or backup code, when `FORGE_AUTH_MFA_KEY` is set (`forge new` put one in `.env.local`) |
| `/mfa` | two-factor settings: turn on, replace the authenticator, new backup codes, turn off |
| `/passwords/new` | forgot password (needs outgoing mail, section 9) |
| `/passwords/edit?token=...` | choose a new password from the emailed link |

Sign in as the owner at <http://localhost:8080/session/new>. Visiting a
guarded page signed out redirects to the sign-in page and back afterwards;
an API client gets a `401` JSON error instead. A page asks for one permission
(`posts:read`, `comments:read`...) and roles hold permissions. Sign-ins
(success, failure, throttling) and permission denials of signed-in users are
audited; a request with no or an invalid session is refused with a `401` or a
redirect but not recorded unless you add `auth.WithAuthenticationAuditor`
([AUTHENTICATION.md](AUTHENTICATION.md), [AUDIT.md](AUDIT.md)). To let a signed-in user sign out, put
`{{template "sessions/_sign_out" .}}` in a page: it renders a `DELETE /session`
form.

Turn on two-factor authentication for the owner: signed in, open
<http://localhost:8080/mfa>, enter the password, add the key to an
authenticator app (open the `otpauth://` link on a phone or type the Base32
key) and confirm with the password and the app's 6-digit code. Keep the 10
backup codes the page shows once; you are signed out everywhere, and from
now on signing in asks for a code at `/session/mfa`. Replacing the
authenticator, new backup codes and turning it off are on the same page and
ask for the password and a current code ([MFA.md](MFA.md)).

`forge generate authentication` writes the same pages into an application
that predates them; here it refuses, because they exist:

```sh
forge generate authentication --dry-run
```

```text
forge: refusing to overwrite app/adapters/htmlauth/htmlauth.go; nothing was written
```

`forge setup` refuses once a tenant exists. The same command exists in the
application binary for containers and production, with the password piped
(never an argument). After `forge build`, from the application directory:

```sh
printf 'Owner password: '; read -rs OWNER_PASSWORD; echo   # bash or zsh
printf '%s\n' "$OWNER_PASSWORD" | bin/blog setup --tenant acme --email you@example.com --password-stdin
unset OWNER_PASSWORD
```

Details, tokens for API clients, two-factor enrollment and account recovery:
[AUTHENTICATION.md](AUTHENTICATION.md), [MFA.md](MFA.md),
[ACCOUNT_RECOVERY.md](ACCOUNT_RECOVERY.md).

## 8. Translations

Every generated page reads its text from `config/locales`. Add Brazilian
Portuguese by copying the three files you have, renaming the top-level key
(line 2 of each file), and translating the values:

```sh
cd config/locales
for pair in en:pt-BR authentication.en:authentication.pt-BR posts.en:posts.pt-BR; do
  cp "${pair%%:*}.json" "${pair##*:}.json"
  sed -i.bak '2s/"en"/"pt-BR"/' "${pair##*:}.json" && rm "${pair##*:}.json.bak"
done
sed -i.bak 's/"title": "Welcome to %{app}"/"title": "Bem-vindo ao %{app}"/' pt-BR.json && rm pt-BR.json.bak
cd ../..
forge i18n check
```

```text
config/locales: en, pt-BR; 130 keys, 115 referenced in templates and Go code

en: complete

pt-BR: complete
```

A locale is just the files that exist; the copy is complete but still in
English except the welcome title. `forge i18n check` exits `1` and lists what
a locale lacks (`pt-BR: 1 missing`, then the key), plural forms a locale
needs, and keys nothing references; `--json` is for CI. Each request picks its
locale from the `locale` cookie, then `Accept-Language`, then the default:

```sh
curl -s -H 'Accept-Language: pt-BR' http://127.0.0.1:8080/ | grep '<h1'
curl -s -H 'Accept-Language: en' http://127.0.0.1:8080/ | grep '<h1'
```

```text
<h1 id="welcome-title">Bem-vindo ao blog</h1>
<h1 id="welcome-title">Welcome to blog</h1>
```

To make Portuguese the default, set `FORGE_I18N_DEFAULT_LOCALE=pt-BR`
(`.env.local`, no rebuild). For a new application, `forge new shop --locale
pt-BR` ships the generated pages already translated into `pt-BR`, `es` or
`en`. In templates use `{{t "posts.index.title"}}`, `{{l .CreatedAt "short"}}`
and `{{number .Total 2}}`; in Go `i18n.T(ctx, key)`. See [I18N.md](I18N.md).

## 9. Email

Generate a mailer with typed methods, text and HTML templates, browser
previews and tests:

```sh
forge generate mailer user welcome:name,url
```

```text
  create  app/mailers/user_mailer.go
  create  app/mailers/user_mailer_preview.go
  create  app/mailers/user_mailer_test.go
  create  app/mailers/templates/user/welcome.subject.tmpl
  create  app/mailers/templates/user/welcome.text.tmpl
  create  app/mailers/templates/user/welcome.html.gohtml
```

Wire it. Add the imports `"blog/app/mailers"` (and `"context"`, `"errors"` and
`"github.com/rocketinsidecorp/forge-go/mail"` if `bootstrap.go` lacks them; the
sign-in code already imports the last two). Then, in `identityRoutes` after the
comments controller registration, paste the sender, the mailer and its
previews:

```go
	mailSender, err := mail.FromConfig(app.Config(), mail.WithLogger(app.Logger()))
	var mailFrom mail.Address
	switch {
	case errors.Is(err, mail.ErrNotConfigured):
		// Mail is optional: until FORGE_MAIL_URL is set, every delivery fails
		// closed with mail.ErrNotConfigured instead of pretending to send.
		app.Logger().Warn("outgoing mail is not configured: set FORGE_MAIL_URL and FORGE_MAIL_FROM")
		mailSender = mail.SenderFunc(func(context.Context, mail.Message) error { return mail.ErrNotConfigured })
		mailFrom = mail.Address{Email: "noreply@example.invalid"}
	case err != nil:
		return err
	default:
		if mailFrom, err = mail.DefaultFrom(app.Config()); err != nil {
			return err // FORGE_MAIL_FROM
		}
	}
	userMailer, err := mailers.NewUserMailer(mailSender, mailFrom)
	if err != nil {
		return err
	}
	// Previews render each email with fixture data and send nothing. They are
	// served only in development and test, and only with FORGE_MAIL_PREVIEW=true.
	if app.Config().Mail.Preview {
		userPreviews, err := mail.NewPreviewHandler(app.Config(), "/_mail/user", mailers.UserMailerPreviews(userMailer)...)
		if err != nil {
			return err
		}
		if err := app.Handle("GET /_mail/user/", userPreviews); err != nil {
			return err
		}
	}
```

Configure development mail: every message becomes an `.eml` file under
`tmp/mail`, and previews are on. These settings also turn on the
password-reset pages, which need a mail transport and the public URL of the
reset page:

```sh
cat >> .env.local <<'ENV'
FORGE_MAIL_URL=file:tmp/mail
FORGE_MAIL_FROM="blog <no-reply@example.com>"
FORGE_MAIL_PREVIEW=true
FORGE_AUTH_PASSWORD_RESET_URL=http://localhost:8080/passwords/edit
ENV
gofmt -w app/bootstrap/bootstrap.go
forge test
```

Open <http://localhost:8080/_mail/user/> for the preview list and `/_mail/user/welcome`
for the email rendered with fixture data. Send one from your own code with
`userMailer.Welcome(ctx, to, mailers.UserWelcomeData{Name: ..., URL: ...})`;
data is escaped for its context and header injection is refused before a byte
is sent. To see the password-reset email, request one at
<http://localhost:8080/passwords/new> and open the newest `tmp/mail/*.eml`. In
production set `FORGE_MAIL_URL=smtps://user:password@smtp.example.com:465`
([MAIL.md](MAIL.md)).

## 10. Background jobs

Forge has two kinds. A **recurring job** runs in the application's own
scheduler; a **queue job** is handed off through the database (retries,
backoff, dead jobs). First the recurring kind. `forge new` already created
`bootstrap.JobsConfigure` with a scheduler:

```sh
forge generate job cleanup
```

```text
  create  app/jobs/cleanup.go
  create  app/jobs/cleanup_test.go
  implement the job, then register it in bootstrap.JobsConfigure with an explicit schedule
```

The generated stub returns "not implemented" so that registering a placeholder
can never report success. Implement it, and its test:

```sh
cat > app/jobs/cleanup.go <<'EOF'
package jobs

import (
	"context"
	"log/slog"
)

// RunCleanup performs one cleanup job invocation. The scheduler provides a
// bounded context and cancels it during shutdown; honor cancellation promptly.
// Every replica runs this job, so make the work idempotent or acquire a
// PostgreSQL advisory lock when only one replica may perform a schedule tick.
func RunCleanup(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	slog.InfoContext(ctx, "cleanup ran")
	return nil
}
EOF
cat > app/jobs/cleanup_test.go <<'EOF'
package jobs

import (
	"context"
	"errors"
	"testing"
)

func TestRunCleanupSucceeds(t *testing.T) {
	t.Parallel()
	if err := RunCleanup(context.Background()); err != nil {
		t.Fatalf("RunCleanup error = %v", err)
	}
}

func TestRunCleanupHonorsCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunCleanup(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunCleanup error = %v, want context.Canceled", err)
	}
}
EOF
```

Register it in `JobsConfigure`, after the `scheduler := jobs.NewScheduler(...)`
statement and before the `app.OnShutdown` call, importing the package under an
alias because it shares its name with `forge-go/jobs`
(`appjobs "blog/app/jobs"`):

```go
	if err := scheduler.Register("cleanup", jobs.ScheduleSpec{Every: 30 * time.Second}, appjobs.RunCleanup); err != nil {
		_ = scheduler.Stop()
		return err
	}
```

```sh
gofmt -w app/bootstrap/bootstrap.go
forge test ./app/jobs
```

Within a minute `forge dev` prints `INFO cleanup ran` every 30 seconds.
Schedules are `Every`, `DailyAt` (UTC) or a five-field `Cron`; failures are
reported but not retried.

For work you hand off ("send this welcome email"), generate a typed handler for
the persistent queue, with its payload, enqueue function and tests:

```sh
forge generate job send_welcome user_id:uuid --queue
forge test ./app/jobs
```

The handler fails permanently until you implement `PerformSendWelcome`, so it
can never report fake success. Running the queue needs its tables and a worker
first (`queuepostgres.Migrations()`, `queuepostgres.NewStore`, `queue.NewWorker`
started in `Configure` and stopped in `OnShutdown`); [JOBS.md](JOBS.md)
sections 1 and 4 have the wiring. This guide leaves it unwired and removes it in
section 15.

## 11. OpenAPI, tests and diagnostics

JSON routes declare their types and Forge builds an OpenAPI 3.1 document from
them. `forge new` and the scaffold's `/v1/posts` are described already:

```sh
curl -s http://127.0.0.1:8080/openapi.json | head -c 120      # served in development and test
forge openapi --output openapi.json                          # write it, to commit and diff
forge openapi --check openapi.json                           # exit 1 when the file is stale
```

```text
openapi: 29 routes are not described (openapi.Describe or openapi.Typed): GET /_mail/user/, GET /assets/, GET /comments, ...
openapi: wrote openapi.json (15 paths)
openapi: openapi.json is up to date
```

The HTML pages are not part of the API document, so they are listed on stderr,
not failed. `forge openapi` builds your application and runs `Configure`
without listening; staging and production serve `/openapi.json` only with
`FORGE_OPENAPI_ENABLED=true` (the document lists every documented route, and
the application logs a warning at start-up). Declare your own JSON routes with
`openapi.Typed`/`openapi.Describe` ([OPENAPI.md](OPENAPI.md)).

The rest of the toolbox:

```sh
forge test                  # go test ./... with .env.development loaded; arguments pass through
forge test -run Post ./app/...
forge doctor                # validate FORGE_* configuration (secrets redacted); --json for tools
forge routes                # every route found in the source, with its action and file:line
forge inspect --json        # versioned inventory: commands, migrations, routes, modules
forge build                 # static binary in bin/blog
forge db status             # applied and pending migrations
forge db console -- -c 'select name from forge_permissions order by 1'   # runs psql
```

`forge doctor` prints:

```text
configuration: ok
environment: development
http address: 127.0.0.1:8080
transport: plain
database: postgres://blog@127.0.0.1:27521/blog_development?sslmode=disable
```

`forge db rollback`/`forge rollback` revert your newest migration, never a
framework table. The generated `.github/workflows/ci.yml` runs gofmt, vet, the
tests against a PostgreSQL service, `govulncheck` and a Docker build.

## 12. Credentials

Secrets that do not belong in `.env` files live encrypted in
`config/credentials/ENV.json.enc`, which you commit; the key stays out of git
(`config/credentials/development.key`, mode 0600, created by `forge new`, or
the `FORGE_MASTER_KEY` variable in deployed environments). Like `rails
credentials:edit`:

```sh
forge credentials show
```

```text
{
  "encryption": {
    "active": "[REDACTED]",
    "keys": {
      "k1": "[REDACTED]"
    }
  }
}
```

`show` prints the structure with every value redacted (`--reveal` prints the
values, in an interactive terminal only). `edit` decrypts into a private
temporary file, opens `$VISUAL` or `$EDITOR`, validates the JSON, re-encrypts
atomically and wipes the plaintext. Add a secret of your own, for example
`"payments": { "api_key": "sk_test_example" }` next to `"encryption"`, save and
quit:

```sh
VISUAL=nano forge credentials edit      # any editor; $VISUAL wins over $EDITOR
forge credentials show
```

```text
saved config/credentials/development.json.enc (key 46b0d18d056656c2)
{
  "encryption": {
    ...
  },
  "payments": {
    "api_key": "[REDACTED]"
  }
}
```

Create another environment's pair, and rotate its key when someone who knew it
leaves. The new key list is written to the key file and the old keys stay
readable until every deployment has the new file (`--keep-old`):

```sh
forge credentials init --environment production
forge credentials rotate-key --environment production --keep-old
```

```text
  create  config/credentials/production.key
  create  config/credentials/production.json.enc
  ignore  /config/credentials/*.key in .gitignore and .dockerignore

The key in config/credentials/production.key is the only way to read config/credentials/production.json.enc.
Back it up in a password manager or secret store; it is never committed.
Deploy with:  FORGE_MASTER_KEY=$(cat config/credentials/production.key)   (see docs/CREDENTIALS.md)
Next:         forge credentials edit --environment production
re-encrypted config/credentials/production.json.enc with key 51063fafdc95178c; the previous key list stays readable (1 key(s)); drop it once every deployment has the new file
update FORGE_MASTER_KEY wherever it is stored (docs/CREDENTIALS.md, "Rotating the key")
```

Back up every key: without it the file cannot be read. Application code reads
values with `credentials.Open` in your composition root, and the `encryption`
package encrypts a column with the key ring that is already in the file
([CREDENTIALS.md](CREDENTIALS.md), [ENCRYPTION.md](ENCRYPTION.md)).

## 13. Observability

Telemetry is on by default and exports nothing. To see metrics without any
extra tooling:

```sh
FORGE_TELEMETRY_METRICS_ADDR=127.0.0.1:9464 forge dev
curl -s http://127.0.0.1:9464/metrics | grep http_server_request_duration_seconds
```

Every generated application also has an opt-in stack: an OpenTelemetry
Collector, Prometheus, Tempo (traces), Loki (logs) and Grafana with a starter
dashboard, every port bound to `127.0.0.1`. It needs Docker and runs the
application as a container, replacing `forge dev` while it is up:

```sh
export GRAFANA_ADMIN_PASSWORD="$(openssl rand -base64 18)"   # required, also for `down`
docker compose -f compose.yaml -f compose.observability.yaml up --build
curl http://127.0.0.1:8080/v1/status
```

Open Grafana at <http://127.0.0.1:3000> (user `admin`, the password above):
the dashboard "Forge application", Explore > Tempo for traces and Explore >
Loki for logs; Prometheus is at <http://127.0.0.1:9090>. Log lines carry
`trace_id`, so a log links to its trace. Stop it with
`docker compose -f compose.yaml -f compose.observability.yaml down` (add `-v`
to delete the data). Details, the metric catalogue and what is never recorded:
[OBSERVABILITY.md](OBSERVABILITY.md).

## 14. Deploy

`forge deploy` ships your image to servers that have only SSH and Docker,
Kamal-style, with health-gated, zero-downtime switches behind **forge-proxy**.
These commands need servers and a registry, so this guide only shows the
shape; [DEPLOYMENT.md](DEPLOYMENT.md#forge-deploy) walks through a fresh VPS.

```sh
forge deploy init                      # config/deploy.yml and .forge/secrets (0600, git-ignored)
$EDITOR config/deploy.yml .forge/secrets
forge deploy setup --dry-run           # every remote command, secrets redacted
forge deploy setup                     # docker check, network, forge-proxy, accessories
forge deploy                           # build, push, migrate, switch on every host
forge deploy details                   # versions per host and role
forge logs -f --grep ERROR             # logs from every host, prefixed host|
forge app exec migrate status          # a one-off container of the deployed image
forge deploy rollback 3f2a9c1d07be     # back to a retained version
forge deploy maintenance on            # serve the 503 page; `off` restores the app
```

A deploy migrates before any switch, keeps the previous containers until every
host passes its health check, and routes everything back if one fails. Secrets
come from `.forge/secrets` or your environment and never appear in command
lines or dry runs. Before deploying: build the proxy image once
(`docker build -f cmd/forge-proxy/Dockerfile -t REGISTRY/forge-proxy:TAG .` from
a Forge checkout, then `docker push` it; DEPLOYMENT.md has the signed image to
pull instead), set `FORGE_STREAMS_KEY`, `FORGE_AUTH_MFA_KEY` and your mail
settings as secrets,
and read the production notes in [DEPLOYMENT.md](DEPLOYMENT.md). You can run
the image yourself with `docker build -t blog .`.

## 15. Undo with `forge destroy`

`forge destroy GENERATOR ARGUMENTS...` takes the arguments you generated with
and removes exactly the files the generator wrote, but **only while they are
unchanged**. You edited the comments page, so it refuses:

```sh
forge destroy controller comments index --dry-run
```

```text
forge: refusing to destroy: 1 file(s) differ from what this generator would write from these arguments (edited since, generated with other arguments, or written by another generator), so nothing was removed:
  app/views/comments/index.gohtml
Review them, or pass --force to remove them anyway
```

Take the controller away with `--force`, then delete the
`RegisterCommentsController` block from `bootstrap.go` (destroy prints it):

```sh
forge destroy controller comments index --force
```

Migrations are history: they stay unless you roll them back first and then
ask for `--migrations`. Remove the comments model the same way, and the queue
job that was never wired:

```sh
forge rollback                         # reverts the newest migration: create_comments
forge destroy model comment post_id:uuid body:text --tenant-scoped --migrations
forge destroy job send_welcome user_id:uuid --queue
forge test
```

Destroy never edits `bootstrap.go` (it prints the wiring to remove), never
removes shared files (the views base, the live-updates transport) and removes
directories only when they become empty. Supported generators: `model`,
`resource`, `scaffold`, `controller`, `mailer`, `job` (both kinds) and `view`.
Do not run it while an editor or formatter writes the same files.

## 16. Where to go next

- [README](../README.md): the full command reference, deploy and credentials.
- [GENERATORS.md](GENERATORS.md): every generator, `--api` controllers, `resource`, permission ownership.
- [VIEWS.md](VIEWS.md): layouts, partials, helpers, Hotwire, live updates.
- [DATABASE.md](DATABASE.md): lifecycle (`forge db`), transactions, repositories, MySQL, MariaDB and SQLite.
- [AUTHENTICATION.md](AUTHENTICATION.md), [MFA.md](MFA.md), [ACCOUNT_RECOVERY.md](ACCOUNT_RECOVERY.md), [MULTI_TENANCY.md](MULTI_TENANCY.md), [AUDIT.md](AUDIT.md).
- [I18N.md](I18N.md), [MAIL.md](MAIL.md), [JOBS.md](JOBS.md), [OPENAPI.md](OPENAPI.md).
- [CREDENTIALS.md](CREDENTIALS.md), [ENCRYPTION.md](ENCRYPTION.md), [RATE_LIMITING.md](RATE_LIMITING.md), [CACHING.md](CACHING.md), [STORAGE.md](STORAGE.md).
- [OBSERVABILITY.md](OBSERVABILITY.md) and [DEPLOYMENT.md](DEPLOYMENT.md).
- [ARCHITECTURE.md](ARCHITECTURE.md), [THREAT_MODEL.md](THREAT_MODEL.md) and the [ADRs](adr).
- [RAILS_PARITY.md](RAILS_PARITY.md): what Forge has and lacks relative to Rails.
