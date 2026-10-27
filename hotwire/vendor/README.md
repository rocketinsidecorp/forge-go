# Vendored Turbo and Stimulus

Unmodified ES module builds of [Turbo](https://turbo.hotwired.dev/) (37signals,
MIT, `TURBO-LICENSE`) and [Stimulus](https://stimulus.hotwired.dev/) (Basecamp,
MIT, `STIMULUS-LICENSE`), served to applications through an import map
(ADR 0029). No Node toolchain is needed. These are the same bytes the sibling
Rust framework vendors, so both frameworks serve identical assets.

| | Turbo | Stimulus |
| --- | --- | --- |
| Package | `@hotwired/turbo` 8.0.23 | `@hotwired/stimulus` 3.2.2 |
| Source | `https://registry.npmjs.org/@hotwired/turbo/-/turbo-8.0.23.tgz` | `https://registry.npmjs.org/@hotwired/stimulus/-/stimulus-3.2.2.tgz` |
| npm integrity (tarball) | `sha512-GZ7cijxEZ6Ig71u7rD6LHaRv/wcE/hNsc+nEfiWOkLNqUgLOwo5MNGWOy5ZV9ZUDSiQx1no7YxjTNnT4O6//cQ==` | `sha512-eGeIqNOQpXoPAIP7tC1+1Yc1yl1xnwYqg+3mzqxyrbE5pg5YFBZcA6YoTiByJB6DKAEsiWtl6tjTJS4IYtbB7A==` |
| File | `package/dist/turbo.es2017-esm.js` (203,701 bytes, no imports) | `package/dist/stimulus.js` (88,680 bytes, no imports) |
| SHA-256 | `b9d35d123a07614f55eaaf993f74d687a503ae41ba50ef835aafa18dbb265a13` | `23deecdac6f36c08e8f39fbed6b27f60c850cacad3a34eb71194462b7b1647f3` |
| Licence text | `MIT-LICENSE` at tag `v8.0.23` of github.com/hotwired/turbo (the npm tarball omits it) | the package's `LICENSE.md` |

The npm integrity values were verified against the registry before vendoring
(by the sibling Rust project, which owns the provenance record); the file
SHA-256 values above are checked by `hotwire_test.go` on every test run, and
match `crates/forge-views/vendor` of the Rust framework byte for byte. The
upstream 3.2.2 tarball ships a file whose banner still reads "Stimulus 3.2.1".

## Updating

1. Download the new tarball from the npm registry and verify its
   `dist.integrity`.
2. Replace `turbo.js` / `stimulus.js` without edits and refresh the licence from
   the matching upstream tag.
3. Update this table and the constants in `hotwire.go` (`*Version`, `*SHA256`).
4. Run `go test ./hotwire`. If Turbo's progress bar CSS changed, the test fails
   until `TurboProgressBarStyleSHA256` (the CSP style hash) is updated too.
5. Update the type declarations generated applications get
   (`internal/scaffold/templates/typescript/types`, ADR 0049): Stimulus's from
   the same npm tarball (`dist/types`), Turbo's from the newest
   `@types/hotwired__turbo` for the same minor line; refresh
   `types/README.md` and the `typesSHA256` constant of
   `internal/scaffold/typescript_test.go`, which fails until both match.
