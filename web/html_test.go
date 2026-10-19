package web_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/web"
)

func TestRedirectAcceptsLocalPaths(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"/", "/posts", "/posts/0190?tab=edit#form", "/a/b?next=%2F%2Fevil.example", "/%2F%2Fevil.example"} {
		recorder := httptest.NewRecorder()
		if err := web.Redirect(recorder, target); err != nil {
			t.Fatalf("%q: %v", target, err)
		}
		if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != target {
			t.Fatalf("%q: status=%d location=%q", target, recorder.Code, recorder.Header().Get("Location"))
		}
	}
}

func TestRedirectRejectsOpenRedirects(t *testing.T) {
	t.Parallel()
	for _, target := range []string{
		"", "posts", "https://evil.example/", "//evil.example", "///evil.example", "/\\evil.example", "\\\\evil.example",
		"/\\/evil.example", "/a\\b", "/\t/evil.example", "/\n/evil.example", "/ /evil.example", "/\x00", "/\x7f",
		"javascript:alert(1)", "/café", "∕∕evil.example", "/" + strings.Repeat("a", 2048),
	} {
		recorder := httptest.NewRecorder()
		if err := web.Redirect(recorder, target); !errors.Is(err, web.ErrUnsafeRedirect) {
			t.Fatalf("%q: error = %v", target, err)
		}
		if recorder.Header().Get("Location") != "" || recorder.Body.Len() != 0 || recorder.Code != http.StatusOK {
			t.Fatalf("%q: response written: %d %v", target, recorder.Code, recorder.Header())
		}
		if web.IsLocalPath(target) {
			t.Fatalf("%q reported local", target)
		}
	}
}

func TestNegotiate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		accept string
		html   bool
		want   string
	}{
		{accept: "", html: false, want: "application/json"},
		{accept: "*/*", html: false, want: "application/json"},
		{accept: "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", html: true, want: "text/html"},
		{accept: "application/json", html: false, want: "application/json"},
		{accept: "text/*", html: true, want: "text/html"},
		{accept: "text/html;q=0.5, application/json;q=0.9", html: false, want: "application/json"},
		{accept: "text/html;q=0, */*", html: false, want: "application/json"},
		{accept: "TEXT/HTML", html: true, want: "text/html"},
		{accept: "image/png", html: false, want: ""},
		{accept: "text/html;q=bogus, application/json;q=0.1", html: false, want: "application/json"},
	}
	for _, tt := range tests {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		if tt.accept != "" {
			request.Header.Set("Accept", tt.accept)
		}
		if got := web.Negotiate(request, "application/json", "text/html"); got != tt.want {
			t.Errorf("Accept %q: Negotiate = %q, want %q", tt.accept, got, tt.want)
		}
		if got := web.PrefersHTML(request); got != tt.html {
			t.Errorf("Accept %q: PrefersHTML = %v, want %v", tt.accept, got, tt.html)
		}
	}
}

func TestMethodOverride(t *testing.T) {
	t.Parallel()
	var seen string
	handler := web.MethodOverride(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Method + " " + r.PostForm.Get("title")
		w.WriteHeader(http.StatusNoContent)
	}))
	tests := []struct {
		name        string
		method      string
		target      string
		contentType string
		body        string
		status      int
		want        string
	}{
		{name: "patch", method: http.MethodPost, target: "/posts/1", contentType: "application/x-www-form-urlencoded", body: "_method=patch&title=x", status: 204, want: "PATCH x"},
		{name: "delete", method: http.MethodPost, target: "/posts/1", contentType: "application/x-www-form-urlencoded; charset=utf-8", body: "_method=DELETE", status: 204, want: "DELETE "},
		{name: "put", method: http.MethodPost, target: "/posts/1", contentType: "application/x-www-form-urlencoded", body: "_method=Put", status: 204, want: "PUT "},
		{name: "plain post", method: http.MethodPost, target: "/posts", contentType: "application/x-www-form-urlencoded", body: "title=y", status: 204, want: "POST y"},
		{name: "query ignored", method: http.MethodPost, target: "/posts/1?_method=DELETE", contentType: "application/x-www-form-urlencoded", body: "title=z", status: 204, want: "POST z"},
		{name: "never on get", method: http.MethodGet, target: "/posts/1?_method=DELETE", status: 204, want: "GET "},
		{name: "json untouched", method: http.MethodPost, target: "/posts/1", contentType: "application/json", body: `{"_method":"DELETE"}`, status: 204, want: "POST "},
		{name: "to get rejected", method: http.MethodPost, target: "/posts/1", contentType: "application/x-www-form-urlencoded", body: "_method=GET", status: 400},
		{name: "to connect rejected", method: http.MethodPost, target: "/posts/1", contentType: "application/x-www-form-urlencoded", body: "_method=CONNECT", status: 400},
		{name: "repeated rejected", method: http.MethodPost, target: "/posts/1", contentType: "application/x-www-form-urlencoded", body: "_method=PATCH&_method=DELETE", status: 400},
		{name: "oversized rejected", method: http.MethodPost, target: "/posts/1", contentType: "application/x-www-form-urlencoded", body: "_method=DELETE&title=" + strings.Repeat("a", web.MaxFormBytes), status: 413},
	}
	for _, tt := range tests {
		seen = ""
		request := httptest.NewRequest(tt.method, tt.target, strings.NewReader(tt.body))
		if tt.contentType != "" {
			request.Header.Set("Content-Type", tt.contentType)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != tt.status {
			t.Fatalf("%s: status=%d body=%s", tt.name, recorder.Code, recorder.Body.String())
		}
		if tt.status == 204 && seen != tt.want {
			t.Fatalf("%s: handler saw %q, want %q", tt.name, seen, tt.want)
		}
		if tt.status != 204 {
			if seen != "" {
				t.Fatalf("%s: handler ran", tt.name)
			}
			var body web.ErrorBody
			if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil || body.Error.Code == "" {
				t.Fatalf("%s: error body %v %+v", tt.name, err, body)
			}
		}
	}
}
