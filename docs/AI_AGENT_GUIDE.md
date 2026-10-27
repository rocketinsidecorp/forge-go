# AI agent guide

Forge exposes a versioned CLI catalog and read-only inspection so coding agents
can understand a project before proposing changes. Its provider-neutral
`agent` package can execute application-registered actions through policy,
validation, principal permissions, explicit approval for destructive/external
actions, and a mandatory journal. Forge does not call an LLM or provide an
autonomous action catalog; the application owns those integrations.

## The generated AGENTS.md

`forge new` writes `AGENTS.md` into the application: the rings and the
architecture test (`app/architecture_test.go`), the commands, the routes seam,
the security defaults and how to run the tests, in under 120 lines and only for
what that application has (no CSRF paragraph in an `--api` application, no
migrations without a database, no sign-in without PostgreSQL). It is a starting
point: edit it as the application grows. The template is
`internal/scaffold/templates/AGENTS.md.tmpl`;
`internal/cli/agents_guide_test.go` fails when it names a command or a file
that does not exist. Tools that read only `CLAUDE.md` can import it with a
`CLAUDE.md` that contains `@AGENTS.md`.

## Discover before acting

```sh
forge help --json
forge inspect --json
forge doctor --json
```

Use the catalog's `side_effects`, `environment`, `flags`, and `exit_codes` fields
before selecting a command. `inspect` inventories source and configuration
without executing application code or modifying project files. JSON reports
are versioned; agents should reject unknown schema versions rather than assume
that fields have unchanged meaning.

## Preview and request approval

Generators that support `--dry-run` show planned paths without writing files:

```sh
forge generate migration create_orders --dry-run
forge generate view orders --dry-run
forge generate job cleanup --dry-run
forge generate resource product name:string 'summary:text?' price_cents:int --dry-run
forge generate scaffold product name:string 'summary:text?' price_cents:int --org-scoped --dry-run   # with organizations
```

`forge generate scaffold` takes the same arguments as `generate resource` and
adds HTML pages (`app/adapters/htmlui`, `app/views/<plural>`); everything below
about resources applies to it, and its pages are guarded like the JSON routes.

For `forge generate resource` and `forge generate scaffold`, always run `--dry-run` first and show the user
the planned files and the wiring it prints. In an application with
organizations (`forge new --tenants`, `const Organizations = true` in
`app/bootstrap/app_gen.go`) choose the scope deliberately: `--org-scoped` for
data that belongs to one organization, `--global` only for data shared by all
of them; the command refuses to guess. Other applications have plain tables
and refuse `--org-scoped`. Money is an `int` in
minor units (`price_cents:int`); there are no floating-point or decimal types.
Quote an optional field (`'summary:text?'`) so the shell does not expand the
`?`. The generator never edits `app/bootstrap/bootstrap.go` and never opens
routes to the public: the printed `protect` function must be wired (PostgreSQL
applications use Forge identity; other databases need an application-written
guard) before the endpoints answer anything but `403`. For a `--global` resource
in a PostgreSQL application the write routes stay behind a separate deny-all
`protectPlatformWrite`: tenant roles can grant themselves permissions, so
writes to rows every tenant reads need operator-level authorization that only
the application can implement.

Review the proposed paths and content with the user before invoking a
write-producing generator. `--dry-run` is a preview, not an authorization
token. Commands such as `forge new`, `forge migrate`, and `forge rollback`
perform filesystem, subprocess, or database effects; get explicit approval
from the user before running them. In particular, rollback may destroy data.

`forge db` is the database lifecycle (`create`, `drop`, `reset`, `setup`,
`prepare`, `migrate`, `rollback`, `status`, `seed`, `schema:dump`,
`schema:load`, `console`). Its catalog entry lists `database_destructive`.
`status`, `schema:dump` (writes only `db/schema.sql`) and `prepare` on an
existing database are the low-risk ones; `drop`, `reset`, `schema:load` and
`rollback` change or destroy data and need the user's explicit approval for the
named database. Never add `--force-production`, `--yes` or
`FORGE_ALLOW_DESTRUCTIVE=true` on your own: they exist so that a person can
confirm a production operation, and an agent supplying them defeats the
guard. `forge db console` opens an interactive client and is for people; use
`forge db console -- -c 'SELECT ...'` only for read-only queries the user asked
for. `forge dev` starts a long-running process that listens on
`FORGE_HTTP_ADDR` and restarts the application when files change; run it in the
background, and stop it with `SIGTERM`.

