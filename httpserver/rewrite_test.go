package httpserver_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/httpserver"
)

// trimSegment is a rewriting middleware: its handler removes a leading
// segment, as the i18n path prefix does. (Unlike i18n, it would remove a
// repeated segment again; the tests do not repeat one.)
type trimSegment struct {
	segment string
	next    http.Handler
}

func (s trimSegment) RewriteRequest(r *http.Request) *http.Request {
	if rest, ok := strings.CutPrefix(r.URL.Path, s.segment); ok && strings.HasPrefix(rest, "/") {
		clone := r.Clone(r.Context())
		clone.URL.Path = rest
		return clone
	}
	return r
}

func (s trimSegment) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.next.ServeHTTP(w, s.RewriteRequest(r))
}

func trim(segment string) httpserver.Middleware {
	return func(next http.Handler) http.Handler { return trimSegment{segment: segment, next: next} }
}

func TestRewritesRunBeforeEveryMiddlewareWhereverTheyAreRegistered(t *testing.T) {
	t.Parallel()
	var seen []string
	record := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = append(seen, r.URL.Path)
			next.ServeHTTP(w, r)
		})
	}
	var instrumented string
	server, err := httpserver.NewWithOptions(config.Default(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Path))
	}), slog.New(slog.NewTextHandler(io.Discard, nil)), httpserver.Options{
		Instrument: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				instrumented = r.URL.Path
				next.ServeHTTP(w, r)
			})
		},
		// The record middleware is outermost, the rewriting ones after it.
		// The rewrites run in registration order, then Rewrite: "/b" and
		// "/c" are leading segments only once "/a" is gone.
		Middlewares: []httpserver.Middleware{record, trim("/a"), record, trim("/b")},
		Rewrite: func(r *http.Request) *http.Request {
			return trimSegment{segment: "/c"}.RewriteRequest(r)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/a/b/c/admin", nil))
	if recorder.Body.String() != "/admin" || len(seen) != 2 || seen[0] != "/admin" || seen[1] != "/admin" {
		t.Fatalf("body=%q middleware saw %v", recorder.Body.String(), seen)
	}
	// Instrumentation (request logs, metrics) sees the path as sent.
	if instrumented != "/a/b/c/admin" {
		t.Fatalf("instrumentation saw %q", instrumented)
	}
}
