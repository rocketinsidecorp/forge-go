// The entry point of the application's front end, loaded by the layout with
// {{javascriptImportmapTags "application"}}. It is TypeScript: `forge dev`,
// `forge build` and `forge check` strip the types with esbuild into
// app/javascript/dist, which is what the binary embeds and serves. There is
// no bundler and no Node toolchain: the browser resolves these bare specifiers
// through the import map the layout renders (see app/assets/assets.go), which
// maps each one to a digested file URL.
//
// Turbo Drive turns link clicks and form submissions into fetch requests;
// Turbo Streams update the page from form responses and from real-time
// broadcasts (<turbo-stream-source>).
import "@hotwired/turbo"
import "controllers"
