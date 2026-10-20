package csrf_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/rocketinsidecorp/forge-go/csrf"
)

type benchWriter struct{ header http.Header }

func (w benchWriter) Header() http.Header          { return w.header }
func (benchWriter) Write(data []byte) (int, error) { return len(data), nil }
func (benchWriter) WriteHeader(int)                {}

// BenchmarkCSRFVerify is scenario "csrf_verify" in docs/BENCHMARKS.md.
//
//	match        csrf.Match of a cookie token and a submitted token
//	header_post  a POST through Protection.Handler with the token in the
//	             X-CSRF-Token header
//	form_post    the same with the token in the authenticity_token form field
//	             (the body is parsed on every iteration)
func BenchmarkCSRFVerify(b *testing.B) {
	token, err := csrf.NewToken()
	if err != nil {
		b.Fatal(err)
	}
	b.Run("match", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if !csrf.Match(token, token) {
				b.Fatal("token did not match")
			}
		}
	})
	protection, err := csrf.New(csrf.Options{})
	if err != nil {
		b.Fatal(err)
	}
	handler := protection.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !csrf.Verified(r) {
			http.Error(w, "unverified", http.StatusForbidden)
		}
	}))
	w := benchWriter{header: http.Header{}}
	b.Run("header_post", func(b *testing.B) {
		request := httptest.NewRequest(http.MethodPost, "/posts", nil)
		request.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: token})
		request.Header.Set(csrf.HeaderName, token)
		b.ReportAllocs()
		for b.Loop() {
			handler.ServeHTTP(w, request)
		}
	})
	b.Run("form_post", func(b *testing.B) {
		form := []byte(url.Values{csrf.FormField: {token}, "title": {"Hello Forge"}, "body": {"My first post"}}.Encode())
		request := httptest.NewRequest(http.MethodPost, "/posts", nil)
		request.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: token})
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		b.ReportAllocs()
		for b.Loop() {
			request.Body = io.NopCloser(bytes.NewReader(form))
			request.PostForm, request.Form = nil, nil
			handler.ServeHTTP(w, request)
		}
	})
}
