# Type declarations for Turbo and Stimulus

`tsc` (see `tsconfig.json`, `forge check`) checks `app/javascript/**/*.ts`
against these declarations. They describe the two modules the import map
serves; the JavaScript itself is vendored by Forge (`hotwire/vendor` in the
framework) and never read from here. They are copied unchanged, so do not
edit them.

| | Stimulus | Turbo |
| --- | --- | --- |
| Served module | `@hotwired/stimulus` 3.2.2 | `@hotwired/turbo` 8.0.23 |
| Declarations from | `dist/types` of the `@hotwired/stimulus` 3.2.2 npm package, the exact version Forge serves (the `tests/` directory left out) | `@types/hotwired__turbo` 8.0.16 from DefinitelyTyped: Turbo ships no declarations of its own, and this is the newest release for the 8.0 line |
| Source | `https://registry.npmjs.org/@hotwired/stimulus/-/stimulus-3.2.2.tgz` | `https://registry.npmjs.org/@types/hotwired__turbo/-/hotwired__turbo-8.0.16.tgz` |
| npm integrity (tarball) | `sha512-eGeIqNOQpXoPAIP7tC1+1Yc1yl1xnwYqg+3mzqxyrbE5pg5YFBZcA6YoTiByJB6DKAEsiWtl6tjTJS4IYtbB7A==` | `sha512-FOYSD+lq8reFMIiZAp8O5ooKf5Lv9hdrEi0OPKfAgBp4Y7KpNfCbPzyalWbnyR05FepztJatz4PHK7nbsGi+vQ==` |
| Licence | MIT, Basecamp (`@hotwired/stimulus/LICENSE.md`) | MIT, Microsoft Corporation (`@hotwired/turbo/LICENSE`) |

Turbo's declarations track the 8.0 line, not each patch release, so a newer
Turbo API can be missing from them: declare what you need in your own `.d.ts`
file next to your code. Updating the declarations is a change to Forge (its
`internal/scaffold/templates/typescript/types` directory and the test that
pins them), made together with the vendored JavaScript.
