# ADR 0021: Resource generator

- **Status:** Accepted
- **Date:** 2026-10-14

## Context

`forge generate migration|view|job` exist, but nothing generates a working
resource: schema, validation, persistence, HTTP and tests. That is the step
that makes `forge new` + `generate` + `migrate` productive. A generator also
fixes conventions in every application that uses it, so its defaults must be
the secure ones: Forge's identifier is UUIDv7 (ADR 0001), tenant isolation
fails closed (ADR 0010, ADR 0017), and Forge never ships public CRUD by
accident.

## Decision

`forge generate resource NAME field:type... (--tenant-scoped | --global)
[--plural PLURAL] [--dry-run]` generates, for the application's database
(read from the project, as `forge inspect` does):

- `db/migrations/<UTC timestamp>_create_<plural>.{up,down}.sql`
- `app/domain/<name>.go` and test: entity, constructor, per-field validation,
  errors as `fault` sentinels with stable codes.
- `app/application/<name>.go` and test: ports, service, use cases; tests use
  an in-memory fake.
- `app/adapters/persistence/<name>.go` and integration test: explicit SQL for
  the application's database, errors through the adapter's `Translate`.
- `app/adapters/httpapi/<name>.go` and test: thin handlers and a
  `Register<Plural>` function.

Rules:

1. **UUIDv7 everywhere, no exceptions.** The primary key, and `tenant_id`
   when scoped, use the database's UUID storage: PostgreSQL
   `uuid PRIMARY KEY DEFAULT uuidv7()`, MySQL/MariaDB `CHAR(36)`, SQLite
   `TEXT` in a `STRICT` table. IDs are generated in Go by an injected
   `uuid.New`; the PostgreSQL default is only a fallback for rows inserted by
   hand. Generated SQL never contains `serial`, `AUTO_INCREMENT`,
   `gen_random_uuid()`, `UUID()` or another identifier strategy, and a test
   enforces it for every database. `field:uuid` references use the same
   storage.
2. **Explicit scope.** Exactly one of `--tenant-scoped` or `--global` is
   required; there is no default to forget. Tenant-scoped resources take the
   tenant from `tenancy.Require(ctx)` in the repository, bind `tenant_id` in
   every predicate and insert, and index `(tenant_id, id)`. On PostgreSQL the
   repository also runs in `InTenantTx` behind `ENABLE`/`FORCE ROW LEVEL
   SECURITY` policies (defense in depth). Generated tests prove two tenants
   with overlapping data never see or modify each other's rows.
3. **No public CRUD.** `Register<Plural>` requires a non-nil `Protect`
   function that wraps each handler with its permission
   (`<plural>:read` for reads, `<plural>:write` for writes). In applications
   with Forge identity (PostgreSQL) the migration inserts those permissions
   into `forge_permissions` and the wiring snippet uses `auth.RequireAudited`
   behind the authentication middleware, so denials are audited like other
   security events; elsewhere the application supplies its own guard
   explicitly. Roles belong to tenants, so a tenant administrator can grant
   itself any permission: for a `--global` resource in an application with
   Forge identity the snippet keeps reads behind `<plural>:read` but routes
   writes through a separate deny-all `protectPlatformWrite` that the
   application replaces with platform-level (operator) authorization.
4. **Optimistic locking and keyset pagination by default.** Rows carry
   `version`, `created_at`, `updated_at` (UTC); updates and deletes require
   the version the client read (stale → `409`); lists are newest-first keyset
   pages over the UUIDv7 key with the `pagination` package.
5. **Portable, exact types only.** `string` (≤255 characters), `text`, `int`
   (64-bit), `bool`, `time` (UTC), `uuid`; a trailing `?` makes a field
   optional (`NULL`, pointer in Go). There is no floating-point or decimal
   type: money is an `int` in minor units (`price_cents:int`).
6. **No hidden edits.** The generator only creates files and refuses to
   overwrite any of them (all-or-nothing, as the other generators). It does
   not edit `bootstrap.go`; it prints the exact wiring to add.
7. `--dry-run` lists every planned file and the wiring without writing
   anything.

## Consequences

- Every generated table has the same identifier, timestamp, versioning and
  isolation shape on all four databases.
- Applications without Forge identity must provide an authorization guard;
  the generator cannot make that decision for them.
- Pluralization is a small built-in rule set; irregular plurals use
  `--plural`.
