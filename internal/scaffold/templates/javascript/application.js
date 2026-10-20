// The entry point of the application's JavaScript, loaded by the layout with
// {{javascriptImportmapTags "application"}}. There is no bundler: the browser
// resolves these bare specifiers through the import map the layout renders
// (see app/assets/assets.go), which maps each one to a digested file URL.
//
// Turbo Drive turns link clicks and form submissions into fetch requests;
// Turbo Streams update the page from form responses and from real-time
// broadcasts (<turbo-stream-source>).
import "@hotwired/turbo"
import "controllers"
