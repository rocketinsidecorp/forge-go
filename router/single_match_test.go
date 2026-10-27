package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/rocketinsidecorp/forge-go/web"
)

// referenceServe is how ServeHTTP answered before it matched once: mux.Handler
// to learn the pattern (and whether any route accepts the request), then
// mux.ServeHTTP to run it with its path values, on a mux that holds the routes
// and nothing else.
func referenceServe(r *Router, mux *http.ServeMux, w http.ResponseWriter, request *http.Request) {
	handler, pattern := mux.Handler(request)
	if pattern == "" {
		r.unmatched(w, request, handler)
		return
	}
	web.RecordRoute(request.Context(), pattern)
	mux.ServeHTTP(w, request)
}

// outcome is everything a client or the telemetry middleware can observe of
// one request.
type outcome struct {
	status       int
	header, body string
	route        string
}

func serveObserved(t *testing.T, serve func(http.ResponseWriter, *http.Request), method, target, requestURI string) outcome {
	t.Helper()
	request := httptest.NewRequest(method, "/", nil)
	if requestURI != "" {
		request.RequestURI = requestURI
	} else {
		target, err := url.ParseRequestURI(target)
		if err != nil {
			t.Fatal(err)
		}
		request.URL = target
	}
	info := &web.RouteInfo{}
	request = request.WithContext(web.WithRouteInfo(request.Context(), info))
	recorder := httptest.NewRecorder()
	serve(recorder, request)
	return outcome{
		status: recorder.Code,
		header: fmt.Sprint("Allow=", recorder.Header().Get("Allow"), " Location=", recorder.Header().Get("Location"), " Content-Type=", recorder.Header().Get("Content-Type")),
		body:   recorder.Body.String(),
		route:  info.Pattern(),
	}
}

// echo answers with what a handler can read of the request.
func echo(w http.ResponseWriter, request *http.Request) {
	_, _ = fmt.Fprintf(w, "%s|%s|%s|%s|%s", request.Pattern, request.URL.Path, request.PathValue("id"), request.PathValue("path"), request.PathValue("x")+request.PathValue("y")+request.PathValue("b"))
}

// twoStep is a router that serves as ServeHTTP did before it matched once.
type twoStep struct {
	router *Router
	mux    *http.ServeMux
}

func newTwoStep(t *testing.T, patterns []string) (*Router, twoStep) {
	t.Helper()
	routes := New()
	reference := http.NewServeMux()
	for _, pattern := range patterns {
		if err := routes.Handle(pattern, http.HandlerFunc(echo)); err != nil {
			t.Fatal(err)
		}
		reference.Handle(pattern, http.HandlerFunc(echo))
	}
	return routes, twoStep{routes, reference}
}

func (o twoStep) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	referenceServe(o.router, o.mux, w, request)
}

// compareFlows serves every method against every target, and "*", with both.
func compareFlows(t *testing.T, got, want http.Handler, targets []string) {
	t.Helper()
	methods := []string{"GET", "HEAD", "POST", "DELETE", "PUT", "OPTIONS", "CONNECT", "PATCH", "TRACE", "get"}
	for _, method := range methods {
		for _, target := range targets {
			wantOutcome := serveObserved(t, want.ServeHTTP, method, target, "")
			if gotOutcome := serveObserved(t, got.ServeHTTP, method, target, ""); gotOutcome != wantOutcome {
				t.Errorf("%s %s:\n got %+v\nwant %+v", method, target, gotOutcome, wantOutcome)
			}
		}
	}
	wantOutcome := serveObserved(t, want.ServeHTTP, "OPTIONS", "", "*")
	if gotOutcome := serveObserved(t, got.ServeHTTP, "OPTIONS", "", "*"); gotOutcome != wantOutcome {
		t.Errorf("OPTIONS *:\n got %+v\nwant %+v", gotOutcome, wantOutcome)
	}
}

