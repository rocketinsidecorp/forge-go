package router_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/router"
)

type fullResource struct{}

func (fullResource) Index(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("index")) }
func (fullResource) New(w http.ResponseWriter, _ *http.Request)   { _, _ = w.Write([]byte("new")) }
func (fullResource) Create(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("create"))
}
func (fullResource) Show(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("show " + r.PathValue("id")))
}
func (fullResource) Edit(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("edit " + r.PathValue("id")))
}
func (fullResource) Update(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("update " + r.PathValue("id")))
}
func (fullResource) Destroy(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("destroy " + r.PathValue("id")))
}

type readOnlyResource struct{}

func (readOnlyResource) Index(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("list")) }
func (readOnlyResource) Show(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("item " + r.PathValue("project") + "/" + r.PathValue("id")))
}

func serve(routes http.Handler, method, target string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, httptest.NewRequest(method, target, nil))
	return recorder
}

func TestResourcesRegistersConventionalRoutes(t *testing.T) {
	t.Parallel()
	routes := router.New()
	if err := routes.Resources("/posts", fullResource{}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ method, target, want string }{
		{http.MethodGet, "/posts", "index"},
		{http.MethodGet, "/posts/new", "new"},
		{http.MethodPost, "/posts", "create"},
		{http.MethodGet, "/posts/42", "show 42"},
		{http.MethodGet, "/posts/42/edit", "edit 42"},
		{http.MethodPatch, "/posts/42", "update 42"},
		{http.MethodPut, "/posts/42", "update 42"},
		{http.MethodDelete, "/posts/42", "destroy 42"},
	} {
		if recorder := serve(routes, tt.method, tt.target); recorder.Code != http.StatusOK || recorder.Body.String() != tt.want {
			t.Fatalf("%s %s: %d %q", tt.method, tt.target, recorder.Code, recorder.Body.String())
		}
	}
	if len(routes.Routes()) != 8 {
		t.Fatalf("routes = %v", routes.Routes())
	}
}

func TestResourcesRegistersOnlyImplementedActions(t *testing.T) {
	t.Parallel()
	routes := router.New()
	if err := routes.Resources("/projects/{project}/tasks", readOnlyResource{}); err != nil {
		t.Fatal(err)
	}
	if recorder := serve(routes, http.MethodGet, "/projects/p1/tasks/7"); recorder.Body.String() != "item p1/7" {
		t.Fatalf("show: %q", recorder.Body.String())
	}
	if recorder := serve(routes, http.MethodDelete, "/projects/p1/tasks/7"); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unimplemented destroy: %d", recorder.Code)
	}
	if recorder := serve(routes, http.MethodGet, "/projects/p1/tasks/new"); recorder.Body.String() != "item p1/new" {
		t.Fatalf("without New, /new is a show: %q", recorder.Body.String())
	}
	if got := len(routes.Routes()); got != 2 {
		t.Fatalf("routes = %v", routes.Routes())
	}
}

func TestResourcesMiddlewareSeesEachAction(t *testing.T) {
	t.Parallel()
	routes := router.New()
	var wrapped []string
	guard := func(action router.Action, next http.Handler) (http.Handler, error) {
		wrapped = append(wrapped, string(action))
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !action.ReadOnly() && r.Header.Get("X-Write") == "" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		}), nil
	}
	if err := routes.Resources("/posts", fullResource{}, guard); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(wrapped, ","); got != "index,new,create,show,edit,update,update,destroy" {
		t.Fatalf("wrapped = %s", got)
	}
	if recorder := serve(routes, http.MethodDelete, "/posts/1"); recorder.Code != http.StatusForbidden {
		t.Fatalf("guarded destroy: %d", recorder.Code)
	}
	if recorder := serve(routes, http.MethodGet, "/posts/1/edit"); recorder.Code != http.StatusOK {
		t.Fatalf("read-only edit: %d", recorder.Code)
	}
}

