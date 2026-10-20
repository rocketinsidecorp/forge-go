# Generators

`forge generate` writes tested, reviewable code the way `rails g` does, and
`forge destroy` takes it away again. Every generator follows the same rules
([ADR 0041](adr/0041-generators-and-destroy.md)):

- **Plain code.** What is generated is ordinary Go: no runtime reflection, no
  hidden registration, nothing to reverse-engineer.
- **Never overwrite.** A generator refuses to write over any file, or through
  a symlink, and writes nothing at all when it refuses. `--dry-run` prints the
  exact files without creating anything.
- **Never edit your composition root.** `app/bootstrap/bootstrap.go` is yours.
  A generator prints the imports and the lines to paste, and says where.
- **Never public by accident.** Anything that serves data needs a guard you
  supply, and with Forge identity it comes with its permission.
- **Validated names.** Names and fields become Go identifiers, file names,
  SQL identifiers and URL segments, so they are strict lowercase `snake_case`
  (letters, digits, single underscores, starting with a letter, at most 40
  characters). Path separators, dots, quotes, template syntax and uppercase
  letters are refused before anything is built.

| Command | Creates | Rails |
| ------- | ------- | ----- |
| `forge generate migration NAME` | an empty up/down migration pair | `rails g migration` |
| `forge generate model NAME field:type...` | migration, domain entity, repository port and use cases, persistence adapter, tests | `rails g model` |
| `forge generate controller NAME ACTION...` | HTTP handlers, routes, views, tests; no persistence | `rails g controller` |
| `forge generate mailer NAME METHOD...` | a mailer with text and HTML templates, previews, tests | `rails g mailer` |
| `forge generate job NAME [field:type...] --queue` | a typed handler for the persistent job queue, its enqueue function, tests | `rails g job` |
| `forge generate job NAME` | a recurring job stub for the in-process scheduler | |
| `forge generate resource` / `scaffold` | model plus JSON API / plus HTML CRUD | `rails g scaffold` |
| `forge generate view`, `authentication` | one page; the sign-in pages | |
| `forge destroy GENERATOR ARGUMENTS...` | removes what a generator created | `rails d` |

## Model

```sh
forge generate model post title:string 'body:text?' published:bool --tenant-scoped --dry-run
forge generate model post title:string 'body:text?' published:bool --tenant-scoped
forge migrate
forge test
```

Takes exactly the arguments of `forge generate resource` (`--tenant-scoped` or
`--global`, `--plural`, field types `string`, `text`, `int`, `bool`, `time`,
`uuid`, a trailing `?` for optional) and writes exactly the model and
persistence files `resource` writes, byte for byte, without the HTTP layer:

```text
db/migrations/<timestamp>_create_posts.{up,down}.sql
app/domain/post.go, post_test.go            entity, validation, typed errors
app/application/post.go, post_test.go       PostRepository port, PostService use cases
app/adapters/persistence/post.go, post_test.go
```

The printed wiring builds the service where you need it:

```go
postService, err := application.NewPostService(persistence.NewPostRepository(database), time.Now, uuid.New)
```

Tenant-scoped models read the tenant from the context and bind it in every
statement (and, on PostgreSQL, run behind forced row-level security); calls
without a tenant fail with `tenant_required`. IDs are UUIDv7 generated in Go.
Adding the HTTP layer later is `forge generate scaffold` on a new name: a
model, resource and scaffold of one name collide instead of mixing layers.

## Controller

```sh
forge generate controller pages home about_us --public --dry-run
forge generate controller pages home about_us --public
forge generate controller reports monthly           # guarded: needs reports:read
forge generate controller status_checks index --api  # JSON under /v1
```

`NAME` is usually plural; each `ACTION` is a read-only `GET` page (the actions
that change data, `create`, `update` and `destroy`, belong to
`forge generate scaffold`, which also generates the model, the form and the
CSRF-protected routes they need, and are refused here).

| Action | HTML route | with `--api` |
| ------ | ---------- | ------------ |
| `index` | `GET /NAME` | `GET /v1/NAME` |
| any other, `about_us` | `GET /NAME/about_us` | `GET /v1/NAME/about_us` |

Files:

```text
app/adapters/htmlui/NAME_controller.go, NAME_controller_test.go   (app/adapters/httpapi with --api)
app/views/NAME/ACTION.gohtml                                      (HTML only; plus the views base if missing)
db/migrations/<timestamp>_create_NAME_controller_permissions.{up,down}.sql   (guarded, with Forge identity)
```

The handlers render `app/views/NAME/ACTION.gohtml` with `Title` and `Content`
(auto-escaped; the views join the application layout, CSRF meta tag and flash
partial) or, with `--api`, answer a small JSON document. Replace the page
struct and the handler bodies with your own. The routes use literal patterns,
so `forge routes` lists them.

`Register<Name>Controller` mounts every route behind a guard you pass:

- Without `--public` it needs a `protect(permission, next)` function and asks
  it for `NAME:read`. With Forge identity the printed wiring authenticates the
  session cookie (HTML; signed-out browsers go to `/session/new` and back) or
  the bearer token (`--api`), and the migration registers the permission and
  grants it to the `owner` role. Without identity the printed guard denies
  every request until you write the real one.
- `--public` mounts the routes with no guard, prints a warning, and registers
  no permission. Use it for pages anyone may read.

Generated tests check that every action answers, that only `GET` is routed,
that the guard is asked for `NAME:read` once per route, that a caller without
it is refused, and that a missing router, renderer or guard stops the
application at start-up.