// TestSingleMatchAnswersLikeTheTwoStepFlow compares the router, which matches
// each request once, with the two-step flow it replaced, for matched routes,
// HEAD, OPTIONS, TRACE, a lowercase method, 404, 405 and its Allow header,
// trailing-slash redirects (with and without a query) and clean-path
// redirects, CONNECT and "*": status, headers, body, path values and the
// route recorded for telemetry.
func TestSingleMatchAnswersLikeTheTwoStepFlow(t *testing.T) {
	t.Parallel()
	targets := []string{
		"/", "/items", "/items/", "/items/7", "/items/new", "/items/a%20b", "/items/%41", "/items/7/x", "/tree", "/tree/",
		"/tree?x=1", "/tree/?x=1", "/tree/a/b", "/files", "/files?q=a%20b", "/files/", "/files/a/b/c", "/files/a%20b/c",
		"/missing", "/a/1/b/2", "/a/1/b", "/a/1", "/a/1/", "//items/7", "/items//7", "/files//a", "//", "/tree//",
		"/éclair", "/items/7%2e", "/h", "/o", "/m", "/m/", "/posts", "/posts/", "/posts/1", "/posts/1/comments", "/b", "/b/", "/b/x/y",
		"/deep/one", "/deep/one/", "/deep/one/two", "/docs", "/docs/",
	}
	t.Run("routes with methods only", func(t *testing.T) {
		t.Parallel()
		routes, reference := newTwoStep(t, []string{
			"GET /items/{id}", "GET /items/new", "POST /items", "GET /tree/", "GET /files/{path...}", "GET /{$}",
			"DELETE /items/{id}", "GET /a/{x}/b/{y}", "HEAD /h", "OPTIONS /o",
			"GET /m", "POST /m", "PUT /m", "DELETE /m", "PATCH /m", "GET /b/{b...}", "GET /deep/one/{$}", "GET /docs/{$}",
		})
		compareFlows(t, routes, reference, targets)
	})
	t.Run("with a subtree at the root next to the catch-all", func(t *testing.T) {
		t.Parallel()
		routes, reference := newTwoStep(t, []string{"GET /", "GET /posts/", "POST /posts/{id}", "GET /items/{id}", "GET /tree/", "HEAD /h"})
		compareFlows(t, routes, reference, targets)
	})
	t.Run("no routes", func(t *testing.T) {
		t.Parallel()
		routes, reference := newTwoStep(t, nil)
		compareFlows(t, routes, reference, targets)
	})
}

// TestNestedRouterKeepsTheInnerRoute: an outer subtree route that strips its
// prefix and hands the request to an inner router is labeled with the inner
// route, as it was when the router recorded the route only before the
// handler. The request StripPrefix hands on is a clone with the same context.
func TestNestedRouterKeepsTheInnerRoute(t *testing.T) {
	t.Parallel()
	build := func(wrap func(*Router, *http.ServeMux) http.Handler) http.Handler {
		inner, innerReference := newTwoStep(t, []string{"GET /users/{id}", "GET /dir/", "POST /users"})
		outer := New()
		outerReference := http.NewServeMux()
		if err := outer.Handle("GET /api/", http.StripPrefix("/api", inner)); err != nil {
			t.Fatal(err)
		}
		outerReference.Handle("GET /api/", http.StripPrefix("/api", innerReference))
		return wrap(outer, outerReference)
	}
	got := build(func(outer *Router, _ *http.ServeMux) http.Handler { return outer })
	want := build(func(outer *Router, mux *http.ServeMux) http.Handler { return twoStep{outer, mux} })
	targets := []string{"/api/users/1", "/api/users", "/api/dir", "/api/dir/", "/api/dir/x", "/api/missing", "/api", "/api/", "/other"}
	compareFlows(t, got, want, targets)
	if label := serveObserved(t, got.ServeHTTP, "GET", "/api/users/1", "").route; label != "GET /users/{id}" {
		t.Errorf("GET /api/users/1 is labeled %q, want the inner route", label)
	}
}

// TestMatchedRequestMatchesOnce is the allocation evidence that a request is
// matched once. A bare ServeMux allocates once to match a route with path
// values and not at all for a static route; the flow this replaced (Handler,
// then ServeHTTP) allocated twice as often, and Split in the traversal check
// added one more.
func TestMatchedRequestMatchesOnce(t *testing.T) {
	routes := New()
	for _, pattern := range []string{"GET /items/{id}", "GET /status"} {
		if err := routes.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {}); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		path string
		want float64
	}{{"/items/42", 1}, {"/status", 0}} {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		recorder := httptest.NewRecorder()
		routes.ServeHTTP(recorder, request)
		if got := testing.AllocsPerRun(100, func() { routes.ServeHTTP(recorder, request) }); got > test.want {
			t.Errorf("GET %s allocated %v times, want at most %v: it is matched more than once", test.path, got, test.want)
		}
	}
}

// TestRouteIsRecordedBeforeTheHandlerRuns: with http.TimeoutHandler the
// middleware may read the route while the handler is still running.
func TestRouteIsRecordedBeforeTheHandlerRuns(t *testing.T) {
	t.Parallel()
	routes := New()
	var during string
	if err := routes.HandleFunc("GET /items/{id}", func(_ http.ResponseWriter, request *http.Request) {
		during = web.RouteInfoFromContext(request.Context()).Pattern()
	}); err != nil {
		t.Fatal(err)
	}
	info := &web.RouteInfo{}
	request := httptest.NewRequest(http.MethodGet, "/items/1", nil)
	routes.ServeHTTP(httptest.NewRecorder(), request.WithContext(web.WithRouteInfo(request.Context(), info)))
	if during != "GET /items/{id}" {
		t.Errorf("route during the handler = %q", during)
	}
}
