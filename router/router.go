// Package router provides Forge's route registry on top of net/http semantics.
package router

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/web"
)

// ErrFrozen is returned when a route is registered after the router served its
// first request.
var ErrFrozen = errors.New("router is frozen")

// Route identifies a registered route by HTTP method and path pattern.
type Route struct {
	Method  string
	Pattern string
}

// Registration is a registered route together with how it got there, which
// `forge routes` prints.
type Registration struct {
	Route
	// Action is the resource action for a route that Resources registered,
	// and empty for a route registered with Handle.
	Action Action
	// Source is the "file.go:line" of the application code that registered
	// the route. It is empty for routes the framework registers itself.
	Source string
}

// Router is the route registry. Routes are registered before the first
// request; after that the router is frozen.
type Router struct {
	mu            sync.RWMutex
	mux           *http.ServeMux
	routes        []Route
	handlers      map[Route]http.Handler
	registrations map[Route]Registration
	frozen        atomic.Bool

	// probe holds the same patterns as mux with handlers that do nothing. It
	// only answers what a request that matched no route is (404 or 405 with
	// its Allow header), so mux itself can carry a catch-all and match each
	// request once; see ServeHTTP.
	probe *http.ServeMux
	seal  sync.Once
}

// New returns an empty router.
func New() *Router {
	return &Router{
		mux: http.NewServeMux(), probe: http.NewServeMux(),
		handlers: map[Route]http.Handler{}, registrations: map[Route]Registration{},
	}
}

// Handle registers handler for pattern, a net/http.ServeMux pattern such as
// "GET /posts/{id}". It returns an error for an invalid or conflicting
// pattern, a nil handler, or when the router is already frozen.
func (r *Router) Handle(pattern string, handler http.Handler) error {
	return r.handle(pattern, handler, "")
}

func (r *Router) handle(pattern string, handler http.Handler, action Action) (err error) {
	if handler == nil {
		return errors.New("route handler is required")
	}
	method, path, err := parsePattern(pattern)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen.Load() {
		return ErrFrozen
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("invalid or conflicting route %q: %v", pattern, recovered)
		}
	}()
	r.mux.Handle(pattern, &routeHandler{pattern: pattern, next: handler})
	r.probe.Handle(pattern, http.NotFoundHandler())
	route := Route{Method: method, Pattern: path}
	r.routes = append(r.routes, route)
	r.handlers[route] = handler
	r.registrations[route] = Registration{Route: route, Action: action, Source: registrationSource()}
	return nil
}

// frameworkPackage is the import path prefix of the Forge module, which
// registrationSource skips to find the application's registering code.
var frameworkPackage = strings.TrimSuffix(reflect.TypeOf(Router{}).PkgPath(), "/router")

// registrationSource returns the file and line of the nearest caller outside
// the framework and the standard library, or "" when only they registered the
// route.
func registrationSource() string {
	var pcs [16]uintptr
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs[:])])
	for {
		frame, more := frames.Next()
		if frame.Function != "" && !skippedFrame(frame.Function, mainModulePath()) {
			return fmt.Sprintf("%s:%d", frame.File, frame.Line)
		}
		if !more {
			return ""
		}
	}
}

// mainModulePath is the module path of the running binary's main module, or
// "" when the build information is unavailable.
var mainModulePath = sync.OnceValue(func() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Main.Path
	}
	return ""
})

// skippedFrame reports whether a fully qualified function name belongs to
// the framework's own packages or to the standard library and the runtime
// (sync.Once, runtime.goexit, testing.tRunner): neither is the application
// code that registered a route. Examples and tests of the framework are
// application code, and so is every package of the main module, whatever its
// path: a module created with `forge new myapp` is simply "myapp".
func skippedFrame(function, mainModule string) bool {
	if rest, ok := strings.CutPrefix(function, frameworkPackage); ok && rest != "" && (rest[0] == '/' || rest[0] == '.') {
		if strings.HasPrefix(rest, "/examples/") {
			return false
		}
		if i := strings.Index(rest, "."); i >= 0 && strings.HasSuffix(rest[:i], "_test") {
			return false
		}
		return true
	}
	if mainModule != "" && (strings.HasPrefix(function, mainModule+"/") || strings.HasPrefix(function, mainModule+".")) {
		return false
	}
	return standardLibraryFunction(function)
}

