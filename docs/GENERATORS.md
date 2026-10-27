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
  `forge generate scaffold`, `resource` and `controller` mount their routes in
  `app/bootstrap/routes_gen.go` instead (see "The routes seam" below); the
  mailer and job generators print the lines to paste, and say where.
- **Never public by accident.** Anything that serves data needs a guard you
  supply, and with Forge identity it comes with its permission.
- **Validated names.** Names and fields become Go identifiers, file names,
  SQL identifiers and URL segments, so they are strict lowercase `snake_case`
  (letters, digits, single underscores, starting with a letter, at most 40
  characters). Path separators, dots, quotes, template syntax and uppercase
  letters are refused before anything is built.

| Command | Creates | Rails |
| ------- | ------- | ----- |
| `forge generate migration NAME [field:type...]` | an up/down migration pair: `ALTER TABLE` statements for `add_COLUMNS_to_TABLE` and `remove_COLUMNS_from_TABLE` with fields, else blank (with a warning to fill it in) | `rails g migration` |
| `forge generate model NAME field:type...` | migration, domain entity, repository port and use cases, persistence adapter, tests | `rails g model` |
| `forge generate controller NAME ACTION...` | HTTP handlers, routes, views, tests; no persistence | `rails g controller` |
| `forge generate mailer NAME METHOD...` | a mailer with text and HTML templates, previews, tests | `rails g mailer` |
| `forge generate job NAME [field:type...] --queue` | a typed handler for the persistent job queue, its enqueue function, tests | `rails g job` |
| `forge generate job NAME` | a recurring job stub for the in-process scheduler | |
| `forge generate resource` / `scaffold` | model plus JSON API / plus HTML CRUD | `rails g scaffold` |
| `forge generate view`, `authentication` | one page; the sign-in pages | |
| `forge destroy GENERATOR ARGUMENTS...` | removes what a generator created | `rails d` |

`forge g` is short for `forge generate` and `forge d` for `forge destroy`, as
in Rails (`forge g scaffold post title:string --global`).

## The front end of a new application

`forge new APP` generates the front end in TypeScript ([ADR
0049](adr/0049-typescript-by-default.md)); `forge new APP --js` generates
the plain JavaScript it generated before. No other generator writes
JavaScript or TypeScript: they add Go and `.gohtml` files, and pages use the
controllers already in `app/javascript/controllers`.

```sh
forge new shop -d sqlite3          # app/javascript/application.ts, controllers/*.ts, tsconfig.json, types/, dist/*.js
forge new shop -d sqlite3 --js     # app/javascript/application.js and controllers/*.js, no tsconfig.json, no dist
forge new shop --api --js          # refused: an API-only application has no front end
```

| Default (TypeScript) | `--js` |
| -------------------- | ------ |
| `app/javascript/application.ts`, `controllers/{application,index,hello_controller}.ts` | the same names with `.js` |
| `app/javascript/dist/**.js`: compiled by `forge`, committed, embedded by `go build` | none: the `.js` sources are embedded |
| `tsconfig.json` (strict, `noEmit`) and `app/javascript/types/` (Turbo and Stimulus declarations) | none |
| `app/javascript/javascript_test.go`: fails when `dist` is stale; CI runs `tsc --noEmit` | none |

`forge dev`, `forge build`, `forge test` and `forge check` compile the
TypeScript; `forge check` also runs `tsc --noEmit` when `tsc` is installed
(`npm install --global typescript@6.0.3`). `forge generate view`,
`controller` and `scaffold` in an application that has no views package yet
add this base too, in TypeScript.

## Migration

```sh
forge g migration add_sku_to_products sku:string stock:int 'released_at:time?'
forge g migration remove_sku_from_products sku:string
forge g migration backfill_prices
```

`add_COLUMNS_to_TABLE` with fields writes one `ALTER TABLE TABLE ADD COLUMN`
per field in the application's dialect (the same column types, constraints
and quoting as a generated table), and `DROP COLUMN` in the down migration;
`remove_COLUMNS_from_TABLE` is the reverse (its down brings the columns back,
not their values). Rows that already exist need a value, so a required
column gets its type's zero value as default (`''`, `0`, false); a required
`time` or `uuid` column has none and is refused: declare it optional
(`released_at:time?`). Any other name, or no fields, writes the blank pair and
warns: fill in both files before `forge migrate` or `forge test`, because the
migrator refuses a migration with no statements. Fields with a name that
infers no table are refused (for a new table use `forge generate model`).
The table is everything after the first `_to_` (or `_from_`), so
`add_x_to_y_to_z` adds to the table `y_to_z`; the columns come from the
fields, never from the name. Running `remove_...` prints a data-loss warning.