For application-defined agent actions, route execution through `agent.Engine`.
Register only the use cases the application intends to expose and validate
their input strictly. `Engine.Execute` accepts no principal argument: it reads
the principal established by Forge authentication middleware and rejects a
missing principal or tenant-context mismatch. It requires that principal's
exact permission for every action, then calls the configured approval adapter
for `EffectDestructive` and `EffectExternal`. It refuses to run if the policy,
validator, permission check, approval, or journal fails.
Policy and approval are application-supplied integrations: the framework
enforces that they are called, but cannot prove that a custom policy is
restrictive or that an approval represents a human grant. Configure them to
deny by default and bind approvals to the authenticated actor, tenant, action,
and intent ID.
`agent/postgres` is currently the durable journal adapter; its schema must be
applied before use, and all records are tenant-scoped with PostgreSQL RLS.
The journal adapter also requires the authenticated principal and checks that
attempt/finalization actor IDs match it; direct journal calls do not accept a
caller-invented actor.
MySQL, MariaDB, and SQLite do not yet have a first-party agent journal.

To find executions requiring operator review, grant the current principal the
exact `agent:reconcile` permission and call
`Journal.ListReconciliationCandidates(ctx, cursor, limit)`. The query returns
at most 100 metadata-only rows per page, scoped to the authenticated tenant;
both reserved rows (an attempt recorded before the callback is invoked) and
explicitly unresolved rows are included. This API only lists candidates. It
does not decide whether a remote side effect happened or mark the execution
resolved; the application must reconcile with the affected system before any
manual retry. A reserved row may still be actively executing and is never
eligible for operator resolution through this API. If a process crashes while
an execution remains `reserved`, the framework exposes it for investigation
but does not automatically declare it abandoned; applications need a worker
liveness/fencing policy before safely recovering that case. Once the executor
reports an uncertain result, an operator who has independently verified the
external effect can call
`Journal.ResolveReconciliation(ctx, executionID, result)` for an `unresolved`
row. That writes a single tenant-scoped decision
(`effect_applied` or `effect_not_applied`) with the operator identity and
timestamp, without changing the original execution record or automatically
retrying it. Pages reflect live state rather than a point-in-time snapshot; if
an execution is resolved while paging, it can disappear from later pages.

The journal persists execution/intent/actor/tenant IDs, action name, effect,
status, and a safe failure code—not prompts, inputs, outputs, or input hashes.
It is a lifecycle journal, not a complete authorization-proof ledger; it does
not independently capture policy versions, permission snapshots, or approval
grant identities.
The intent UUID is an idempotency key: a rejected or previously executed
intent cannot be replayed with modified input. If an executor returns an error,
output validation fails after execution, or final audit is uncertain, the
journal records `unresolved` where possible and the engine returns a stable
error with an execution ID and `MayHaveExecuted`; reconcile it instead of
blindly retrying. Failures before the executor starts are recorded as
failed/denied, not unresolved.

For automation, prefix the command with `--error-format=json` to receive a
versioned, secret-safe failure object on stderr:

```sh
forge --error-format=json version extra
```

The envelope uses `schema_version: 1`; its `error.code` is stable, while
underlying error details and argument values are deliberately omitted. The
process exit status remains authoritative. `doctor --json` and `inspect --json`
return their command-specific reports on stdout.

## Security boundaries

- `forge db` and `forge dev` pass database credentials to vendor tools through
  environment variables or a private temporary file, never arguments; do not
  work around that by putting a URL on a command line.
- Never pass secrets as command-line arguments or include them in generated
  source, prompts, or reports. Doctor and inspect intentionally redact
  configuration values.
- The CLI is not an authorization system. It cannot determine whether a human
  approved a CLI command. The action engine protects only calls routed through
  its registered pipeline; applications can bypass it by invoking their own
  use cases directly. The engine is
  `Intent -> Policy -> Validation -> Authorization -> Execution -> Audit`.
- PostgreSQL has a built-in transaction-scoped RLS helper. MySQL, MariaDB, and
  SQLite tenant isolation remains an application/repository responsibility;
  do not infer equivalent database enforcement from a configured adapter.
- Generated applications use Docker-oriented assets by default, but container
  generation does not replace review of secrets, database privileges, network
  exposure, or production configuration.

See [the threat model](THREAT_MODEL.md), [authentication guidance](AUTHENTICATION.md),
and [the CLI catalog](../README.md#developing-the-framework) for the current
security limits and project workflow.
