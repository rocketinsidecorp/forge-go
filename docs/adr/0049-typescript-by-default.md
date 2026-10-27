# ADR 0049: TypeScript front end by default, compiled by forge with esbuild

- **Status:** Accepted
- **Date:** 2026-10-27
- **Amends:** [ADR 0029](0029-hotwire-assets-and-live-updates.md) (the
  generated `app/javascript` is TypeScript; the asset pipeline, import map and
  Content-Security-Policy are unchanged)
- **Owner decision:** D32, in the owner's words: "o front deve ter typescript
  por default pois o turbo do rails nao tem" (the front end should be
  TypeScript by default, because Rails' Turbo setup doesn't have it).
- **Parity with:** Forge-Rust, planned after forge-go is complete (decision
  D37); porting notes below.

## Context

ADR 0029 gave generated applications a Rails-style front end: Turbo and
Stimulus served through an import map, digested and embedded in the binary, no
Node toolchain. The application's own JavaScript (`application.js`, Stimulus
controllers) was plain JavaScript, which is what `rails new` gives too. The
owner wants more: types by default, since the Rails Turbo setup has none. The
constraint that stays is ADR 0029's: no mandatory Node toolchain, and no
compiler in a production binary.

## Decision

### What a generated application gets

`forge new APP` generates TypeScript:

- `app/javascript/application.ts` and `controllers/{application,index,hello_controller}.ts`
  (typed Stimulus: `declare readonly nameTarget: HTMLInputElement`);
- `tsconfig.json`: `strict`, `noEmit`, `target`/`module` ES2022,
  `moduleResolution: "bundler"`, `verbatimModuleSyntax`, `isolatedModules`,
  `types: []` (no ambient `@types`), and `paths` mapping the import map's bare
  names (`@hotwired/turbo`, `@hotwired/stimulus`, `controllers`,
  `controllers/*`) to the declarations and the sources;
- `app/javascript/types/`: declarations of Turbo and Stimulus, copied
  unchanged. Stimulus: `dist/types` of the `@hotwired/stimulus` 3.2.2 npm
  package, the exact version Forge serves (tarball integrity
  `sha512-eGeIqNOQ...`, the same one `hotwire/vendor/README.md` records).
  Turbo ships no declarations, so Turbo's are `@types/hotwired__turbo` 8.0.16
  from DefinitelyTyped (MIT), the newest release for the 8.0 line that Forge's
  8.0.23 belongs to. Both licences are kept next to the files and
  `types/README.md` records provenance and integrity. A test pins the SHA-256
  of the whole directory;
- `app/javascript/dist/**.js`: the compiled output (below);
- `app/javascript/javascript_test.go`: the freshness test (below).

`forge new APP --js` generates exactly what it generated before: JavaScript
sources, no `tsconfig.json`, no `types/`, no `dist/`, no extra test or CI step
(`--js` is refused with `--api`, which has no front end). Generated files for
`--js` are byte for byte what the previous release wrote, apart from the
`--js` flag in the README's Quickstart line (checked by generating with the
previous and the new `forge` and diffing the trees, for SQLite, PostgreSQL and
MySQL; tests assert the `--js` file set and contents).

### Where the compilation happens

`forge` compiles; the application does not. Three options were weighed.

1. **A framework asset handler compiles `.ts` at startup** (`go:embed` the
   sources, call esbuild from the asset package). Rejected: it puts a
   compiler, and its dependency, in every production binary and every
   application's module graph, and does CPU work at boot. It would also
   break ADR 0006, which keeps core packages free of third-party modules.
2. **`go generate`, `dist` git-ignored.** Rejected: a fresh clone (CI,
   `docker build`, `go test ./...`) would fail to compile with
   `pattern dist/application.js: no matching files` until something ran the
   generator; the generated Dockerfile and CI run plain `go build` and
   `go test`.
3. **`forge` compiles into `app/javascript/dist`, which is committed and
   embedded.** Chosen. The `javascript` package embeds `dist/` and exposes it
   rooted there, so the digest pipeline, `PinAllFrom` and the import map see
   the same logical names as before (`application.js`,
   `controllers/hello_controller.js`). The production binary serves
   precompiled, digested JavaScript; nothing compiles at runtime.

`forge dev`, `forge build`, `forge test`, `forge check`, the commands that run
the application (`forge migrate`, `forge setup`, `forge seed`,
`forge dev --no-reload`) and `forge deploy` (which builds the image from the
working tree, unless `--skip-build` or `--dry-run`) compile before building
(`frontend.Sync`: only files whose content changed are written, files whose
source is gone are removed).

