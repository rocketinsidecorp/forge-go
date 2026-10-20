# Rails parity

Forge exists in two frameworks, Forge-Go and Forge-Rust, that ship the same
capabilities with the same commands, names and conventions (owner decision,
2026-10-20). This matrix tracks Ruby on Rails 8 capabilities and the places
where Forge intends to go further. A row is **Done** only when code, tests and
README commands exist on `main`; **In review** means a pull request is open or
the work is finished and waiting for its review; **Planned** means not started.

| Capability | Rails 8 | Forge-Go | Forge-Rust |
| --- | --- | --- | --- |
| New application with Docker and CI | `rails new` | Done | Done |
| Database selection | `rails new -d` | Done (PostgreSQL, MySQL, MariaDB, SQLite) | Done (PostgreSQL, MySQL, MariaDB, SQLite) |
| Server-rendered HTML, layouts, partials, forms, flash, CSRF | Action View | Done | Done |
| HTML + JSON CRUD generator | `rails g scaffold` | Done | Done |
| Authentication pages, password reset | `rails g authentication` | Done | In review |
| First tenant and owner, seeds | `db:seed` | Done (`forge setup`, `forge seed`) | In review (`setup`, `seed`) |
| Multi-tenancy with database enforcement | (gems) | Done (PostgreSQL RLS) | Done (PostgreSQL RLS) |
| TOTP MFA | (gems) | Done | Planned |
| Persistent jobs, cron, outbox | Solid Queue | Done | In review |
| Mailer with previews | Action Mailer | Done | Planned |
| Rate limiting | `rate_limit` | Done | In review |
| Client address behind trusted proxies | `request.remote_ip`, `trusted_proxies` | Done (`FORGE_HTTP_TRUSTED_PROXIES`, ADR 0037) | Done (`FORGE_HTTP_TRUSTED_PROXIES`, Forge-Rust ADR 0032) |
| Cache | Solid Cache | Done | In review |
| File storage with signed URLs | Active Storage | Done (disk) | In review (disk) |
| Import maps, Turbo, Stimulus | Hotwire | Done | Done |
| Turbo Streams over SSE | Action Cable | Done | Done (`scaffold --broadcast`, Forge-Rust ADR 0031) |
| Traces, metrics, log correlation | (gems) | Done (OpenTelemetry, Prometheus) | Planned ([forge-rust#13](https://github.com/rocketinsidecorp/forge-rust/issues/13)) |
| Preconfigured Prometheus, Grafana, Tempo, Loki | — | Done (`compose.observability.yaml`, [ADR 0038](adr/0038-local-observability-stack.md)) | Planned ([forge-rust#13](https://github.com/rocketinsidecorp/forge-rust/issues/13), after Turbo Streams and trusted proxies) |
| Zero-downtime deploys, rollback, remote logs | Kamal | Done (`forge deploy`, `forge logs`, `forge-proxy`) | Planned |
| Live reload and database lifecycle | `bin/dev`, `db:*` | Done (`forge dev`, `forge db`) | Planned |
| Rollback never reverts framework migrations; disabled framework sets tolerated | (engine migrations copied into the app) | Done (`rollback`, `Migrator.DownFramework`, ADR 0039) | Done (`rollback`, `rollback_framework`, ADR 0030) |
| Encrypted credentials, attribute encryption | `credentials:edit`, AR Encryption | Done (`forge credentials`, `encryption`) | Planned |
| i18n | I18n | Done (`config/locales/*.json`, `t`, CLDR plurals, `forge new --locale`, `forge i18n check`, [ADR 0042](adr/0042-internationalization.md)) | Planned (forge-rust#?) |
| OpenAPI from typed routes | — | Planned | Planned |
| Controller, model, mailer and job generators, `destroy` | `rails g`, `rails d` | Done (`forge generate model\|controller\|mailer\|job --queue`, `forge destroy`, [ADR 0041](adr/0041-generators-and-destroy.md)) | Planned ([forge-rust#15](https://github.com/rocketinsidecorp/forge-rust/issues/15)) |
| Continuous fuzzing, SBOM, signed images | — | Planned | Planned |
| Published benchmarks | — | Planned | Planned |
| Getting Started guide | Rails Guides | Planned | Planned |

## Review gate

Every pull request passes the Claude Code Review before it merges to `main`,
including a screen for malicious or exfiltrating code: unexpected network
calls, access to credentials, wallets or crypto keys, obfuscated code, build
scripts and new dependencies.