func TestResourcesRejectsInvalidRegistrations(t *testing.T) {
	t.Parallel()
	failing := func(router.Action, http.Handler) (http.Handler, error) { return nil, errors.New("no permission") }
	nilHandler := func(router.Action, http.Handler) (http.Handler, error) { return nil, nil }
	for _, tt := range []struct {
		name       string
		path       string
		handler    any
		middleware []router.ResourceMiddleware
	}{
		{name: "root", path: "/", handler: fullResource{}},
		{name: "trailing slash", path: "/posts/", handler: fullResource{}},
		{name: "relative", path: "posts", handler: fullResource{}},
		{name: "traversal", path: "/a/../posts", handler: fullResource{}},
		{name: "id wildcard", path: "/posts/{id}/comments", handler: fullResource{}},
		{name: "nil handler", path: "/posts", handler: nil},
		{name: "no actions", path: "/posts", handler: struct{}{}},
		{name: "failing middleware", path: "/posts", handler: fullResource{}, middleware: []router.ResourceMiddleware{failing}},
		{name: "nil from middleware", path: "/posts", handler: fullResource{}, middleware: []router.ResourceMiddleware{nilHandler}},
		{name: "nil middleware", path: "/posts", handler: fullResource{}, middleware: []router.ResourceMiddleware{nil}},
	} {
		routes := router.New()
		if err := routes.Resources(tt.path, tt.handler, tt.middleware...); err == nil {
			t.Fatalf("%s: Resources succeeded", tt.name)
		}
		if len(routes.Routes()) != 0 {
			t.Fatalf("%s: registered %v", tt.name, routes.Routes())
		}
	}
	routes := router.New()
	if err := routes.HandleFunc("GET /posts", func(http.ResponseWriter, *http.Request) {}); err != nil {
		t.Fatal(err)
	}
	if err := routes.Resources("/posts", fullResource{}); err == nil {
		t.Fatal("conflicting resource registered")
	}
}

func TestResourceRoutesOrder(t *testing.T) {
	t.Parallel()
	routes, err := router.ResourceRoutes("/posts")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, route := range routes {
		lines = append(lines, route.Method+" "+route.Pattern+" "+string(route.Action))
	}
	want := "GET /posts index|GET /posts/new new|POST /posts create|GET /posts/{id} show|GET /posts/{id}/edit edit|PATCH /posts/{id} update|PUT /posts/{id} update|DELETE /posts/{id} destroy"
	if got := strings.Join(lines, "|"); got != want {
		t.Fatalf("routes = %s", got)
	}
}

func TestRegistrationsRecordTheActionAndTheRegisteringCode(t *testing.T) {
	t.Parallel()
	r := router.New()
	if err := r.Resources("/posts", fullResource{}); err != nil {
		t.Fatal(err)
	}
	if err := r.HandleFunc("POST /webhooks", func(http.ResponseWriter, *http.Request) {}); err != nil {
		t.Fatal(err)
	}
	registrations := r.Registrations()
	if len(registrations) != 9 {
		t.Fatalf("registrations = %v", registrations)
	}
	actions := map[string]router.Action{}
	for _, registration := range registrations {
		// The nearest caller outside the framework is this test file; the
		// framework's own frames (router.Resources, router.Handle) are skipped.
		if !strings.Contains(registration.Source, "resources_test.go:") {
			t.Errorf("%s %s registered at %q", registration.Method, registration.Pattern, registration.Source)
		}
		actions[registration.Method+" "+registration.Pattern] = registration.Action
	}
	for pattern, want := range map[string]router.Action{
		"GET /posts": router.ActionIndex, "POST /posts": router.ActionCreate, "PUT /posts/{id}": router.ActionUpdate,
		"DELETE /posts/{id}": router.ActionDestroy, "POST /webhooks": "",
	} {
		if got, ok := actions[pattern]; !ok || got != want {
			t.Errorf("%s: action %q (registered %v), want %q", pattern, got, ok, want)
		}
	}
}
