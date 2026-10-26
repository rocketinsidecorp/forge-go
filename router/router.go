// Package router provides Forge's route registry on top of net/http semantics.
package router

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"runtime"
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
}

// New returns an empty router.
func New() *Router {
	return &Router{mux: http.NewServeMux(), handlers: map[Route]http.Handler{}, registrations: map[Route]Registration{}}
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
	r.mux.Handle(pattern, handler)
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
		if frame.Function != "" && !frameworkFunction(frame.Function) {
			return fmt.Sprintf("%s:%d", frame.File, frame.Line)
		}
		if !more {
			return ""
		}
	}
}

// frameworkFunction reports whether a fully qualified function name belongs
// to the framework's own packages or to the standard library and the runtime
// (sync.Once, runtime.goexit, testing.tRunner): neither is the application
// code that registered a route. Examples and tests of the framework are
// application code.
func frameworkFunction(function string) bool {
	if standardLibraryFunction(function) {
		return true
	}
	rest, ok := strings.CutPrefix(function, frameworkPackage)
	if !ok || rest == "" || (rest[0] != '/' && rest[0] != '.') {
		return false
	}
	if strings.HasPrefix(rest, "/examples/") {
		return false
	}
	if i := strings.Index(rest, "."); i >= 0 && strings.HasSuffix(rest[:i], "_test") {
		return false
	}
	return true
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

func (r *Router) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if !r.frozen.Load() {
		r.frozen.Store(true)
	}

	path := strings.ToLower(request.URL.EscapedPath())
	if strings.Contains(path, "%2f") || strings.Contains(path, "%5c") ||
		strings.Contains(path, "\\") || hasTraversalSegment(request.URL.Path) {
		web.Error(w, request, fault.New("invalid_path", "invalid request path", fault.CategoryInvalid, http.StatusBadRequest))
		return
	}
	handler, pattern := r.mux.Handler(request)
	if pattern == "" {
		r.unmatched(w, request, handler)
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

func hasTraversalSegment(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}