- Relationships, decimal types, soft delete and audit columns are future
  work; `field:uuid` gives an unconstrained reference until then.

## Addendum (2026-10-19): `forge generate scaffold`

`forge generate scaffold NAME field:type... (--tenant-scoped | --global)
[--plural PLURAL] [--dry-run]` is the resource generator plus a server-rendered
HTML adapter ([ADR 0025](0025-server-rendered-html.md)), as `rails g scaffold`
is to `rails g model`. It is not a second implementation: it parses, validates,
names, renders SQL and generates every resource file through the same code, and
its plan is checked for conflicts together with the resource's files
(all-or-nothing, refuse-to-overwrite, `--dry-run`). The resource files are
byte-identical to those of `generate resource` (a test compares them).

Extra files:

- `app/adapters/htmlui/<name>.go` and `_test.go`: `Register<Plural>(app,
  service, renderer, protect)` mounts `Router.Resources("/<plural>", ...)`.
  The adapter depends on `application`, `domain` and framework packages only.
- `app/views/<plural>/{index,show,new,edit,_form,not_found}.gohtml`, rendered in
  `layouts/application`.
- The views base `forge new` generates (`app/views/views.go`, the layout, the
  flash partial) when the application has none. `forge generate view` shares
  it: a view is `app/views/DIR/NAME.gohtml` (DIR defaults to `pages`), so
  `forge new`, `generate view` and `generate scaffold` agree on one layout.

Decisions:

1. **Forms.** `string` is a text input, `text` a textarea, `int` a number
   input, `time` a `datetime-local` input, `uuid` a text input with a UUIDv7
   pattern, a required `bool` a checkbox. An optional `bool` is a select
   (not set / yes / no): an unchecked box cannot tell "no" from "not set".
   Optional fields are not `required` and may be left empty (empty clears the
   stored value). The `pattern`, `maxlength` and `required` attributes are
   conveniences; the domain stays the authority.
2. **Errors.** Values that do not convert (`web.DecodeForm`) are all reported
   together. A domain validation error is mapped to its field with the
   domain's own message, so the form and the JSON API cannot disagree; the
   domain reports the first invalid field per submission. Both re-render the
   form with `422` and the submitted text. Unknown fields (`id`, `tenant_id`,
   a version on create) are `400`, so nothing can be mass-assigned.
3. **Success.** A flash notice and `303 See Other` to the show page (create,
   update) or the index (destroy).
4. **Optimistic locking.** The edit form and the delete button carry the
   version they were loaded from. A stale update re-renders the edit form with
   `409 Conflict` (not `422`: the input is valid, the request conflicts with
   the stored state), keeps what the person typed, writes nothing and links to
   the latest version. A stale delete keeps the record and redirects to it with
   an alert.
5. **Not found.** A missing record, a malformed id and, for tenant-scoped
   resources, another tenant's record all render the `404` page. Other errors
   use the framework's JSON error response (there is no generic HTML error
   page yet).
6. **Access.** The pages are never public. `Register<Plural>` requires the
   same `protect` function as the JSON routes: `<plural>:read` for index and
   show, `<plural>:write` for new, edit, create, update and destroy (a form
   needs the permission of the request that saves it). With Forge identity the
   printed wiring defines `protectCookie`, which authenticates the session
   cookie (`Middleware.AuthenticateCookie`) before the same audited permission
   check; the forms are protected by the CSRF middleware, so `auth.RequireCSRF`
   (a header check) is not added. A global scaffold keeps every write,
   including the new and edit forms, behind `protectPlatformWrite`; without
   identity the same deny-all guard serves both. Forge had no login page then,
   so a browser without a session received the `401` JSON error;
   [ADR 0026](0026-authentication-pages-and-setup.md) adds the sign-in pages,
   and the printed guard now uses `Middleware.AuthenticateBrowser`, which
   redirects signed-out browsers to them.
7. **Browser pipeline.** The pages rely on `web.MethodOverride`,
   `csrf.Protection` and `web.Flash`, which `forge new` installs. The command
   warns when `app/bootstrap/bootstrap.go` does not appear to install them.
8. **Names.** The plural becomes a path (`/posts`), so `assets`, `auth`,
   `health`, `v1`, `layouts` and `shared` are refused as plurals.

Limits: no multipart forms, no per-page layout, no relationships; the index
shows up to four non-text fields and the next-page link of the keyset
pagination.