// standardLibraryFunction reports whether function is in a package whose
// import path has no dot in its first element, which Go reserves for the
// standard library and the runtime. Package main is the application.
func standardLibraryFunction(function string) bool {
	first := function
	if i := strings.IndexByte(first, '/'); i >= 0 {
		first = first[:i]
	} else if i := strings.IndexByte(first, '.'); i >= 0 {
		first = first[:i]
	}
	return first != "main" && !strings.Contains(first, ".")
}

// Registrations returns every registered route with its resource action and
// the source location that registered it, sorted like Routes.
func (r *Router) Registrations() []Registration {
	routes := r.Routes()
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Registration, 0, len(routes))
	for _, route := range routes {
		result = append(result, r.registrations[route])
	}
	return result
}

// Lookup returns the handler registered for route exactly as it was given to
// Handle, so callers can read what it carries, such as an OpenAPI
// description (package openapi).
func (r *Router) Lookup(route Route) (http.Handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	handler, ok := r.handlers[route]
	return handler, ok
}

// HandleFunc is Handle for a handler function.
func (r *Router) HandleFunc(pattern string, handler http.HandlerFunc) error {
	return r.Handle(pattern, handler)
}

// Routes returns the registered routes sorted by path, then method.
func (r *Router) Routes() []Route {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := slices.Clone(r.routes)
	slices.SortFunc(result, func(a, b Route) int {
		if compared := strings.Compare(a.Pattern, b.Pattern); compared != 0 {
			return compared
		}
		return strings.Compare(a.Method, b.Method)
	})
	return result
}

// routeHandler records the pattern that matched, for telemetry and the access
// log (web.RecordRoute), before it runs the route's handler. It is what the
// mux holds, so a matched request is matched exactly once, by the mux itself.
type routeHandler struct {
	pattern string
	next    http.Handler
}

func (h *routeHandler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	web.RecordRoute(request.Context(), h.pattern)
	h.next.ServeHTTP(w, request)
}

// catchAll is the pattern that catches what no route accepts. Every route has
// a method ("GET /x"), so the method-less "/" never conflicts with one.
const catchAll = "/"

// freeze closes registration and gives mux its catch-all. It runs once, with
// the first request.
func (r *Router) freeze() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mux.Handle(catchAll, http.HandlerFunc(r.fallback))
	// Last: a request that sees the router frozen finds the catch-all in place.
	r.frozen.Store(true)
}

// fallback answers a request that reached the mux's catch-all, that is, one no
// route accepts: the probe says whether it is a 404 or a 405.
func (r *Router) fallback(w http.ResponseWriter, request *http.Request) {
	handler, pattern := r.probe.Handler(request)
	if pattern == "" {
		r.unmatched(w, request, handler)
		return
	}
	handler.ServeHTTP(w, request)
}

// ServeHTTP matches the request once. Routes are registered in the mux behind
// routeHandler, and a catch-all behind them takes what no route accepts, so
// the mux's own match both finds the route and sets its path values; matching
// first with mux.Handler to learn the pattern and again in mux.ServeHTTP for
// the path values cost two matches per request.
//
// Requests the mux treats specially (CONNECT, "*", a path that is not clean)
// keep the two-step flow, where the verdict comes before anything is written.
func (r *Router) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if !r.frozen.Load() { // the check keeps the hot path from allocating the method value
		r.seal.Do(r.freeze)
	}

	path := strings.ToLower(request.URL.EscapedPath())
	if strings.Contains(path, "%2f") || strings.Contains(path, "%5c") ||
		strings.Contains(path, "\\") || hasTraversalSegment(request.URL.Path) {
		web.Error(w, request, fault.New("invalid_path", "invalid request path", fault.CategoryInvalid, http.StatusBadRequest))
		return
	}
	if !plainRequest(request) {
		r.serveTwoStep(w, request)
		return
	}
	r.mux.ServeHTTP(w, request)
	// A matched route recorded itself (routeHandler) before it ran, and that
	// label must stand: a handler such as http.StripPrefix(inner router) has
	// recorded a more precise one since. A redirect for a missing trailing
	// slash is answered by the mux itself, in place of a route handler, and
	// still names the pattern it redirects to.
	if redirected(request) {
		web.RecordRoute(request.Context(), request.Pattern)
	}
}

