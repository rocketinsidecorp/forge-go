// Package devloop is the engine behind `forge dev`: a polling file watcher, a
// supervisor that rebuilds and gracefully restarts the application, and a
// development-only reverse proxy that reloads the browser.
//
// Nothing here is compiled into an application binary. The proxy and its
// script live in the forge CLI process and exist only while `forge dev`
// runs with FORGE_ENV=development (ADR 0035).
package devloop