## Mailer

```sh
forge generate mailer user welcome:name,url password_reset:name,reset_url --dry-run
forge generate mailer user welcome:name,url password_reset:name,reset_url
forge test
```

`NAME` is the mailer (`user` becomes `UserMailer`); each `METHOD` is one email,
optionally followed by `:field,field`, the string fields of its template data.

```text
app/mailers/user_mailer.go             UserMailer, NewUserMailer, one typed method per email
app/mailers/user_mailer_preview.go     UserMailerPreviews: fixture data for the browser preview
app/mailers/user_mailer_test.go
app/mailers/templates/user/welcome.subject.tmpl   one line, text/template
app/mailers/templates/user/welcome.text.tmpl      text/plain part, text/template
app/mailers/templates/user/welcome.html.gohtml    HTML part, html/template (contextual escaping)
```

It builds on the framework's [mail package](MAIL.md): `NewUserMailer(sender,
from)` parses the templates at start-up, so a syntax error stops the
application instead of failing a send. Each email has typed data
(`UserWelcomeData{Name, URL string}`), a `Welcome(ctx, to, data)` method that
renders completely before sending, and `BuildWelcome(to, data)` that returns
the message for previews and tests. Addresses, subjects and bodies pass the
same validation as every message (header injection is refused before a byte is
sent), and the HTML part escapes every value for its context.

The printed wiring builds the sender from `FORGE_MAIL_URL`/`FORGE_MAIL_FROM`
(until they are set every delivery fails with `mail.ErrNotConfigured` and the
application logs a warning), constructs the mailer and, only when
`FORGE_MAIL_PREVIEW=true` in development or test, serves its previews at
`/_mail/NAME/`:

```sh
FORGE_MAIL_URL=file:tmp/mails FORGE_MAIL_FROM='App <app@example.com>' FORGE_MAIL_PREVIEW=true forge dev
# open http://localhost:8080/_mail/user/
```

Generated tests deliver through `mailtest.Recorder`, check the headers and both
parts, check that hostile data is escaped in the HTML part, that a failing
sender sends nothing and records nothing, that a header-injecting recipient is
refused, and that previews build valid messages and exist only in development.

## Job

```sh
forge generate job send_welcome user_id:uuid 'note:string?' --queue --dry-run
forge generate job send_welcome user_id:uuid 'note:string?' --queue
forge test
```

Without `--queue`, `forge generate job NAME` keeps generating the recurring job
stub for the in-process scheduler ([docs/JOBS.md](JOBS.md)). With `--queue` it
generates a typed handler for the persistent queue (package `queue`):

```text
app/jobs/send_welcome.go        SendWelcomeKind, SendWelcomePayload, RegisterSendWelcome,
                                EnqueueSendWelcome, PerformSendWelcome
app/jobs/send_welcome_test.go
```

The payload struct is the job's schema, with the same field types as models
(`id` and `version` are allowed names here). `RegisterSendWelcome(registry)`
registers the handler with defaults of five attempts and a one-minute timeout;
`EnqueueSendWelcome(ctx, store.In(tx), registry, payload)` stores a job
atomically with your data. `PerformSendWelcome` returns a permanent
not-implemented error until you replace it, so a placeholder can never report
successful work: the job goes straight to the dead state, visible in
`app jobs dead`. The tests need no database: registration, the stored job and
payload, an unregistered kind, cancellation.

The printed wiring registers the handler where you build the `queue.Registry`.
An application that does not run the queue yet needs its tables, command and
worker first: [docs/JOBS.md](JOBS.md), sections 1 and 4.

## Destroy

```sh
forge destroy scaffold post title:string body:text --tenant-scoped --dry-run
forge destroy scaffold post title:string body:text --tenant-scoped
forge destroy controller pages home about_us
forge destroy mailer user welcome:name,url
forge destroy job send_welcome user_id:uuid --queue
forge destroy model post title:string body:text --tenant-scoped --migrations
```

`forge destroy GENERATOR ARGUMENTS...` takes the arguments the generator was
run with (copy the generate command and replace `generate` with `destroy`),
regenerates what it would create and removes exactly those files. It can
therefore be exact, and it works for code generated by any earlier version.

- A file that differs from what the generator writes is **never removed**. The
  command lists such files, removes nothing and exits with an error;
  `--force` removes them anyway.
- **Migrations are kept**: a migration that ran is part of the database's
  history. Run `forge rollback`, then pass `--migrations` to remove them.
- Files shared with the rest of the application, the views base and the live
  updates transport, are never removed; directories are removed only when
  they become empty and were the generator's.
- `--dry-run` lists what would be removed. Nothing is removed through a
  symlink.
- `app/bootstrap/bootstrap.go` is not edited: destroy prints the wiring to
  take out.

Supported generators: `model`, `resource`, `scaffold`, `controller`, `mailer`,
`job` (both kinds) and `view`. Migrations are not destroyed.

## Reading the code

The generators live in `internal/cli`: `resource.go` (field parsing, naming,
the plan and conflict policy every generator shares), `model.go`,
`controller.go`, `mailer.go`, `queuejob.go`, `destroy.go`, and the templates in
`resourcetemplates/`, `scaffoldtemplates/`, `controllertemplates/`,
`mailertemplates/` and `jobtemplates/`. Every generator has tests that
generate into a fresh application and run `go vet`, `go build`, `go test` and
the repository's `golangci-lint` configuration on the result, for PostgreSQL,
MySQL and SQLite where a database is involved.