// redirected reports whether the mux answered the request, which it has just
// served, with its redirect to the path plus a trailing slash, rather than
// with a route's handler. That happens when request.Pattern ("GET /tree/",
// "GET /files/{path...}", "GET /a/{$}") matches the path only with a slash
// added: the path has no trailing slash and one segment fewer than the
// pattern's, where a request such a pattern serves is as deep as the pattern
// or deeper, and any other pattern serves exactly its own depth. It reads two
// strings and takes no lock.
func redirected(request *http.Request) bool {
	_, pattern, found := strings.Cut(request.Pattern, " ")
	if !found {
		return false
	}
	path := request.URL.EscapedPath()
	return !strings.HasSuffix(path, "/") && strings.Count(path, "/")+1 == strings.Count(pattern, "/")
}

// plainRequest reports whether the mux handles the request without rewriting
// it: not CONNECT or "*", and a path ServeMux would not redirect to its clean
// form (only an empty segment is left; "." and ".." were refused before).
func plainRequest(request *http.Request) bool {
	if request.Method == http.MethodConnect || request.RequestURI == "*" {
		return false
	}
	path := request.URL.EscapedPath()
	return strings.HasPrefix(path, "/") && !strings.Contains(path, "//")
}

// serveTwoStep matches with mux.Handler to learn whether a route accepts the
// request, then lets mux.ServeHTTP run it with its path values.
func (r *Router) serveTwoStep(w http.ResponseWriter, request *http.Request) {
	_, pattern := r.mux.Handler(request)
	if pattern == "" || pattern == catchAll {
		r.fallback(w, request)
		return
	}
	web.RecordRoute(request.Context(), pattern)
	// ServeHTTP (not the handler above) so the request gets its path values.
	r.mux.ServeHTTP(w, request)
}

// unmatched answers requests no route accepts in the standard error format.
// ServeMux decides between 404 and 405 (and computes Allow) internally, so its
// fallback handler runs against a recorder and only its verdict is kept.
func (r *Router) unmatched(w http.ResponseWriter, request *http.Request, fallback http.Handler) {
	recorder := &statusRecorder{header: make(http.Header)}
	fallback.ServeHTTP(recorder, request)
	if recorder.status == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", recorder.header.Get("Allow"))
		web.Error(w, request, fault.New("method_not_allowed", "method not allowed", fault.CategoryInvalid, http.StatusMethodNotAllowed))
		return
	}
	web.Error(w, request, fault.New("route_not_found", "route not found", fault.CategoryNotFound, http.StatusNotFound))
}

// statusRecorder keeps the status and headers of a response and drops its body.
type statusRecorder struct {
	header http.Header
	status int
}

func (s *statusRecorder) Header() http.Header { return s.header }

func (s *statusRecorder) Write(data []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return len(data), nil
}

func (s *statusRecorder) WriteHeader(status int) {
	if s.status == 0 {
		s.status = status
	}
}

func parsePattern(pattern string) (string, string, error) {
	parts := strings.Fields(pattern)
	if len(parts) != 2 {
		return "", "", errors.New("route pattern must be METHOD /path")
	}
	method, path := parts[0], parts[1]
	if method != strings.ToUpper(method) || method == "" {
		return "", "", errors.New("route method must be uppercase")
	}
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "\\") || hasTraversalSegment(path) {
		return "", "", errors.New("route path must be absolute and may not traverse")
	}
	return method, path, nil
}

// hasTraversalSegment reports whether a "." or ".." segment is in path. It
// walks the path without splitting it, so a request does not allocate here.
func hasTraversalSegment(path string) bool {
	for {
		segment, rest, more := strings.Cut(path, "/")
		if segment == "." || segment == ".." {
			return true
		}
		if !more {
			return false
		}
		path = rest
	}
}
