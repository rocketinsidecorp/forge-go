// Package router provides Forge's route registry on top of net/http semantics.
package router

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/fabriciobonjorno/forge-go/fault"
	"github.com/fabriciobonjorno/forge-go/web"
)

var ErrFrozen = errors.New("router is frozen")

type Route struct {
	Method  string
	Pattern string
}

type Router struct {
	mu     sync.RWMutex
	mux    *http.ServeMux
	routes []Route
	frozen atomic.Bool
}

func New() *Router { return &Router{mux: http.NewServeMux()} }

func (r *Router) Handle(pattern string, handler http.Handler) (err error) {
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
	r.routes = append(r.routes, Route{Method: method, Pattern: path})
	return nil
}

func (r *Router) HandleFunc(pattern string, handler http.HandlerFunc) error {
	return r.Handle(pattern, handler)
}

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

// RoutePattern returns the registered ServeMux pattern matching request. It
// freezes route registration just like ServeHTTP, and is intended for
// middleware that needs a bounded route label before dispatch.
func (r *Router) RoutePattern(request *http.Request) string {
	if !r.frozen.Load() {
		r.freeze()
	}
	_, pattern := r.mux.Handler(request)
	return pattern
}

func (r *Router) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if !r.frozen.Load() {
		r.freeze()
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
	// Preserve the ServeMux route template for middleware that observes the
	// request before ServeMux assigns Request.Pattern itself.
	request.Pattern = pattern
	// ServeHTTP (not the handler above) so the request gets its path values.
	r.mux.ServeHTTP(w, request)
}

func (r *Router) freeze() {
	r.mu.Lock()
	r.frozen.Store(true)
	r.mu.Unlock()
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