**Staleness is a test failure, not a silent bug.** Every compiled file starts
with `// Compiled from NAME.ts by forge (esbuild X); do not edit.
source-sha256: <hash>`. The generated `app/javascript/javascript_test.go`
hashes each `.ts` source with the standard library and compares: a changed
source with an old `dist`, a missing output and an orphan output all fail
`go test`, so CI catches a forgotten `forge build` without needing esbuild.

**`forge dev` does not rewrite the committed output.** It compiles with inline
source maps into a temporary directory and passes the files to `go build
-overlay`, which makes them appear at `app/javascript/dist` for that build
only (new files and deletions included; `TestGoBuildEmbedsTheOverlay` pins the
`go:embed` behaviour). The working tree keeps the production form, so a
development session never produces a diff and source maps never reach a
commit. A TypeScript syntax error is a build failure shown over the page like
a Go compile error.

### The compiler: esbuild's Go API, `Transform` only

`internal/frontend` calls `api.Transform` once per file: loader `ts`, format
ESM, target ES2022, `verbatimModuleSyntax` and `useDefineForClassFields` given
as `TsconfigRaw` (so output does not depend on a `tsconfig.json` on disk and
matches what `tsc` checks). Types are stripped; imports are kept exactly as
written, because the browser resolves them through the import map. There is no
bundling, no `node_modules` access, no resolution, no minification, no plugin
and no `Build`/`Serve` call. `tsc` is not shipped and esbuild does not
type-check.

### Type checking

`forge check` compiles, runs `go build ./...` and `go vet ./...`, then
`tsc --noEmit --project tsconfig.json` when `tsc` is on `PATH`. When it is not,
it prints one note (`npm install --global typescript@6.0.3`, or a one-off
`npx`) and still succeeds, because the application compiles and runs without
it. A `tsc` failure fails the command (exit 1).

Both CI files run it: the generated application's `ci.yml` (TypeScript
applications only) and this repository's `generated` job, whose tests
(`TestGeneratedTypeScriptTypeChecksAndCatchesTypeErrors`) type-check a
generated application and prove that a type error, an unknown member and a
missing Stimulus export fail. `FORGE_TEST_REQUIRE_TSC=true` makes a missing
`tsc` a failure there instead of a skip.

### Architecture

esbuild is tooling. `architecture_test.go` computes which packages import it,
directly or through others, and requires exactly `cmd/forge`, `internal/cli`,
`internal/frontend` and `internal/scaffold` (`TestEsbuildIsLinkedOnlyByTheForgeCommandLine`),
so it never reaches a request-handling package, a core package
(`TestCorePackagesDependOnlyOnStandardLibraryAndCore` still applies), `forge-proxy`
or an application. Generated applications import only the framework packages
they did before; esbuild appears in their `go.sum` (it is in Forge's module
graph) but not in their `go.mod`, `vendor/` or binary.

## Why esbuild

- It is a Go library with a documented `Transform` API: no subprocess, no Node,
  no download at run time, no cgo.
- One module, MIT licence (Evan Wallace), a single `go.mod` edge to
  `golang.org/x/sys` which Forge already uses at v0.49.0. Net change to the
  module graph: one module (`github.com/evanw/esbuild`) and one `go.mod`-only
  version node of `golang.org/x/sys` (a 2022 pseudo-version, never selected).
  The `forge` binary links 25 esbuild packages. The linked closure imports
  `net/http` and `net` for esbuild's dev server (never called here), `syscall`
  and `golang.org/x/sys/unix`; it imports no `os/exec`, `plugin` or cgo, and
  `unsafe` only in a Windows-only file (the console width).
- It is what the Rails world already uses for the same job (jsbundling-rails),
  fast (milliseconds per file), and stable at the API we use.
- Pinned exactly: `v0.28.2` (released 2026-08-08) in `go.mod` and `go.sum`;
  `frontend.EsbuildVersion` repeats it and a test compares the two. `go mod
  verify` covers its checksum.

## Alternatives

- **Ship `tsc` or Node with Forge, or require them for `forge dev`.** Rejected:
  it breaks "no mandatory Node toolchain" (ADR 0029) and is a far larger
  supply chain (a package manager, a registry, hundreds of files) on every
  developer machine and in every build. `tsc` stays optional and only checks.
- **Plain JavaScript only (status quo), with JSDoc types.** Rejected by the
  owner's decision; `--js` keeps it available.
- **Another Go type-stripper.** There is no maintained pure-Go TypeScript
  transformer comparable to esbuild.
- **Bundling with esbuild.** Not needed: the import map replaces a bundler, and
  bundling would bake Turbo and Stimulus into the app's file and defeat their
  separate digests and caching.
- **Compile in the framework's asset handler or in a `go generate` step.**
  Considered above.
- **tsc 7 (the native compiler) for the CI pin.** 7.0.x installs a
  platform-specific binary package beside `typescript`; 6.0.3 is one
  pure-JavaScript package with no dependencies. The tsconfig and tests work
  with both.

