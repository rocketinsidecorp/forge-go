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
	status        int
	header, body  string
	route, params string
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

// TestSingleMatchAnswersLikeTheTwoStepFlow compares the router, which matches
// each request once, with the two-step flow it replaced, for matched routes,
// HEAD, 404, 405, trailing-slash and clean-path redirects, CONNECT and "*".
func TestSingleMatchAnswersLikeTheTwoStepFlow(t *testing.T) {
	t.Parallel()
	routes := New()
	reference := http.NewServeMux()
	for _, pattern := range []string{
		"GET /items/{id}", "POST /items", "GET /tree/", "GET /files/{path...}", "GET /{$}",
		"DELETE /items/{id}", "GET /a/{x}/b/{y}",
	} {
		handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			_, _ = fmt.Fprintf(w, "%s|%s|%s|%s", request.Pattern, request.PathValue("id"), request.PathValue("path"), request.PathValue("x")+request.PathValue("y"))
		})
		if err := routes.Handle(pattern, handler); err != nil {
			t.Fatal(err)
		}
		reference.Handle(pattern, handler)
	}
	methods := []string{"GET", "HEAD", "POST", "DELETE", "PUT", "OPTIONS", "CONNECT", "PATCH"}
	paths := []string{
		"/", "/items", "/items/", "/items/7", "/items/a%20b", "/items/%41", "/items/7/x", "/tree", "/tree/",
		"/tree/a/b", "/files", "/files/", "/files/a/b/c", "/files/a%20b/c", "/missing", "/a/1/b/2", "/a/1/b",
		"//items/7", "/items//7", "/files//a", "//", "/tree//", "/éclair", "/items/7%2e",
	}
	for _, method := range methods {
		for _, path := range paths {
			want := serveObserved(t, func(w http.ResponseWriter, request *http.Request) { referenceServe(routes, reference, w, request) }, method, path, "")
			got := serveObserved(t, routes.ServeHTTP, method, path, "")
			if got != want {
				t.Errorf("%s %s:\n got %+v\nwant %+v", method, path, got, want)
			}
		}
	}
	want := serveObserved(t, func(w http.ResponseWriter, request *http.Request) { referenceServe(routes, reference, w, request) }, "OPTIONS", "", "*")
	if got := serveObserved(t, routes.ServeHTTP, "OPTIONS", "", "*"); got != want {
		t.Errorf("OPTIONS *:\n got %+v\nwant %+v", got, want)
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
