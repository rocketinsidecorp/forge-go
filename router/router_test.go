package router_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rocketinsidecorp/forge-go/router"
	"github.com/rocketinsidecorp/forge-go/web"
)

func TestRouterHandleAndFreeze(t *testing.T) {
	t.Parallel()
	routes := router.New()
	if err := routes.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.PathValue("id")))
	}); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/users/42", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "42" {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if err := routes.HandleFunc("GET /late", func(http.ResponseWriter, *http.Request) {}); !errors.Is(err, router.ErrFrozen) {
		t.Fatalf("late registration error=%v", err)
	}
}

func TestRouterRejectsConflictAndTraversal(t *testing.T) {
	t.Parallel()
	routes := router.New()
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if err := routes.Handle("GET /items", handler); err != nil {
		t.Fatal(err)
	}
	if err := routes.Handle("GET /items", handler); err == nil {
		t.Fatal("expected duplicate conflict")
	}
	if err := routes.Handle("GET /../secret", handler); err == nil {
		t.Fatal("expected traversal rejection")
	}
}

func TestRouterRejectsEncodedSeparator(t *testing.T) {
	t.Parallel()
	routes := router.New()
	request := httptest.NewRequest(http.MethodGet, "/a%2fb", nil)
	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", recorder.Code)
	}
}

func TestUnmatchedRequestsUseStandardErrorFormat(t *testing.T) {
	t.Parallel()
	r := router.New()
	if err := r.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, req *http.Request) { _, _ = w.Write([]byte(req.PathValue("id"))) }); err != nil {
		t.Fatal(err)
	}
	if err := r.HandleFunc("GET /tree/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		method, path string
		wantStatus   int
		wantCode     string
		wantAllow    string
	}{
		{http.MethodGet, "/missing", http.StatusNotFound, "route_not_found", ""},
		{http.MethodDelete, "/items/7", http.StatusMethodNotAllowed, "method_not_allowed", "GET, HEAD"},
		{http.MethodGet, "/items/7", http.StatusOK, "", ""},
		{http.MethodGet, "/tree", 0, "", ""}, // ServeMux redirect to /tree/ still applies
	}
	for _, test := range tests {
		recorder := httptest.NewRecorder()
		r.ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, nil))
		if test.wantStatus == 0 {
			// The redirect status is ServeMux's choice (301 before Go 1.27, 307 since).
			if recorder.Code/100 != 3 || recorder.Header().Get("Location") != "/tree/" {
				t.Fatalf("%s %s: status=%d Location=%q, want a redirect to /tree/", test.method, test.path, recorder.Code, recorder.Header().Get("Location"))
			}
			continue
		}
		if recorder.Code != test.wantStatus {
			t.Fatalf("%s %s: status=%d want=%d", test.method, test.path, recorder.Code, test.wantStatus)
		}
		if test.wantCode != "" {
			var body web.ErrorBody
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Error.Code != test.wantCode {
				t.Fatalf("%s %s: body=%s err=%v", test.method, test.path, recorder.Body.String(), err)
			}
		}
		if recorder.Header().Get("Allow") != test.wantAllow {
			t.Fatalf("%s %s: Allow=%q want %q", test.method, test.path, recorder.Header().Get("Allow"), test.wantAllow)
		}
	}
	recorder := httptest.NewRecorder()
	r.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/items/42", nil))
	if recorder.Body.String() != "42" {
		t.Fatalf("path values lost: %q", recorder.Body.String())
	}
}