## Supply chain

- New Go dependency: `github.com/evanw/esbuild v0.28.2`, MIT, one module, no
  cgo, no install or generate scripts that `go` runs, no network access in the
  `Transform` path. `go mod graph` before and after differs by two lines:
  `forge-go -> esbuild@v0.28.2` and `esbuild@v0.28.2 ->
  golang.org/x/sys@v0.0.0-20220715151400-c0bba94af5f8`.
- New CI dependency: `typescript@6.0.3` (Apache-2.0), installed with `npm
  install --global --ignore-scripts typescript@6.0.3`, in this repository's
  `generated` job and in the generated CI. No lockfile; the registry checks the
  tarball integrity and the version is exact.
  `TestTypeScriptCheckerIsTheOnlyNpmPackageAndIsPinned` fails if a workflow runs
  any other Node command or another version, or if the two installs diverge from
  `frontend.TypeScriptVersion`. `FORGE_TEST_REQUIRE_TSC` joins the closed
  environment allowlist of the supply-chain gate.
- Vendored declarations come from the two npm tarballs named above and are
  checksummed as a tree.
- **Update policy.** Updates are reviewed changes (docs/SUPPLY_CHAIN.md,
  "TypeScript: esbuild and tsc"): choose a release at least two weeks old, read
  its changelog, `go get`, `go mod tidy`, `go mod verify`, re-run the import
  review (no new `os/exec`, `plugin`, `unsafe` or network use in what `pkg/api`
  links), update `frontend.EsbuildVersion` and this ADR's version. Dependabot's
  Go module pull requests are reviewed the same way. `tsc` moves with
  `frontend.TypeScriptVersion`, the workflow and the template together.
  Declarations change only with the vendored Turbo or Stimulus
  (`hotwire/vendor/README.md` procedure).

## Consequences

- Applications are typed from the first controller; editors get completion for
  Turbo and Stimulus with no setup beyond `tsc` or the editor's TypeScript.
- Developers commit `dist/` with their sources. Forgetting fails `go test`
  with the instruction to run `forge build`. A `forge dev` session never
  changes it.
- The `forge` binary is about 5.6 MB larger (26.7 MB to 32.2 MB on linux/amd64)
  and links `net/http` code of esbuild it does not use; applications are
  unaffected.
- `tsc` is optional locally; type errors are caught by CI and by `forge check`
  where it is installed. esbuild accepts code `tsc` rejects, by design.
- TypeScript features that need type information to compile (`const enum`
  across files, `isolatedModules` violations) are rejected by the strict
  tsconfig, not by esbuild.
- Turbo's declarations track 8.0.x, not each patch release.

## Porting notes for Forge-Rust

Planned after forge-go is complete (decision D37); same commands, names and
files, with [oxc](https://oxc.rs) replacing esbuild:

- `forge new APP` generates the same TypeScript sources, `tsconfig.json`,
  `types/` (the same files, so the pinned tree hash is the same) and
  `javascript_test`-equivalent; `--js` keeps JavaScript and conflicts with
  `--api`.
- Compile with `oxc_parser` (`SourceType::ts()` as a module), build scoping
  with `oxc_semantic`, strip with `oxc_transformer` (TypeScript options with
  `only_remove_type_imports` to match `verbatimModuleSyntax`; target ES2022 and
  define-semantics class fields), print with `oxc_codegen` and, for development,
  an inline `//# sourceMappingURL=data:application/json;base64,` from the
  generated source map (`oxc_sourcemap`). Check the exact API of the pinned
  release; pin every oxc crate with `=` in `Cargo.toml`, and put them in the
  `forge` CLI crate only, never in `forge-views` or an application's graph (add
  the Rust twin of `TestEsbuildIsLinkedOnlyByTheForgeCommandLine` with
  `cargo tree -i`).
- Keep the contract of the output, not its bytes: imports untouched, ESM,
  ES2022, no bundling. The first line of each compiled file is
  `// Compiled from NAME.ts by forge (TOOL X); do not edit. source-sha256: <hex>`,
  where the generated freshness test only parses the name and the hash, so
  Forge-Rust writes `oxc` and its own version for TOOL.
- The freshness test uses only `sha2` and the standard library, not oxc.
- Development without touching the committed output depends on how Forge-Rust
  embeds assets: if debug builds read from disk, write the development output
  to a gitignored directory and point the embed at it; if they embed at
  compile time, use the build-script or `OUT_DIR` equivalent of `go build
  -overlay`.
- `forge check` runs `tsc --noEmit` when `tsc` is on `PATH`, one note when not;
  the generated CI installs the same pinned `typescript@6.0.3` with
  `--ignore-scripts`.