Generators and `forge destroy` take `.forge/generate.lock` while they run, so
two of them in one application never interleave their edits of the routes
seam; a second one is refused until the first finishes, and so is any run
that cannot take the lock (`.forge` a symlink or a file, for example). The
seam is written through `app/bootstrap/routes_gen.go.tmp`, synced and renamed
over it, keeping its permissions; a leftover temporary file (an interrupted
write) is refused before anything is generated. The generated `.gitignore`
ignores both files.

The names the seam uses cannot name a resource, plural or controller:
`protect`, `public`, `protect_cookie`, `protect_platform_write`, `renderer`,
`signer`, `stream_signer`, `streams`, `generated_routes`, `repository` and
`middleware`.

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

A scaffold's pages read their text from `config/locales/PLURAL.en.json`, which
it generates (and `destroy` removes when unchanged); translate it into
`PLURAL.LOCALE.json` for every other locale, and `forge i18n check` lists what
is missing ([I18N.md](I18N.md)). Generators that add the views base also add
`config/locales` to an application without it.

Adding the HTTP layer later is `forge generate scaffold` on a new name: a
model, resource and scaffold of one name collide instead of mixing layers.
With Forge identity the migration also registers `posts:read` and
`posts:write`; see [Permission ownership](#permission-ownership) for what
happens when a controller already registered one of them.

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
so `forge routes` lists them. `--api` routes are described for the OpenAPI
document (`<name>ControllerOperation`; change its response type with the
handler) and their tests check every response against it
([OPENAPI.md](OPENAPI.md)). Resources and scaffolds describe their JSON API
the same way.

`Register<Name>Controller` mounts every route behind a guard you pass:

- Without `--public` it needs a `protect(permission, next)` function and asks
  it for `NAME:read`. With Forge identity the guard authenticates the
  session cookie (HTML; signed-out browsers go to `/session/new` and back) or,
  for `--api`, the bearer token of `POST /auth/login` (`--api-auth`) or, in an
  application without that login, the session cookie plus the CSRF token in
  the `X-CSRF-Token` header of every change, and the migration registers the permission and
  grants it to the `owner` role. When a migration already registers
  `NAME:read` (a model, resource or scaffold whose plural is `NAME`), the
  controller reuses it and generates no migration; the output names
  the migration that owns it. Without identity the guard denies
  every request until you replace `protect` with your own (below).
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

## In an API-only application

An application created with `forge new --api` has no views, layout, assets,
translations or browser middleware, and no generator adds them to it (as in
Forge-Rust). The generators that write pages refuse it with the reason and
write nothing, `--dry-run` included:

| Generator | In a `forge new --api` application |
| --------- | ---------------------------------- |
| `generate view` | refused: pages need the layout, assets and browser middleware |
| `generate scaffold` | refused: it writes HTML pages; `generate resource` writes the JSON API alone |
| `generate controller NAME ACTION...` | refused; with `--api` it writes JSON endpoints under `/v1/NAME` |
| `generate authentication [--api-auth]` | refused: the sign-in pages need views and CSRF, and the JSON APIs sit beside them |
| `generate resource`, `model`, `controller --api`, `mailer`, `job`, `migration` | as in a full-stack application; nothing under `app/views`, `app/assets` or `config/locales` |

```sh
forge new shop --api && cd shop
forge generate resource product name:string --tenant-scoped   # JSON under /v1/products
forge generate controller status_checks index --api           # JSON under /v1/status_checks
forge generate scaffold product name:string --tenant-scoped   # refused
forge generate view about                                     # refused
```

The printed wiring goes into `Configure`, after the `routes(ctx, app)` call
(`routes`, before its final `return nil`, without a database). An API-only
application has no Forge identity wiring (on PostgreSQL it has the identity,
audit and session tables, but no sign-in or auth middleware), so the guard
is the deny-all placeholder you replace with the authentication of your API
clients, and no permission migration is generated. Forge-Go recognizes
the application by the phrase `API-only application (forge new --api)` in the
package comment of `app/bootstrap/bootstrap.go`; without it, `generate view`
and `scaffold` add the views base as they do in any application that lacks
one.

## Permission ownership

With Forge identity, generated migrations register the permissions their
routes ask for: `PLURAL:read` and `PLURAL:write` for a model, resource or
scaffold, `NAME:read` for a guarded controller. A migration's rollback removes
what it registered, so each permission has **exactly one owner migration**, and
the generators never give it a second one:

```sh
forge generate scaffold widget title:string --global
forge generate controller widgets dashboard   # reuses widgets:read, writes no migration
```

```sh
forge generate controller widgets dashboard   # registers widgets:read (its own migration)
forge generate scaffold widget title:string --global
# refused: db/migrations/<timestamp>_create_widgets_controller_permissions.up.sql
# already registers widgets:read. Pick another name or --plural, or revert and
# delete that migration pair first (the controller keeps asking for widgets:read,
# which the scaffold then registers).
```

- A permission is "registered" when any `db/migrations/*.up.sql` names it as a
  SQL string literal (`'widgets:read'`), hand-written migrations included.
- The controller's permission migration is idempotent. Its rollback removes
  only the owner roles' grant and the permission. While another role still
  holds the permission, the rollback fails and changes nothing (the database
  restricts the delete): revoke those grants first, so a grant an
  administrator made is never revoked silently.
- A controller that reuses a resource's permission depends on that resource's
  migration: rolling it back removes the permission, and the controller's
  routes then refuse everyone.

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

- A file that differs from what the generator would write from these
  arguments (edited, generated with other arguments, or written by another
  generator) is **never removed**. The command lists such files, removes
  nothing and exits with an error; `--force` removes them anyway.
- Files a bigger generation still needs are refused the same way:
  `destroy model post` while `app/adapters/httpapi/post.go` (resource) or
  `app/adapters/htmlui/post.go` (scaffold) exists, `destroy resource post`
  while the scaffold's pages exist, and `destroy view pages/home` while the
  `pages` controller renders that page. Destroy the bigger generation, or
  pass `--force` to remove only these files.
- **Migrations are kept**: a migration that ran is part of the database's
  history. Run `forge rollback`, then pass `--migrations` to remove them; a
  destroy that finds nothing but kept migrations says so.
- Files shared with the rest of the application, the views base and the live
  updates transport, are never removed; directories are removed only when
  they become empty and were the generator's.
- `--dry-run` lists what would be removed. Nothing is removed through a
  symlink.
- `app/bootstrap/bootstrap.go` is not edited: destroy takes the generator's
  block out of `app/bootstrap/routes_gen.go` (and the guards nothing else
  uses), or, for the mailer and job generators, prints the wiring to take
  out. It refuses, removing nothing, while another block of the seam uses
  what this one declares.

## The routes seam

`forge new` generates `app/bootstrap/routes_gen.go` with an empty
`generatedRoutes` function, and `bootstrap.go` calls it once (inside
`identityRoutes` in a PostgreSQL application, after `routes` otherwise).
`forge generate scaffold`, `resource` and `controller` rewrite that file
whole and `go/format` it:

- Each generator adds one block, opened by a `// forge:KIND NAME` comment
  (`// forge:scaffold products`), in the order they ran. Before them come the
  shared blocks the generated ones use, each defined once and dropped when
  nothing uses it any more: the guards (`protect`, `public`,
  `protectPlatformWrite`, `protectCookie`), the view renderer, and the
  live-update signer and stream of `--broadcast`. Imports follow from the code.
- Running a generator again with the same arguments is a no-op: every file,
  and the block, is reported `identical`. Other arguments for an existing name
  are refused, as before.
- A `// forge:checksum` line records what the generator wrote. A file edited
  by hand is refused by every generator and by destroy, which then write
  nothing; move the change into `bootstrap.go` and restore the file.
- The guards (`protect`, `public`, `protectPlatformWrite`, `protectCookie`)
  are package-level functions after `generatedRoutes`; with Forge identity
  `protect` and `protectCookie` take the session store
  (`protect(repository, middleware)`). To replace one, copy it into another
  file of `app/bootstrap`, change it there, and run any `forge generate`
  command again (repeating the last one is enough). Until then the build
  fails with `protect redeclared`, so an override is never silently ignored;
  afterwards the generators leave theirs out and use yours.
- Without Forge identity (SQLite, MySQL, MariaDB, API-only applications) the
  guard refuses every request; `--public` (resources and scaffolds with
  `--global`, and controllers) uses a pass-through `public` guard instead and
  prints a warning. With Forge identity `--public` is refused for resources
  and scaffolds: their routes need sign-in and the permissions the migration
  grants to the `owner` role.
- An application generated before the seam (its `bootstrap.go` does not call
  `generatedRoutes`) still gets the wiring printed to paste.

Supported generators: `model`, `resource`, `scaffold`, `controller`, `mailer`,
`job` (both kinds) and `view`. Migrations are kept unless `--migrations` is
given; roll them back with `forge rollback` first.

Known limitations:

- A directory that becomes empty is removed even if it existed, empty, before
  the generator ran: destroy keeps no record of who made a directory. The
  application's layout directories (`app`, `app/adapters`, `app/views`, `db`,
  `db/migrations`, `cmd`) are always kept.
- Destroy compares each file, then removes it. A file another process rewrites
  in that short window is removed anyway: do not run destroy while an editor,
  a formatter or another command writes the same files.

## Reading the code

The generators live in `internal/cli`: `resource.go` (field parsing, naming,
the plan and conflict policy every generator shares), `model.go`,
`controller.go`, `mailer.go`, `queuejob.go`, `destroy.go`, and the templates in
`resourcetemplates/`, `scaffoldtemplates/`, `controllertemplates/`,
`mailertemplates/` and `jobtemplates/`. Every generator has tests that
generate into a fresh application and run `go vet`, `go build`, `go test` and
the repository's `golangci-lint` configuration on the result, for PostgreSQL,
MySQL and SQLite where a database is involved.
