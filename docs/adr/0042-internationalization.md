# ADR 0042: Internationalization (i18n)

- **Status:** Accepted
- **Date:** 2026-10-20

This decision is language-neutral: Forge-Go and Forge-Rust read the same
translation files with the same keys, plural rules, locale negotiation,
escaping and missing-translation behavior, and ship the same commands (see
[Forge-Rust port](#forge-rust-port)). The guide is [docs/I18N.md](../I18N.md).

## Context

Every user-visible string Forge generated (the welcome page, the sign-in,
two-factor and password-reset pages, flash messages, scaffold pages) was
English in source code. The parity matrix ([Rails parity](../RAILS_PARITY.md))
listed Rails I18n as missing. Rails keeps text in `config/locales/*.yml`,
looks it up with `t("users.welcome", name: ...)`, picks plural forms by
`count`, marks keys ending in `_html` as trusted markup and formats dates and
numbers from the same files.

Constraints: the core is standard-library only (ADR 0006,
`architecture_test.go`), so the core cannot parse YAML (`go.yaml.in/yaml/v3`
is only linked into `forge deploy`); Forge-Rust must read the same files
without new dependencies (it already has `serde_json`); translations reach
HTML, so escaping must be decided once, by the framework.

## Decision

1. **JSON files in `config/locales`, Rails layout.** A file is
   `LOCALE.json` or `SCOPE.LOCALE.json` (`en.json`, `posts.pt-BR.json`); it
   holds one object whose only key is the locale, like a Rails YAML file
   (`{"en": {"users": {"welcome": "Welcome, %{name}!"}}}`), so a rails-i18n
   file converts with `yq -o json`. The files of a locale are merged; a key
   defined twice (in one object or across files) is a load error, never
   "last wins". Keys are dotted paths of segments `[A-Za-z0-9_-]+`. Values
   are strings, objects, plural objects, and for formatting data numbers,
   booleans and lists of strings or `null` (`date.month_names` starts with
   `null`, as in Rails). Subdirectories are not read; applications embed the
   flat directory (`//go:embed *.json` in the generated `config/locales`
   package). Limits: 1 MiB per file, 512 files, 16 levels, 50,000 keys per
   locale, 256-byte keys.
2. **Locale names are a validated BCP 47 subset.** `language[-Script][-REGION]`
   with a 2-3 letter language, 4-letter script and 2-letter or 3-digit region,
   canonicalized (`pt-br` → `pt-BR`). Nothing else (no `_`, extensions,
   private use, dots or slashes) is a locale, so a name from a header, cookie,
   query or path segment can never reach the filesystem, and files must be
   named canonically (`pt-br.json` is refused with the right name). The
   available locales are the files present; the default locale must have one.
3. **Interpolation and escaping.** `%{name}` interpolates named arguments,
   `%%` is a literal `%`; a placeholder without a value stays visible and is
   logged. The views' `t` helper returns plain text, which `html/template`
   escapes for its context (element, attribute, URL, script). Keys whose last
   segment is `html` or ends in `_html` are trusted markup (Rails): the text
   comes from the application's own files and is not escaped, while every
   interpolated value is HTML-escaped unless it is `template.HTML`.
   Translation files are therefore code: they are embedded at build time and
   never loaded from user input. A missing `_html` key is never markup.
4. **CLDR plural rules, built in, data-free.** With a `count` argument an
   object of plural forms (`zero`, `one`, `two`, `few`, `many`, `other`;
   `other` required) selects the form by the CLDR 46 cardinal rule of the
   locale where the text was found, using the operands n, i and v (fractions
   choose as CLDR says: `1.5` is `one` in French). An explicit `zero` form is
   used for 0 in every language (Rails). A form the rule selects but the file
   lacks falls back to `other`; without a count a plural key gives `other`. Built-in rules: en, de, nl, sv, nb, fi, et;
   es, fr, it, pt (Brazilian: 0 and 1 singular), pt-PT; ru, uk, pl, cs, sk;
   ar (all six forms), he; tr, el, hu; ja, zh, ko, vi, th, id, ms (only
   `other`). Rules are looked up by locale then parents (`pt-BR` → `pt`);
   unknown languages use `other`. Applications add or replace rules with
   `i18n.Options.PluralRules`.
5. **Locale negotiation, in order:** the first path segment when the
   application opts in (`PathPrefix`; it must name an available locale and is
   removed before routing), a query parameter when it opts in (`Param`), the
   `locale` cookie (`SetCookie`: Secure, HttpOnly, SameSite=Lax, one year),
   `Accept-Language`, then the default. A candidate matches its own locale,
   else its closest parent (`pt-PT` → `pt`), else a variant of its language
   (the default locale's first, then the first in sorted order: `pt` →
   `pt-BR`). `Accept-Language` follows RFC 9110/4647: q-values in
   thousandths, `q=0` excluded, malformed entries skipped, stable order on
   ties, unsupported subtags truncated, `*` meaning the default; headers over
   4096 bytes are ignored and only 32 entries are read. The middleware stores
   the translator and locale in the request context and sets no headers, so
   language-independent responses (digested assets, JSON) stay cacheable;
   translated responses get `Content-Language` and `Vary: Accept-Language,
   Cookie` from `i18n.SetHeaders`, which the views renderer calls for every
   page.
6. **Missing translations never panic.** Lookup walks the locale and its
   parents. In development and test (`ShowMissing`) a missing key renders
   `translation missing: LOCALE.KEY`; in staging and production it falls back
   to the default locale (and its parents), then to the key's last segment
   humanized (`welcome_back` → "Welcome back"). Both log one warning per
   locale and key, for at most 1,024 distinct keys, so keys built from input
   cannot flood logs or memory; the first warning past the bound logs one
   "missing-translation warnings suppressed" line and the rest are counted
   (`SuppressedWarnings`). `FORGE_I18N_DEFAULT_LOCALE` (validated)
   overrides the application's default (`forge new --locale`, else `en`).
7. **Formatting is data.** `number.format.{separator,delimiter,precision}`,
   `date.formats.*`, `time.formats.*`, `date.{day,abbr_day,month,abbr_month}_names`
   and `time.{am,pm}` come from the locale files (Rails keys), falling back
   through the parents, the default locale, then built-in English. Times use
   a strftime subset (`%Y %y %m %d %e %j %H %I %M %S %p %P %A %a %B %b %z %Z
   %%`, `-` removes padding); numbers group thousands, and floats round to
   the precision (exact binary ties to even).
8. **API surface.** Go: `i18n.Load(fs, Options)`, `Translator.Middleware`,
   `i18n.T(ctx, key, "name", value, ...)`, `i18n.HTML`, `i18n.Number`,
   `i18n.Localize`, `i18n.NewContext` (jobs, mailers), `i18n.SetHeaders`. Views: `t`, `l`,
   `number`, `locale`; they read the translator and locale from the request
   and use `views.Options.I18n` (default locale) for fragments rendered
   without a request, such as broadcasts. Without either, `t` fails the
   render with a message naming the fix. Argument shape mistakes in templates
   (`t "k" "name"`) stop `views.Load`.
9. **Generators.** `forge new [--locale en|es|pt-BR]` writes
   `config/locales/locales.go` (embed, `DefaultLocale`, `New(cfg, logger)`),
   `en.json` and the chosen locale's file, plus `authentication.LOCALE.json`
   with Forge identity; builds the translator in `Configure`; installs the
   middleware first in `useBrowserPipeline`; and translates the layout
   (`<html lang>`, skip link), the welcome page and the sign-in, two-factor,
   password-reset pages and messages (throttling messages are plural by
   minutes). `forge generate scaffold` writes `config/locales/PLURAL.en.json`
   (and `PLURAL.LOCALE.json` for an application whose default locale is not
   `en`, see the follow-ups) and uses `t` and `i18n.T`; it refuses the plurals
   `date`, `time` and `number`, which would merge into the formatting data;
   `forge destroy scaffold` removes the files when unchanged. `generate view`, `controller`, `scaffold` and `authentication`
   add `config/locales` to an application that lacks it and warn when
   `bootstrap.go` does not install the middleware. The printed renderer is
   `views.New(views.WithI18n(translator))`.
10. **`forge i18n check [--json]`.** Read-only: compares every locale with
    the union of all locales' keys and the literal keys referenced by `t
    "KEY"` in `.gohtml` files and `i18n.T|HTML(ctx, "KEY")` /
    `.Translate|HTML(locale, "KEY")` in non-test Go files (skipping
    `vendor`, `node_modules`, `tmp`, `bin`, `storage` and dot directories),
    reports plural forms a locale's rule needs for 0-1000, and lists keys
    nothing references (informational; `date.`, `time.` and `number.` data is
    never "unused"). It exits 1 when a locale lacks a key or a plural form.

## Consequences

- Generated applications are translatable from the first commit and the
  Spanish and Brazilian Portuguese pages ship complete; generated tests fail
  on any `translation missing` in the pages they render.
- Applications that predate this release keep working; their `forge
  generate` runs add `config/locales` and print the two wiring lines.
- JSON has no comments; translator notes go in a `README.md` next to the
  files (`Load` reads only `*.json`). The format stays readable and
  diffable.
- Plural rules are code, not CLDR data: adding a language is a small
  function, with no data file to ship or update.

## Review follow-ups (forge-go#21)

- **Scaffold text in the default locale.** An application created with
  `--locale pt-BR` has `en.json` and `pt-BR.json` but a scaffold only wrote
  `PLURAL.en.json`, so production (which falls back to the default locale,
  then humanizes the key) showed "Back to posts"-style text only for English
  visitors and humanized keys for everyone else. Chosen: the scaffold also
  writes `PLURAL.LOCALE.json` with the English text when the default locale
  is not `en`, the default being the `DefaultLocale` constant of the
  application's `config/locales/locales.go` (what `forge new --locale`
  writes; `en` when absent or unreadable; the `FORGE_I18N_DEFAULT_LOCALE`
  deployment override is not known at generation time). Rejected: making the
  translator fall back to `en` after the default locale. It would change the
  language-neutral lookup rule Forge-Rust must copy, add an implicit second
  default behind the one the application chose, and keep `forge i18n check`
  reporting a gap that is visible only in the file listing; a file the
  developer sees, edits and translates leaves the fallback chain unchanged
  and `forge i18n check` clean. `forge destroy` removes the
  file when it is unchanged and keeps it (reporting `keep`) once it has been
  translated, since translating it is what the scaffold asks for; it is planned from the same
  default locale, so an application whose default changed since generation
  keeps the old file (delete it by hand).
- **Reserved plurals.** `date`, `time` and `number` are formatting scopes
  (`date.formats`, `time.am`, `number.format`): a scaffold with such a plural
  would merge its text into the data `l` and `number` read, and `forge i18n
  check` never reports that scope. The generator refuses them with an error
  naming the scopes.
- **Path prefix.** Middleware passed to `WithHTTPMiddleware` sees the
  prefixed path, so path-sensitive security middleware (allow/deny by path,
  admin guards) must be added with `App.Use` after the i18n middleware, or
  attached to the routes it protects; the stripped path is what routing and
  `ServeMux` redirects see ([I18N.md](../I18N.md#choosing-the-locale)). The
  middleware keeps the removed segment in the request context
  (`i18n.PathPrefix`) and the auth return-to path puts it back, still behind
  `web.IsLocalPath`. A prefix whose escaped form differs from its decoded
  form (`/pt-BR%2F..`) clears `RawPath` so it stays consistent with `Path`.
- **`_html` and URLs.** `href="%{url}"` escapes but does not filter schemes
  (same as Rails); documented in the guide and the threat model.

## Follow-ups

- `forge generate controller` and `forge generate mailer` still emit English
  strings (their stubs are placeholders meant to be replaced); mail
  templates need `NewContext` with the recipient's locale and a `t` in
  `text/template` (with a separate escaping rule for plain text).
- Framework validation messages (`web.FormErrors`, domain errors such as
  "is required") are English; a Rails-style `errors.messages` scope is
  planned.
- Scaffold `show` pages display booleans as Yes/No and times as RFC 3339
  from Go; switching them to `t` and `l` needs the request locale in the
  item builder.
- A `localePath` helper for `PathPrefix` applications.
- Broadcast fragments render in the default locale for every subscriber
  (Rails renders them in `I18n.default_locale` too); per-locale streams are
  not planned.

## Forge-Rust port

Tracked in [forge-rust#22](https://github.com/rocketinsidecorp/forge-rust/issues/22). Same file layout and format (JSON,
one locale key, `SCOPE.LOCALE.json`, duplicate keys refused, same limits),
same key and locale grammar and canonicalization, same `%{name}`/`%%`
interpolation, same `_html` rule and escaping, the same plural rules and
categories table (port `i18n/plural.go` and its sample tests one to one), the
same negotiation order, Accept-Language parsing limits and matching, the same
`locale` cookie attributes, `Content-Language` and `Vary` headers, the same
missing-translation text (`translation missing: LOCALE.KEY`), fallback and
log-once rule, the same formatting keys and strftime subset, the same
`FORGE_I18N_DEFAULT_LOCALE` variable and validation, the same generated files
(`config/locales/en.json`, `authentication.LOCALE.json`,
`PLURAL.en.json`) with the same keys and texts (copy the JSON files), and the
same commands: `forge new --locale en|es|pt-BR` and `forge i18n check
[--json]` with the same report and exit status. Rust keeps its own idioms: a
`t!`-style macro or function taking `&RequestContext`, compile-time checked
templates calling the same lookup, and `serde_json` with a duplicate-key
visitor (serde's default keeps the last value, which this ADR forbids).
